package scheduled

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/maelvls/slackcli/internal/slackapi"
)

// sessionDrafts is what drafts.list answers in these tests: two scheduled
// messages, in the wrong order, a draft that is not scheduled, a scheduled
// draft addressed to two conversations, and junk.
func sessionDrafts() []any {
	return []any{
		map[string]any{
			"id": "Dr0LATER", "date_created": 1770000100, "date_scheduled": 1770300000, "last_updated_ts": "1770000100.123456",
			"destinations": []any{map[string]any{"channel_id": "C0GENERAL"}},
			"blocks":       richText("see you monday"),
		},
		map[string]any{
			"id": "Dr0PLAIN", "date_created": 1770000000, "date_scheduled": 0, "last_updated_ts": "1770000000.000001",
			"destinations": []any{map[string]any{"channel_id": "C0GENERAL"}},
			"blocks":       richText("not scheduled"),
		},
		map[string]any{
			"id": "Dr0SOON", "date_created": 1770000200, "date_scheduled": 1770200000, "last_updated_ts": "1770000200.5",
			"destinations": []any{map[string]any{"channel_id": "C0RANDOM", "thread_ts": "1769999999.000100"}},
			"blocks":       richText("in a thread"),
		},
		map[string]any{
			"id": "Dr0TWICE", "date_created": 1770000300, "date_scheduled": 1770250000, "last_updated_ts": "1770000300.1",
			"destinations": []any{map[string]any{"channel_id": "C0RANDOM"}, map[string]any{"channel_id": "C0GENERAL"}},
			"blocks":       richText("to two places"),
		},
		"junk",
	}
}

func richText(text string) []any {
	return []any{map[string]any{"type": "rich_text", "elements": []any{map[string]any{
		"type": "rich_text_section", "elements": []any{map[string]any{"type": "text", "text": text}},
	}}}}
}

// sessionAPI is a fake Slack that refuses the chat.* scheduled message
// methods to the browser session, as Slack does, and answers drafts.list
// with drafts.
type sessionAPI struct {
	t       *testing.T
	drafts  []any
	hasMore bool
	calls   []string
	params  map[string]map[string]any
}

func (s *sessionAPI) client() *slackapi.Client {
	s.params = map[string]map[string]any{}
	return slackapi.NewFake(func(_ context.Context, method string, params map[string]any) (map[string]any, error) {
		s.calls = append(s.calls, method)
		s.params[method] = params
		switch method {
		case "chat.scheduledMessages.list", "chat.deleteScheduledMessage", "chat.scheduleMessage":
			return map[string]any{"ok": false, "error": "not_allowed_token_type"}, nil
		case "drafts.list":
			return map[string]any{"ok": true, "drafts": s.drafts, "has_more": s.hasMore}, nil
		case "drafts.delete":
			return map[string]any{"ok": true}, nil
		}
		s.t.Errorf("unexpected method %s", method)
		return nil, nil
	})
}

func TestListThroughDraftsWhenTheSessionIsRefused(t *testing.T) {
	api := &sessionAPI{t: t, drafts: sessionDrafts()}
	res, err := List(context.Background(), api.client(), ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"chat.scheduledMessages.list", "drafts.list"}; !reflect.DeepEqual(api.calls, want) {
		t.Errorf("calls = %v, want %v", api.calls, want)
	}
	// Only the active drafts, as many as Slack gives at once.
	if want := map[string]any{"is_active": true, "limit": DraftsPageSize}; !reflect.DeepEqual(api.params["drafts.list"], want) {
		t.Errorf("drafts.list params = %v, want %v", api.params["drafts.list"], want)
	}
	// Soonest first; the draft that is not scheduled is left out; a draft
	// addressed to two conversations is listed with its first.
	want := []Message{
		{ID: "Dr0SOON", ChannelID: "C0RANDOM", PostAt: 1770200000, DateCreated: 1770000200, Text: "in a thread"},
		{ID: "Dr0TWICE", ChannelID: "C0RANDOM", PostAt: 1770250000, DateCreated: 1770000300, Text: "to two places"},
		{ID: "Dr0LATER", ChannelID: "C0GENERAL", PostAt: 1770300000, DateCreated: 1770000100, Text: "see you monday"},
	}
	if !reflect.DeepEqual(res.ScheduledMessages, want) {
		t.Errorf("scheduled messages = %+v\nwant %+v", res.ScheduledMessages, want)
	}
	if !res.OK || res.NextCursor != "" || res.Incomplete {
		t.Errorf("result = %+v", res)
	}
}

// --channel, --oldest, --latest and --limit, which drafts.list does not
// take, are applied to what it returns.
func TestListThroughDraftsFilters(t *testing.T) {
	for _, tt := range []struct {
		name string
		opts ListOptions
		want []string
	}{
		{"channel", ListOptions{ChannelID: "C0GENERAL"}, []string{"Dr0TWICE", "Dr0LATER"}},
		{"oldest is inclusive", ListOptions{Oldest: "1770250000"}, []string{"Dr0TWICE", "Dr0LATER"}},
		{"latest is inclusive", ListOptions{Latest: "1770250000"}, []string{"Dr0SOON", "Dr0TWICE"}},
		{"fractional bounds", ListOptions{Oldest: "1770199999.5", Latest: " 1770200000.5 "}, []string{"Dr0SOON"}},
		{"limit keeps the soonest", ListOptions{Limit: 2}, []string{"Dr0SOON", "Dr0TWICE"}},
		{"all together", ListOptions{ChannelID: "C0RANDOM", Oldest: "1770200001", Limit: 5}, []string{"Dr0TWICE"}},
		{"nothing matches", ListOptions{ChannelID: "C0NOTHING"}, nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			api := &sessionAPI{t: t, drafts: sessionDrafts()}
			res, err := List(context.Background(), api.client(), tt.opts)
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, m := range res.ScheduledMessages {
				got = append(got, m.ID)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("ids = %v, want %v", got, tt.want)
			}
			if tt.opts.ChannelID != "" {
				for _, m := range res.ScheduledMessages {
					if m.ChannelID != tt.opts.ChannelID {
						t.Errorf("%s is listed in %s, want %s", m.ID, m.ChannelID, tt.opts.ChannelID)
					}
				}
			}
		})
	}
}

func TestListThroughDraftsSaysWhenSlackHasMore(t *testing.T) {
	api := &sessionAPI{t: t, drafts: sessionDrafts(), hasMore: true}
	res, err := List(context.Background(), api.client(), ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Incomplete || len(res.ScheduledMessages) != 3 {
		t.Errorf("result = %+v, want 3 messages and Incomplete", res)
	}
}

// drafts.list has no pages: a cursor cannot be honoured, and is refused
// rather than ignored.
func TestListThroughDraftsRefusesACursor(t *testing.T) {
	api := &sessionAPI{t: t, drafts: sessionDrafts()}
	_, err := List(context.Background(), api.client(), ListOptions{Cursor: "abc"})
	if err == nil || !strings.Contains(err.Error(), "--cursor") || !strings.Contains(err.Error(), "drafts.list") {
		t.Errorf("err = %v", err)
	}
	if want := []string{"chat.scheduledMessages.list"}; !reflect.DeepEqual(api.calls, want) {
		t.Errorf("calls = %v, want %v", api.calls, want)
	}
}

// An app token that Slack refuses gets Slack's refusal, as before: only a
// browser session has the drafts API to fall back on.
func TestListWithARefusedAppTokenDoesNotFallBack(t *testing.T) {
	var calls []string
	client := slackapi.New(slackapi.Auth{Type: slackapi.AuthStandard, Token: "xoxb-test"}, "https://acme.slack.com",
		slackapi.WithHandler(func(_ context.Context, method string, _ map[string]any) (map[string]any, error) {
			calls = append(calls, method)
			return map[string]any{"ok": false, "error": "not_allowed_token_type"}, nil
		}))
	_, err := List(context.Background(), client, ListOptions{})
	if !slackapi.IsErrorCode(err, "not_allowed_token_type") || !strings.HasPrefix(err.Error(), "listing scheduled messages: ") {
		t.Errorf("err = %v", err)
	}
	if want := []string{"chat.scheduledMessages.list"}; !reflect.DeepEqual(calls, want) {
		t.Errorf("calls = %v, want %v", calls, want)
	}
}

// What List prints for a browser session, Cancel takes: a draft ID goes
// to drafts.delete, with the draft's last_updated_ts.
func TestCancelScheduledDraft(t *testing.T) {
	api := &sessionAPI{t: t, drafts: sessionDrafts()}
	if err := Cancel(context.Background(), api.client(), "C0RANDOM", "Dr0SOON"); err != nil {
		t.Fatal(err)
	}
	if want := []string{"drafts.list", "drafts.delete"}; !reflect.DeepEqual(api.calls, want) {
		t.Errorf("calls = %v, want %v", api.calls, want)
	}
	want := map[string]any{"draft_id": "Dr0SOON", "client_last_updated_ts": "1770000200.5000000"}
	if !reflect.DeepEqual(api.params["drafts.delete"], want) {
		t.Errorf("drafts.delete params = %v, want %v", api.params["drafts.delete"], want)
	}

	// A draft addressed to two conversations may be named by either.
	api = &sessionAPI{t: t, drafts: sessionDrafts()}
	if err := Cancel(context.Background(), api.client(), "C0GENERAL", "Dr0TWICE"); err != nil {
		t.Fatal(err)
	}
}

// Cancel never deletes a draft that is not a pending scheduled message,
// nor one scheduled in another conversation than the one named.
func TestCancelScheduledDraftRefusals(t *testing.T) {
	for _, tt := range []struct {
		name, channel, id string
		hasMore           bool
		want              []string
	}{
		{"not scheduled", "C0GENERAL", "Dr0PLAIN", false, []string{"Dr0PLAIN is a draft that is not scheduled", "message draft delete"}},
		{"another channel", "C0GENERAL", "Dr0SOON", false, []string{"scheduled in C0RANDOM, not in C0GENERAL"}},
		{"unknown", "C0GENERAL", "Dr0GONE", false, []string{"no pending scheduled message Dr0GONE", "already sent or cancelled"}},
		{"unknown, more drafts", "C0GENERAL", "Dr0GONE", true, []string{"no pending scheduled message Dr0GONE", "more drafts than the"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			api := &sessionAPI{t: t, drafts: sessionDrafts(), hasMore: tt.hasMore}
			err := Cancel(context.Background(), api.client(), tt.channel, tt.id)
			if err == nil {
				t.Fatal("Cancel succeeded")
			}
			for _, w := range tt.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("err = %v, want it to contain %q", err, w)
				}
			}
			if want := []string{"drafts.list"}; !reflect.DeepEqual(api.calls, want) {
				t.Errorf("calls = %v, want %v", api.calls, want)
			}
		})
	}
}

// A Q… ID is cancelled with chat.deleteScheduledMessage whatever the
// credentials; when Slack refuses it to the browser session, the error
// says what the session can cancel instead.
func TestCancelAPIScheduledMessageWithASession(t *testing.T) {
	api := &sessionAPI{t: t, drafts: sessionDrafts()}
	err := Cancel(context.Background(), api.client(), "C0GENERAL", "Q0123ABCD")
	if !slackapi.IsErrorCode(err, "not_allowed_token_type") {
		t.Fatalf("err = %v, want Slack's refusal", err)
	}
	for _, w := range []string{"browser session", "Dr", "the token that scheduled it"} {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("err = %v, want it to contain %q", err, w)
		}
	}
	if want := []string{"chat.deleteScheduledMessage"}; !reflect.DeepEqual(api.calls, want) {
		t.Errorf("calls = %v, want %v", api.calls, want)
	}
}

// An app token has no drafts: whatever the ID, Cancel calls
// chat.deleteScheduledMessage, as before.
func TestCancelWithAnAppTokenNeverUsesDrafts(t *testing.T) {
	var calls []string
	client := slackapi.New(slackapi.Auth{Type: slackapi.AuthStandard, Token: "xoxp-test"}, "https://acme.slack.com",
		slackapi.WithHandler(func(_ context.Context, method string, _ map[string]any) (map[string]any, error) {
			calls = append(calls, method)
			return map[string]any{"ok": true}, nil
		}))
	if err := Cancel(context.Background(), client, "C0GENERAL", "Dr0SOON"); err != nil {
		t.Fatal(err)
	}
	if want := []string{"chat.deleteScheduledMessage"}; !reflect.DeepEqual(calls, want) {
		t.Errorf("calls = %v, want %v", calls, want)
	}
}
