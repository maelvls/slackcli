package unreads

import (
	"context"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/maelvls/slackcli/internal/slackapi"
)

// grid is an Enterprise Grid workspace that restricts client.counts.
func grid() *recorder {
	return &recorder{serve: func(method string, params map[string]any) map[string]any {
		id, _ := params["channel"].(string)
		switch method {
		case "client.counts":
			return apiError("team_is_restricted")
		case "users.counts":
			return map[string]any{
				"channels": []any{
					map[string]any{"id": "C1", "name": "quiet", "unread_count_display": 0.0, "mention_count_display": 0.0},
					map[string]any{"id": "C2", "name": "busy", "unread_count_display": 3.0, "mention_count_display": 0.0},
					map[string]any{"id": "C3", "name": "muted", "is_muted": true, "unread_count_display": 9.0, "mention_count_display": 0.0},
					map[string]any{"id": "C4", "name": "muted-mention", "is_muted": true, "unread_count_display": 2.0, "mention_count_display": 1.0},
					map[string]any{"id": "C5", "name": "archived", "is_archived": true, "unread_count_display": 4.0, "mention_count_display": 0.0},
				},
				"groups": []any{map[string]any{"id": "G1", "is_mpim": true, "unread_count_display": 1.0, "mention_count_display": 0.0}},
				"ims": []any{
					map[string]any{"id": "D1", "user_id": "U1", "dm_count": 2.0},
					map[string]any{"id": "D2", "user_id": "U2", "dm_count": 0.0},
				},
			}
		case "conversations.info":
			ch := map[string]any{"id": id, "last_read": "100.000000"}
			if strings.HasPrefix(id, "D") {
				ch["is_im"], ch["user"] = true, "U1"
			} else {
				ch["name"] = "name-" + id
			}
			return map[string]any{"channel": ch}
		case "users.info":
			return map[string]any{"user": map[string]any{"id": params["user"], "name": "alice", "profile": map[string]any{"display_name": "Alice"}}}
		case "conversations.history":
			return map[string]any{"messages": []any{msg("101.000000", "hi", "user", "U1")}}
		}
		return apiError("unknown_method")
	}}
}

// Where client.counts is restricted, the conversations come from
// users.counts and the read cursor from conversations.info.
func TestFetchFallsBackToUsersCounts(t *testing.T) {
	rec := grid()
	res, err := Fetch(context.Background(), rec.client(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	hi := func(user string) []Message {
		return []Message{{TS: "101.000000", Time: at(101), Author: &Author{UserID: user}, Content: "hi"}}
	}
	// Not there: C1 (nothing unread), C3 (muted, no mention), C5 (archived),
	// D2 (nothing unread). users.counts has real counts, and no threads.
	want := Result{Channels: []Channel{
		{ChannelID: "D1", ChannelName: "Alice", ChannelType: TypeDM, UnreadCount: intp(2), MentionCount: 2, Messages: hi("U1")},
		{ChannelID: "C4", ChannelName: "name-C4", ChannelType: TypeChannel, UnreadCount: intp(2), MentionCount: 1, Messages: hi("U1")},
		{ChannelID: "C2", ChannelName: "name-C2", ChannelType: TypeChannel, UnreadCount: intp(3), Messages: hi("U1")},
		{ChannelID: "G1", ChannelName: "name-G1", ChannelType: TypeMPIM, UnreadCount: intp(1), Messages: hi("U1")},
	}}
	if !reflect.DeepEqual(res, want) {
		t.Errorf("got:\n%s\nwant:\n%s", dump(res), dump(want))
	}

	if n := len(rec.only("users.counts")); n != 1 {
		t.Errorf("users.counts calls = %d", n)
	}
	// One conversations.info per conversation gives both the name and the
	// read cursor.
	if n := len(rec.only("conversations.info")); n != 4 {
		t.Errorf("conversations.info calls = %d, want 4", n)
	}
	history := rec.only("conversations.history")
	if len(history) != 4 {
		t.Fatalf("conversations.history calls = %d, want 4", len(history))
	}
	for _, c := range history {
		if c.params["oldest"] != "100.000000" || c.params["inclusive"] != false {
			t.Errorf("history params = %v", c.params)
		}
	}
}

func TestFetchFallbackCountsOnly(t *testing.T) {
	rec := grid()
	res, err := Fetch(context.Background(), rec.client(), Options{CountsOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Channels) != 4 || res.Channels[0].ChannelName != "Alice" || res.Channels[0].Messages != nil {
		t.Errorf("channels:\n%s", dump(res))
	}
	if n := len(rec.only("conversations.history")); n != 0 {
		t.Errorf("conversations.history calls = %d, want none", n)
	}
}

// Only a "*_is_restricted" answer of client.counts leads to users.counts,
// and a users.counts that fails is the error.
func TestFetchFallbackErrors(t *testing.T) {
	rec := &recorder{serve: func(string, map[string]any) map[string]any { return apiError("invalid_auth") }}
	if _, err := Fetch(context.Background(), rec.client(), Options{}); !slackapi.IsAuthError(err) || !strings.Contains(err.Error(), "fetching unread counts") {
		t.Errorf("err = %v", err)
	}
	if n := len(rec.only("users.counts")); n != 0 {
		t.Errorf("users.counts was called %d times after invalid_auth", n)
	}

	rec = &recorder{serve: func(method string, _ map[string]any) map[string]any {
		if method == "client.counts" {
			return apiError("enterprise_is_restricted")
		}
		return apiError("team_is_restricted")
	}}
	if _, err := Fetch(context.Background(), rec.client(), Options{}); !slackapi.IsErrorCode(err, "team_is_restricted") || !strings.Contains(err.Error(), "users.counts") {
		t.Errorf("err = %v", err)
	}
}

// Without a read cursor the unread messages cannot be told from the read
// ones: they are not fetched, rather than the newest ones passed off as
// unread.
func TestFetchFallbackWithoutReadCursor(t *testing.T) {
	inner := grid()
	for name, tc := range map[string]struct {
		info      func() (map[string]any, error)
		wantError string
		warnings  int
	}{
		"unreadable": {func() (map[string]any, error) { return apiError("channel_not_found"), nil }, "channel_not_found", 0},
		"rate limited": {func() (map[string]any, error) {
			return nil, &slackapi.RateLimitError{Method: "conversations.info", RetryAfter: time.Minute}
		}, "ratelimited", 2},
	} {
		t.Run(name, func(t *testing.T) {
			var mu sync.Mutex
			history := 0
			client := slackapi.NewFake(func(_ context.Context, method string, params map[string]any) (map[string]any, error) {
				switch method {
				case "conversations.info":
					return tc.info()
				case "conversations.history":
					mu.Lock()
					history++
					mu.Unlock()
				}
				return inner.serve(method, params), nil
			})
			res, err := Fetch(context.Background(), client, Options{})
			if err != nil {
				t.Fatal(err)
			}
			if len(res.Channels) != 4 {
				t.Fatalf("channels:\n%s", dump(res))
			}
			for _, c := range res.Channels {
				if c.MessagesError != tc.wantError || c.Messages != nil || c.ChannelName != "" {
					t.Errorf("channel = %+v", c)
				}
			}
			if history != 0 {
				t.Errorf("conversations.history calls = %d, want none", history)
			}
			if len(res.Warnings) != tc.warnings {
				t.Errorf("warnings = %q", res.Warnings)
			}
			if tc.warnings > 0 && !strings.Contains(res.Warnings[0], "4 of 4 conversations were not fetched") {
				t.Errorf("warning = %q", res.Warnings[0])
			}
		})
	}
}
