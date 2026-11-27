package cachefile

import (
	"testing"
	"time"
)

type entry struct {
	at   int64
	name string
}

func (e entry) Fetched() int64 { return e.at }

var (
	now    = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	policy = Policy{TTL: 24 * time.Hour, Keep: 30 * 24 * time.Hour}
)

func ago(d time.Duration) int64 { return now.Add(-d).UnixMilli() }

func TestPolicyState(t *testing.T) {
	tests := []struct {
		name   string
		policy Policy
		at     int64
		want   State
	}{
		{"just fetched", policy, ago(0), Fresh},
		{"before TTL", policy, ago(23 * time.Hour), Fresh},
		{"at TTL", policy, ago(24 * time.Hour), Stale},
		{"before Keep", policy, ago(30*24*time.Hour - time.Minute), Stale},
		{"at Keep", policy, ago(30 * 24 * time.Hour), Discard},
		{"in the future", policy, ago(-time.Minute), Discard},
		{"no date", policy, 0, Discard},
		{"no Keep: stale however old", Policy{TTL: time.Hour}, ago(365 * 24 * time.Hour), Stale},
	}
	for _, tt := range tests {
		if got := tt.policy.State(tt.at, now); got != tt.want {
			t.Errorf("%s: State = %v, want %v", tt.name, got, tt.want)
		}
	}
}

// The invariants of Merge: a discarded copy, on either side, is dropped;
// of two usable copies, the one fetched last wins, and on a tie the one
// merged in.
func TestPolicyMerge(t *testing.T) {
	var (
		fresh   = ago(time.Hour)
		older   = ago(2 * time.Hour)
		stale   = ago(48 * time.Hour)
		future  = ago(-time.Hour)
		expired = ago(31 * 24 * time.Hour)
	)
	tests := []struct {
		name     string
		existing *entry
		incoming entry
		want     string // "" when nothing is left
	}{
		{"into nothing", nil, entry{fresh, "in"}, "in"},
		{"newer wins", &entry{older, "old"}, entry{fresh, "in"}, "in"},
		{"older loses", &entry{fresh, "old"}, entry{older, "in"}, "old"},
		{"tie goes to the incoming", &entry{fresh, "old"}, entry{fresh, "in"}, "in"},
		{"fresh over stale", &entry{stale, "old"}, entry{fresh, "in"}, "in"},
		{"stale under fresh", &entry{fresh, "old"}, entry{stale, "in"}, "old"},
		{"stale into nothing", nil, entry{stale, "in"}, "in"},
		{"future existing is dropped", &entry{future, "old"}, entry{older, "in"}, "in"},
		{"expired existing is dropped", &entry{expired, "old"}, entry{stale, "in"}, "in"},
		{"future incoming is not put", &entry{older, "old"}, entry{future, "in"}, "old"},
		{"expired incoming is not put", nil, entry{expired, "in"}, ""},
		{"both discarded", &entry{future, "old"}, entry{expired, "in"}, ""},
	}
	for _, tt := range tests {
		entries := map[string]entry{}
		if tt.existing != nil {
			entries["id"] = *tt.existing
		}
		Merge(policy, entries, "id", tt.incoming, now)
		if got := entries["id"].name; got != tt.want {
			t.Errorf("%s: got %q, want %q", tt.name, got, tt.want)
		}
	}
}

func TestPolicyPrune(t *testing.T) {
	all := func() map[string]entry {
		return map[string]entry{
			"fresh": {ago(time.Hour), "fresh"}, "stale": {ago(48 * time.Hour), "stale"},
			"future": {ago(-time.Hour), "future"}, "expired": {ago(31 * 24 * time.Hour), "expired"},
		}
	}
	for least, want := range map[State][]string{Stale: {"fresh", "stale"}, Fresh: {"fresh"}} {
		entries := all()
		Prune(policy, entries, now, least)
		if len(entries) != len(want) {
			t.Errorf("Prune(%v) left %v, want %v", least, entries, want)
		}
		for _, id := range want {
			if _, ok := entries[id]; !ok {
				t.Errorf("Prune(%v) removed %s", least, id)
			}
		}
	}
}
