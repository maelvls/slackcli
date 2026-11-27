package unreads

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/maelvls/slackcli/internal/slack/channels"
	"github.com/maelvls/slackcli/internal/slackapi"
)

var t0 = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

func clock(t time.Time) func() time.Time { return func() time.Time { return t } }

// namedWorkspace has a channel and a DM with unreads.
func namedWorkspace() workspace {
	return workspace{
		counts: map[string]any{
			"channels": []any{map[string]any{"id": "C0GENERAL1", "has_unreads": true, "mention_count": 1.0, "last_read": "10.000001"}},
			"ims":      []any{map[string]any{"id": "D0DM000001", "has_unreads": true, "last_read": "10.000001"}},
		},
		names:   map[string]string{"C0GENERAL1": "general"},
		dmUsers: map[string]string{"D0DM000001": "U0ALICE001"},
	}
}

// client.counts gives the read cursors: the conversations.info calls are
// only for the names, and the next run takes them from the cache.
func TestFetchCachesNames(t *testing.T) {
	dir := t.TempDir()
	for _, countsOnly := range []bool{true, false} {
		first := namedWorkspace().recorder()
		opts := Options{CacheDir: dir, CountsOnly: countsOnly, Names: channels.NewNames(channels.NamesOptions{CacheDir: dir, Now: clock(t0)})}
		miss, err := Fetch(context.Background(), first.client(), opts)
		if err != nil {
			t.Fatal(err)
		}
		again := namedWorkspace().recorder()
		opts.Names = channels.NewNames(channels.NamesOptions{CacheDir: dir, Now: clock(t0.Add(time.Hour))})
		hit, err := Fetch(context.Background(), again.client(), opts)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(hit, miss) {
			t.Errorf("counts only %v: from the cache:\n%s\nlooked up:\n%s", countsOnly, dump(hit), dump(miss))
		}
		if miss.Channels[0].ChannelName != "general" || miss.Channels[1].ChannelName != "user-U0ALICE001" {
			t.Errorf("names: %s", dump(miss))
		}
		if n := len(again.only("conversations.info")) + len(again.only("users.info")); n != 0 {
			t.Errorf("counts only %v: %d name lookups on the second run", countsOnly, n)
		}
	}
}

// users.counts has no read cursor: conversations.info is asked for it on
// every run, cache or not, and the name it gives feeds the cache.
func TestFetchViaUsersCountsAlwaysAsksForTheReadCursor(t *testing.T) {
	dir := t.TempDir()
	serve := func(method string, params map[string]any) map[string]any {
		switch method {
		case "client.counts":
			return apiError("team_is_restricted")
		case "users.counts":
			return map[string]any{"channels": []any{map[string]any{"id": "C0BUSY0001", "unread_count_display": 2.0}}}
		case "conversations.info":
			return map[string]any{"channel": map[string]any{"id": "C0BUSY0001", "name": "busy", "last_read": "100.000000"}}
		case "conversations.history":
			return map[string]any{"messages": []any{msg("101.000000", "hi")}}
		}
		return apiError("unknown_method")
	}
	for run := range 2 {
		rec := &recorder{serve: serve}
		names := channels.NewNames(channels.NamesOptions{CacheDir: dir, Now: clock(t0.Add(time.Duration(run) * time.Minute))})
		res, err := Fetch(context.Background(), rec.client(), Options{CacheDir: dir, Names: names})
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Channels) != 1 || res.Channels[0].ChannelName != "busy" || len(res.Channels[0].Messages) != 1 {
			t.Fatalf("run %d: %s", run, dump(res))
		}
		if n := len(rec.only("conversations.info")); n != 1 {
			t.Errorf("run %d: %d conversations.info calls, want 1", run, n)
		}
		if h := rec.only("conversations.history"); len(h) != 1 || h[0].params["oldest"] != "100.000000" {
			t.Errorf("run %d: history %v", run, h)
		}
	}
	// What the calls learnt serves the commands that only need the name.
	broken := slackapi.NewFake(func(context.Context, string, map[string]any) (map[string]any, error) {
		return apiError("unexpected"), nil
	})
	names := channels.NewNames(channels.NamesOptions{CacheDir: dir, Now: clock(t0.Add(time.Hour))})
	if got, err := names.Lookup(context.Background(), broken, "C0BUSY0001"); got != "busy" || err != nil {
		t.Errorf("cached name = %q, %v", got, err)
	}
}

// A rate-limited name lookup falls back on the stale cached name; only
// the conversations left without any name are warned about.
func TestFetchRateLimitedNamesUseStaleEntries(t *testing.T) {
	dir := t.TempDir()
	ws := namedWorkspace()
	ws.counts["channels"] = append(ws.counts["channels"].([]any), map[string]any{"id": "C0RANDOM01", "has_unreads": true, "last_read": "10.000001"})
	ws.names["C0RANDOM01"] = "random"
	// Two days ago: expired.
	old := channels.NewNames(channels.NamesOptions{CacheDir: dir, WorkspaceURL: "https://test.slack.com", Now: clock(t0.Add(-48 * time.Hour))})
	old.Remember(nil, "C0GENERAL1", "general", old.Now())
	// The name of a DM is kept for the identity that looked it up: the
	// same credentials as the client below (NewFake).
	old.Remember(slackapi.NewFake(nil), "D0DM000001", "user-U0ALICE001", old.Now())

	inner := ws.recorder()
	client := slackapi.NewFake(func(_ context.Context, method string, params map[string]any) (map[string]any, error) {
		if method == "conversations.info" {
			return nil, &slackapi.RateLimitError{Method: method, RetryAfter: time.Minute}
		}
		return inner.serve(method, params), nil
	})
	res, err := Fetch(context.Background(), client, Options{CacheDir: dir, CountsOnly: true, Names: channels.NewNames(channels.NamesOptions{CacheDir: dir, Now: clock(t0)})})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, c := range res.Channels {
		got[c.ChannelID] = c.ChannelName
	}
	if want := map[string]string{"C0GENERAL1": "general", "D0DM000001": "user-U0ALICE001", "C0RANDOM01": ""}; !reflect.DeepEqual(got, want) {
		t.Errorf("names = %v, want %v", got, want)
	}
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "the names of 1 of 3 conversations were not looked up") {
		t.Errorf("warnings = %q", res.Warnings)
	}
}
