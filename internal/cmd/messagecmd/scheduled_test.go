package messagecmd

import (
	"reflect"
	"strings"
	"testing"

	"github.com/maelvls/slackcli/internal/auth"
	"github.com/maelvls/slackcli/internal/slackapi"
	"github.com/maelvls/slackcli/internal/testenv"
)

func scheduledListFixture(h *harness) {
	h.fixtures["chat.scheduledMessages.list"] = func(p map[string]any) (map[string]any, error) {
		channel, _ := p["channel"].(string)
		if channel == "" {
			channel = testChannel
		}
		return map[string]any{
			"ok": true,
			"scheduled_messages": []any{
				map[string]any{"text": "scheduled", "post_at": 1770168709, "id": "Q1234ABCD", "date_created": 1770160000, "channel_id": channel},
			},
			"response_metadata": map[string]any{"next_cursor": "next-1"},
		}, nil
	}
}

const wantScheduledList = `{
	"ok": true,
	"scheduled_messages": [{"id": "Q1234ABCD", "channel_id": "C12345678", "post_at": 1770168709, "date_created": 1770160000, "text": "scheduled"}],
	"next_cursor": "next-1"
}`

func TestScheduledList(t *testing.T) {
	h := newHarness(t)
	scheduledListFixture(h)
	got := h.mustRun("scheduled", "list", "--channel", "general", "--cursor", "cursor-1",
		"--oldest", "1770160000", "--latest", "1770170000", "--limit", "25")
	wantJSON(t, got, wantScheduledList)
	calls := h.callsTo("chat.scheduledMessages.list")
	want := map[string]any{"channel": testChannel, "cursor": "cursor-1", "oldest": "1770160000", "latest": "1770170000", "limit": 25}
	if len(calls) != 1 || !reflect.DeepEqual(calls[0].params, want) {
		t.Errorf("chat.scheduledMessages.list calls = %v", calls)
	}
}

// The fields of an entry come out in the order the help gives, not sorted.
func TestScheduledListFieldOrder(t *testing.T) {
	h := newHarness(t)
	scheduledListFixture(h)
	out, err := h.run("scheduled", "list")
	if err != nil {
		t.Fatal(err)
	}
	want := `{
  "ok": true,
  "scheduled_messages": [
    {
      "id": "Q1234ABCD",
      "channel_id": "C12345678",
      "post_at": 1770168709,
      "date_created": 1770160000,
      "text": "scheduled"
    }
  ],
  "next_cursor": "next-1"
}
`
	if want := testenv.CompactJSON(t, want); out != want {
		t.Errorf("output:\n%s\nwant:\n%s", out, want)
	}
}

func TestScheduledDefaultsToList(t *testing.T) {
	h := newHarness(t)
	scheduledListFixture(h)
	wantJSON(t, h.mustRun("scheduled"), wantScheduledList)
	h.wantCalls(call{"chat.scheduledMessages.list", map[string]any{}})

	// The flags of "list" work without naming it.
	h.mustRun("scheduled", "--channel", "U05BRPTKL6A", "--limit", "5")
	h.wantCalls(
		call{"conversations.open", map[string]any{"users": "U05BRPTKL6A"}},
		call{"chat.scheduledMessages.list", map[string]any{"channel": "D12345678", "limit": 5}},
	)
}

func TestScheduledListValidation(t *testing.T) {
	for _, tt := range []struct {
		args []string
		want string
	}{
		// --limit is an integer flag, validated like on every other command.
		{[]string{"scheduled", "list", "--limit", "abc"}, `invalid argument "abc" for "--limit" flag`},
		{[]string{"scheduled", "list", "--limit", "0"}, "invalid --limit value 0: must be a positive integer"},
		{[]string{"scheduled", "--limit", "-2"}, "invalid --limit value -2: must be a positive integer"},
		{[]string{"scheduled", "list", "--limit", ""}, `invalid argument "" for "--limit" flag`},
		{[]string{"scheduled", "list", "--oldest", "yesterday"}, "invalid --oldest"},
		{[]string{"scheduled", "list", "--channel", ""}, "--channel needs a value"},
		{[]string{"scheduled", "list", "--channel", "https://example.com/x"}, "--channel: unsupported target URL"},
		{[]string{"scheduled", "list", "extra"}, `unknown command "extra"`},
	} {
		h := newHarness(t)
		h.mustFail(tt.want, tt.args...)
		if n := len(h.callsTo("chat.scheduledMessages.list")); n != 0 {
			t.Errorf("%v: Slack was called", tt.args)
		}
	}
}

// "scheduled_messages" is printed even when nothing is scheduled.
func TestScheduledListWithNothingScheduled(t *testing.T) {
	h := newHarness(t)
	for _, args := range [][]string{{"scheduled", "list"}, {"scheduled"}, {"scheduled", "list", "--channel", testChannel}} {
		h.mustRun(args...)
		if want := "{\"ok\":true,\"scheduled_messages\":[]}\n"; h.stdout.String() != want {
			t.Errorf("%v printed %s, want %s", args, h.stdout, want)
		}
	}
	for _, name := range []string{"scheduled", "list"} {
		cmd := findCommand(t, h.command(), "scheduled")
		if name == "list" {
			cmd = findCommand(t, h.command(), "scheduled", "list")
		}
		if typ := cmd.Flags().Lookup("limit").Value.Type(); typ != "int" {
			t.Errorf("%s --limit is a %s flag, want int", name, typ)
		}
	}
}

// Cancelling deletes a message a human scheduled: refused in safe mode,
// before anything is looked up.
func TestSafeModeRefusesScheduledCancel(t *testing.T) {
	h := newHarness(t)
	h.rt.SafeModeFlag = true
	h.mustFail(`"message scheduled cancel" is blocked`, "scheduled", "cancel", "Q1234ABCD", "--channel", "#general")
	h.wantCalls()
	// Listing them is reading.
	h.mustRun("scheduled", "list")
}

func TestScheduledCancel(t *testing.T) {
	h := newHarness(t)
	got := h.mustRun("scheduled", "cancel", "Q1234ABCD", "--channel", testChannel)
	h.wantCalls(call{"chat.deleteScheduledMessage", map[string]any{"channel": testChannel, "scheduled_message_id": "Q1234ABCD"}})
	wantJSON(t, got, `{"ok": true, "channel_id": "C12345678", "scheduled_message_id": "Q1234ABCD"}`)

	// The conversation may be given as for "send": name, user or URL.
	for target, wantChannel := range map[string]string{
		"#general":    testChannel,
		"U05BRPTKL6A": "D12345678",
		testURL:       testChannel,
	} {
		got := h.mustRun("scheduled", "cancel", "Q1234ABCD", "--channel", target)
		if got["channel_id"] != wantChannel {
			t.Errorf("--channel %s: channel_id = %v, want %s", target, got["channel_id"], wantChannel)
		}
	}
}

func TestScheduledCancelFailures(t *testing.T) {
	h := newHarness(t)
	h.mustFail(`required flag(s) "channel" not set`, "scheduled", "cancel", "Q1234ABCD")
	h.mustFail("accepts 1 arg", "scheduled", "cancel", "--channel", testChannel)
	h.mustFail("--channel: missing target", "scheduled", "cancel", "Q1234ABCD", "--channel", "")
	h.mustFail("<id> is empty", "scheduled", "cancel", " ", "--channel", testChannel)
	h.wantCalls()

	h.fixtures["chat.deleteScheduledMessage"] = func(map[string]any) (map[string]any, error) {
		return nil, apiError("chat.deleteScheduledMessage", "invalid_scheduled_message_id")
	}
	h.mustFail("no pending scheduled message Q1234ABCD", "scheduled", "cancel", "Q1234ABCD", "--channel", testChannel)
}

// useBrowserSession replaces the app token of the test workspace with a
// browser session, the default credentials.
func (h *harness) useBrowserSession() {
	h.t.Helper()
	if err := h.rt.Store.Upsert(auth.Workspace{
		URL:  testWorkspace,
		Auth: slackapi.Auth{Type: slackapi.AuthBrowser, XoxcToken: "xoxc-test", XoxdCookie: "xoxd-test"},
	}); err != nil {
		h.t.Fatal(err)
	}
}

// sessionScheduledFixtures make the fake Slack answer as it does a browser
// session: the chat.* scheduled message methods are refused, and the
// messages scheduled in the Slack client are drafts with a send time.
func sessionScheduledFixtures(h *harness, hasMore bool) {
	refuse := func(method string) func(map[string]any) (map[string]any, error) {
		return func(map[string]any) (map[string]any, error) { return nil, apiError(method, "not_allowed_token_type") }
	}
	for _, m := range []string{"chat.scheduledMessages.list", "chat.deleteScheduledMessage", "chat.scheduleMessage"} {
		h.fixtures[m] = refuse(m)
	}
	section := func(text string) []any {
		return []any{map[string]any{"type": "rich_text", "elements": []any{map[string]any{
			"type": "rich_text_section", "elements": []any{map[string]any{"type": "text", "text": text}},
		}}}}
	}
	h.fixtures["drafts.list"] = func(map[string]any) (map[string]any, error) {
		return map[string]any{"ok": true, "has_more": hasMore, "drafts": []any{
			map[string]any{"id": "Dr0LATER", "date_created": 1770160000, "date_scheduled": 1770268709, "last_updated_ts": "1770160000.000100",
				"destinations": []any{map[string]any{"channel_id": testChannel}}, "blocks": section("later")},
			map[string]any{"id": "Dr0PLAIN", "date_created": 1770150000, "last_updated_ts": "1770150000.000100",
				"destinations": []any{map[string]any{"channel_id": testChannel}}, "blocks": section("unsent draft")},
			map[string]any{"id": "Dr0SOON", "date_created": 1770160001, "date_scheduled": 1770168709, "last_updated_ts": "1770160001.000200",
				"destinations": []any{map[string]any{"channel_id": "D12345678"}}, "blocks": section("soon")},
		}}, nil
	}
}

// With a browser session, the list is that of the drafts that have a send
// time, in the shape of chat.scheduledMessages.list.
func TestScheduledListWithBrowserSession(t *testing.T) {
	h := newHarness(t)
	h.useBrowserSession()
	sessionScheduledFixtures(h, false)
	got := h.mustRun("scheduled", "list")
	wantJSON(t, got, `{
	"ok": true,
	"scheduled_messages": [
		{"id": "Dr0SOON", "channel_id": "D12345678", "post_at": 1770168709, "date_created": 1770160001, "text": "soon"},
		{"id": "Dr0LATER", "channel_id": "C12345678", "post_at": 1770268709, "date_created": 1770160000, "text": "later"}
	]
}`)
	h.wantCalls(
		call{"chat.scheduledMessages.list", map[string]any{}},
		call{"drafts.list", map[string]any{"is_active": true, "limit": 100}},
	)

	// The filters are applied to the drafts.
	got = h.mustRun("scheduled", "--channel", "#general", "--oldest", "1770000000", "--limit", "5")
	if msgs, _ := got["scheduled_messages"].([]any); len(msgs) != 1 || msgs[0].(map[string]any)["id"] != "Dr0LATER" {
		t.Errorf("--channel #general: %v", got)
	}
	got = h.mustRun("scheduled", "list", "--latest", "1770000000")
	if want := []any{}; !reflect.DeepEqual(got["scheduled_messages"], want) {
		t.Errorf("--latest before both: %v", got)
	}

	// drafts.list has no pages.
	h.mustFail("--cursor cannot be used with a browser session", "scheduled", "list", "--cursor", "next-1")
}

func TestScheduledListWithBrowserSessionWarnsWhenSlackHasMore(t *testing.T) {
	h := newHarness(t)
	h.useBrowserSession()
	sessionScheduledFixtures(h, true)
	got := h.mustRun("scheduled", "list")
	warnings, _ := got["warnings"].([]any)
	if len(warnings) != 1 || !strings.Contains(warnings[0].(string), "scheduled messages may be missing") {
		t.Errorf("warnings = %v", got["warnings"])
	}
	if h.stderr.Len() != 0 {
		t.Errorf("stderr = %q", h.stderr)
	}
}

// An ID that "list" prints for a browser session is one "cancel" takes.
func TestScheduledCancelWithBrowserSession(t *testing.T) {
	h := newHarness(t)
	h.useBrowserSession()
	sessionScheduledFixtures(h, false)
	got := h.mustRun("scheduled", "cancel", "Dr0LATER", "--channel", testChannel)
	wantJSON(t, got, `{"ok": true, "channel_id": "C12345678", "scheduled_message_id": "Dr0LATER"}`)
	h.wantCalls(
		call{"drafts.list", map[string]any{"is_active": true, "limit": 100}},
		call{"drafts.delete", map[string]any{"draft_id": "Dr0LATER", "client_last_updated_ts": "1770160000.0001000"}},
	)

	// A draft that is not scheduled, or not in --channel, is not deleted.
	h.mustFail("not scheduled", "scheduled", "cancel", "Dr0PLAIN", "--channel", testChannel)
	h.mustFail("scheduled in D12345678, not in C12345678", "scheduled", "cancel", "Dr0SOON", "--channel", testChannel)
	if n := len(h.callsTo("drafts.delete")); n != 0 {
		t.Errorf("drafts.delete was called %d times", n)
	}

	// A Q… ID still goes to chat.deleteScheduledMessage, whose refusal
	// says what this session can cancel.
	h.mustFail(`"Dr…" IDs that this command cancels`, "scheduled", "cancel", "Q1234ABCD", "--channel", testChannel)
}

// Safe mode refuses a cancel through drafts as well, before any call.
func TestSafeModeRefusesScheduledCancelWithBrowserSession(t *testing.T) {
	h := newHarness(t)
	h.useBrowserSession()
	sessionScheduledFixtures(h, false)
	h.rt.SafeModeFlag = true
	h.mustFail(`"message scheduled cancel" is blocked`, "scheduled", "cancel", "Dr0LATER", "--channel", testChannel)
	h.wantCalls()
	h.mustRun("scheduled", "list")
}

// "send --schedule" with a browser session that Slack refuses says that
// nothing was sent, and what to do instead.
func TestSendScheduleWithBrowserSessionRefused(t *testing.T) {
	h := newHarness(t)
	h.useBrowserSession()
	sessionScheduledFixtures(h, false)
	err := h.mustFail("nothing was sent or scheduled", "send", testChannel, "later", "--schedule-in", "3h")
	for _, want := range []string{"message draft create", "SLACK_TOKEN", "not_allowed_token_type"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want it to contain %q", err, want)
		}
	}
	if methods := h.methods(); !reflect.DeepEqual(methods, []string{"chat.scheduleMessage"}) {
		t.Errorf("calls = %v", methods)
	}
}

// An app token that Slack refuses keeps Slack's error: only a browser
// session has drafts to fall back on.
func TestScheduledWithRefusedAppTokenIsUnchanged(t *testing.T) {
	h := newHarness(t)
	sessionScheduledFixtures(h, false)
	err := h.mustFail("not_allowed_token_type", "scheduled", "list")
	if !strings.HasPrefix(err.Error(), "listing scheduled messages: ") {
		t.Errorf("error = %v", err)
	}
	h.mustFail("not_allowed_token_type", "scheduled", "cancel", "Dr0LATER", "--channel", testChannel)
	err = h.mustFail("not_allowed_token_type", "send", testChannel, "later", "--schedule-in", "3h")
	if strings.Contains(err.Error(), "nothing was sent") {
		t.Errorf("error = %v", err)
	}
	if n := len(h.callsTo("drafts.list")); n != 0 {
		t.Errorf("drafts.list was called %d times", n)
	}
}
