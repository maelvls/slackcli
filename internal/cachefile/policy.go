package cachefile

import "time"

// State is what an entry of a cache is good for at a given time.
type State int

const (
	// Discard is an entry dated in the future (the clock moved backwards)
	// or past retention.
	Discard State = iota
	// Stale is an entry past its TTL: shown only when looking it up again
	// failed (a rate limit).
	Stale
	// Fresh is an entry shown without asking Slack.
	Fresh
)

// Entry is an entry of a cache: FetchedAt is when the fetch that got it
// started, in Unix milliseconds.
type Entry interface{ Fetched() int64 }

// Policy is the freshness rule of a cache. It is the only code that
// decides whether an entry is usable and which of two copies wins, for
// every read and every merge of a cache, file and memory alike:
//
//  1. State: Fresh before TTL, Stale until Keep (0: no limit), Discard
//     when dated in the future or past Keep.
//  2. A discarded entry is never served, kept, written or compared: Merge
//     and Prune drop it, wherever it is.
//  3. Of two usable copies, the one fetched last wins; on a tie, the one
//     being merged in.
type Policy struct {
	TTL  time.Duration
	Keep time.Duration
}

// State returns what an entry fetched at fetchedAt (Unix milliseconds) is
// good for at now.
func (p Policy) State(fetchedAt int64, now time.Time) State {
	age := now.Sub(time.UnixMilli(fetchedAt))
	switch {
	case fetchedAt <= 0 || age < 0 || (p.Keep > 0 && age >= p.Keep):
		return Discard
	case age < p.TTL:
		return Fresh
	default:
		return Stale
	}
}

// Merge puts e under id in entries, unless entries hold a usable copy
// fetched later. A discarded e is not put, and a discarded copy in
// entries is dropped.
func Merge[E Entry](p Policy, entries map[string]E, id string, e E, now time.Time) {
	old, ok := entries[id]
	if ok && p.State(old.Fetched(), now) == Discard {
		delete(entries, id)
		ok = false
	}
	if p.State(e.Fetched(), now) == Discard {
		return
	}
	if !ok || old.Fetched() <= e.Fetched() {
		entries[id] = e
	}
}

// Prune removes from entries those not at least least: Stale keeps what
// may still be shown after a rate limit, Fresh only what is served
// without asking Slack.
func Prune[E Entry](p Policy, entries map[string]E, now time.Time, least State) {
	for id, e := range entries {
		if p.State(e.Fetched(), now) < least {
			delete(entries, id)
		}
	}
}
