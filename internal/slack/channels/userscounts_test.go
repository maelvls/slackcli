package channels

import (
	"context"
	"fmt"
	"reflect"
	"testing"

	"github.com/maelvls/slackcli/internal/slackapi"
)

func TestIsRestricted(t *testing.T) {
	for code, want := range map[string]bool{
		"team_is_restricted":       true,
		"enterprise_is_restricted": true,
		"invalid_auth":             false,
		"is_restricted_no_more":    false,
		"":                         false,
	} {
		err := error(&slackapi.APIError{Method: "client.counts", Code: code})
		if got := IsRestricted(err); got != want {
			t.Errorf("IsRestricted(%q) = %v", code, got)
		}
		if got := IsRestricted(fmt.Errorf("fetching unread counts: %w", err)); got != want {
			t.Errorf("IsRestricted(wrapped %q) = %v", code, got)
		}
	}
	if IsRestricted(nil) || IsRestricted(fmt.Errorf("team_is_restricted")) {
		t.Error("IsRestricted is true for an error that is not Slack's")
	}
}

func TestUsersCounts(t *testing.T) {
	rec := static(map[string]any{
		"channels": []any{
			map[string]any{"id": "C1", "unread_count": 9.0, "unread_count_display": 3.0, "mention_count": 5.0, "mention_count_display": 1.0},
			map[string]any{"id": "C2", "is_member": false, "is_muted": true, "is_archived": true, "unread_count": 2.0},
			map[string]any{"name": "no id"},
		},
		"groups": []any{
			map[string]any{"id": "G1", "is_mpim": true, "is_member": false, "is_open": true, "mention_count": 2.0},
			map[string]any{"id": "G2"},
		},
		"ims": []any{
			map[string]any{"id": "D1", "dm_count": 2.0, "is_open": true},
			map[string]any{"id": "D2"},
		},
	})
	got, err := UsersCounts(context.Background(), rec.client())
	if err != nil {
		t.Fatal(err)
	}
	want := []CountsEntry{
		// The "display" counts, which leave out what the client hides, win.
		{ID: "C1", Kind: CountsChannel, UnreadCount: 3, MentionCount: 1, IsMember: true},
		{ID: "C2", Kind: CountsChannel, UnreadCount: 2, IsMuted: true, IsArchived: true},
		{ID: "G1", Kind: CountsMPIM, MentionCount: 2, IsMember: true},
		{ID: "G2", Kind: CountsChannel, IsMember: true},
		{ID: "D1", Kind: CountsDM, UnreadCount: 2, MentionCount: 2, IsMember: true},
		{ID: "D2", Kind: CountsDM},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("entries = %+v\nwant      %+v", got, want)
	}
	if !reflect.DeepEqual(rec.methods(), []string{"users.counts"}) {
		t.Errorf("calls = %v", rec.methods())
	}

	rec = static(apiError("invalid_auth"))
	if _, err := UsersCounts(context.Background(), rec.client()); !slackapi.IsAuthError(err) {
		t.Errorf("err = %v", err)
	}
}

func TestCountsEntryHasUnreads(t *testing.T) {
	for _, tt := range []struct {
		entry CountsEntry
		want  bool
	}{
		{CountsEntry{}, false},
		{CountsEntry{UnreadCount: 3}, true},
		{CountsEntry{MentionCount: 1}, true},
		// Muted: only a mention counts.
		{CountsEntry{UnreadCount: 9, IsMuted: true}, false},
		{CountsEntry{UnreadCount: 2, MentionCount: 1, IsMuted: true}, true},
		// Archived: never.
		{CountsEntry{UnreadCount: 4, MentionCount: 1, IsArchived: true}, false},
	} {
		if got := tt.entry.HasUnreads(); got != tt.want {
			t.Errorf("%+v.HasUnreads() = %v", tt.entry, got)
		}
	}
}

// On an Enterprise Grid that restricts client.counts, the IDs come from
// users.counts: the channels joined and the DMs that are open.
func TestListViaCountsFallsBackToUsersCounts(t *testing.T) {
	rec := &recorder{serve: func(method string, params map[string]any) (map[string]any, error) {
		switch method {
		case "client.counts":
			return apiError("team_is_restricted"), nil
		case "users.counts":
			return map[string]any{
				"channels": []any{map[string]any{"id": "C1", "is_member": true}, map[string]any{"id": "C2", "is_member": false}},
				"groups":   []any{map[string]any{"id": "G1", "is_member": true, "is_mpim": true}},
				"ims":      []any{map[string]any{"id": "D1", "is_open": true}, map[string]any{"id": "D2", "is_open": false}},
			}, nil
		case "conversations.info":
			return map[string]any{"channel": map[string]any{"id": params["channel"]}}, nil
		}
		return nil, fmt.Errorf("unexpected method: %s", method)
	}}
	page, err := ListViaCounts(context.Background(), rec.client(), ListOptions{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	// Paged locally, like the client.counts listing.
	if !reflect.DeepEqual(ids(page), []string{"C1", "G1"}) || page.NextCursor != "2" {
		t.Errorf("page = %+v", page)
	}
	if got := rec.methods()[:2]; !reflect.DeepEqual(got, []string{"client.counts", "users.counts"}) {
		t.Errorf("first calls = %v", got)
	}
	page, err = ListViaCounts(context.Background(), rec.client(), ListOptions{Limit: 2, Cursor: page.NextCursor})
	if err != nil || !reflect.DeepEqual(ids(page), []string{"D1"}) || page.NextCursor != "" {
		t.Errorf("second page = %+v, %v", page, err)
	}
}

// Any other failure of client.counts, and a failure of users.counts, is
// the error of the listing.
func TestListViaCountsFallbackErrors(t *testing.T) {
	rec := static(apiError("invalid_auth"))
	if _, err := ListViaCounts(context.Background(), rec.client(), ListOptions{}); !slackapi.IsErrorCode(err, "invalid_auth") {
		t.Errorf("err = %v", err)
	}
	if !reflect.DeepEqual(rec.methods(), []string{"client.counts"}) {
		t.Errorf("calls = %v, want no fallback", rec.methods())
	}

	rec = &recorder{serve: func(method string, _ map[string]any) (map[string]any, error) {
		if method == "client.counts" {
			return apiError("team_is_restricted"), nil
		}
		return apiError("enterprise_is_restricted"), nil
	}}
	if _, err := ListViaCounts(context.Background(), rec.client(), ListOptions{}); !slackapi.IsErrorCode(err, "enterprise_is_restricted") {
		t.Errorf("err = %v", err)
	}
}

func TestLookup(t *testing.T) {
	rec := &recorder{serve: func(method string, params map[string]any) (map[string]any, error) {
		switch {
		case method == "conversations.info" && params["channel"] == "C1":
			return map[string]any{"channel": map[string]any{"id": "C1", "name": "general", "last_read": "100.000001"}}, nil
		case method == "conversations.info":
			return map[string]any{"channel": map[string]any{"id": "D1", "is_im": true, "user": "U1", "last_read": "200.000001"}}, nil
		}
		return apiError("user_not_found"), nil
	}}
	if info, err := Lookup(context.Background(), rec.client(), "C1"); err != nil || info != (Info{Name: "general", LastRead: "100.000001"}) {
		t.Errorf("Lookup(C1) = %+v, %v", info, err)
	}
	// The read cursor survives a failed name lookup.
	if info, err := Lookup(context.Background(), rec.client(), "D1"); !slackapi.IsErrorCode(err, "user_not_found") || info != (Info{LastRead: "200.000001"}) {
		t.Errorf("Lookup(D1) = %+v, %v", info, err)
	}
}
