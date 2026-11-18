package messages

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/maelvls/slackcli/internal/slack/enrich"
	"github.com/maelvls/slackcli/internal/slackapi"
)

// historyMessages is FetchChannelHistory for the tests that only look at
// the messages.
func historyMessages(ctx context.Context, client *slackapi.Client, channelID string, opts HistoryOptions) ([]Message, error) {
	h, err := FetchChannelHistory(ctx, client, channelID, opts)
	return h.Messages, err
}

func TestIsSystem(t *testing.T) {
	for subtype, want := range map[string]bool{
		"channel_join": true, "channel_leave": true, "channel_topic": true, "group_join": true, "channel_archive": true,
		"": false, "bot_message": false, "thread_broadcast": false, "file_share": false, "me_message": false,
	} {
		if got := IsSystem(subtype); got != want {
			t.Errorf("IsSystem(%q) = %v, want %v", subtype, got, want)
		}
	}
}

// In a busy public channel, joins outnumber what people wrote: they are
// skipped, and do not count towards the limit.
func TestFetchChannelHistoryHidesSystemMessages(t *testing.T) {
	// Newest first, as Slack answers: 3 messages among 7 joins.
	all := []map[string]any{
		raw("10.000000", "ten"),
		raw("9.000000", "<@U1> has joined the channel", "subtype", "channel_join"),
		raw("8.000000", "<@U2> has joined the channel", "subtype", "channel_join"),
		raw("7.000000", "seven", "subtype", "bot_message"),
		raw("6.000000", "<@U3> has joined the channel", "subtype", "channel_join"),
		raw("5.000000", "<@U4> has left the channel", "subtype", "channel_leave"),
		raw("4.000000", "set the channel topic: x", "subtype", "channel_topic"),
		raw("3.000000", "<@U5> has joined the channel", "subtype", "channel_join"),
		raw("2.000000", "two"),
		raw("1.000000", "<@U6> has joined the channel", "subtype", "channel_join"),
	}
	serve := func(_ string, params map[string]any) (map[string]any, error) {
		limit := params["limit"].(int)
		start := 0
		if latest, _ := params["latest"].(string); latest != "" {
			for start < len(all) && CompareTS(all[start]["ts"].(string), latest) >= 0 {
				start++
			}
		}
		end := min(start+limit, len(all))
		return map[string]any{"messages": anys(all[start:end]...), "has_more": end < len(all)}, nil
	}

	rec := &recorder{serve: serve}
	h, err := FetchChannelHistory(context.Background(), rec.client(), "C12345678", HistoryOptions{Limit: 3})
	if err != nil {
		t.Fatal(err)
	}
	if got := tsList(h.Messages); !reflect.DeepEqual(got, []string{"2.000000", "7.000000", "10.000000"}) || h.SystemHidden != 6 {
		t.Errorf("messages = %v, %d system messages hidden", got, h.SystemHidden)
	}
	if h.Messages[1].Subtype != "bot_message" {
		t.Errorf("subtype = %q", h.Messages[1].Subtype)
	}
	// The first page asks for what is wanted; once system messages have
	// been skipped, full pages.
	if len(rec.calls) != 2 || rec.calls[0].params["limit"] != 3 || rec.calls[1].params["limit"] != pageSize || rec.calls[1].params["latest"] != "8.000000" {
		t.Errorf("calls = %+v", rec.calls)
	}

	// Asked for, they are listed like any other message.
	rec = &recorder{serve: serve}
	h, err = FetchChannelHistory(context.Background(), rec.client(), "C12345678", HistoryOptions{Limit: 3, IncludeSystem: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := tsList(h.Messages); !reflect.DeepEqual(got, []string{"8.000000", "9.000000", "10.000000"}) || h.SystemHidden != 0 || len(rec.calls) != 1 {
		t.Errorf("--include-system: messages = %v, hidden = %d, calls = %+v", got, h.SystemHidden, rec.calls)
	}

	// Nothing but system messages: an empty list, and their number.
	rec = &recorder{serve: func(string, map[string]any) (map[string]any, error) {
		return map[string]any{"messages": anys(all[1], all[2])}, nil
	}}
	h, err = FetchChannelHistory(context.Background(), rec.client(), "C12345678", HistoryOptions{})
	if err != nil || len(h.Messages) != 0 || h.SystemHidden != 2 {
		t.Errorf("only system messages: %+v, %v", h, err)
	}
}

// threadsFake serves the replies of two threads, 2.000000 (two replies)
// and 4.000000 (one reply).
func threadsFake(fail map[string]any) *recorder {
	return &recorder{serve: func(method string, params map[string]any) (map[string]any, error) {
		if method != "conversations.replies" {
			return nil, errors.New("unexpected call to " + method)
		}
		ts := params["ts"].(string)
		switch f := fail[ts].(type) {
		case error:
			return nil, f
		case string:
			return apiError(f), nil
		}
		switch ts {
		case "2.000000":
			return map[string]any{"messages": anys(
				raw("2.000000", "root a", "thread_ts", "2.000000", "reply_count", 2),
				raw("2.500000", "a1", "thread_ts", "2.000000"),
				raw("9.000000", "a2", "thread_ts", "2.000000"),
			)}, nil
		case "4.000000":
			return map[string]any{"messages": anys(
				raw("4.000000", "root b", "thread_ts", "4.000000", "reply_count", 1),
				raw("4.500000", "b1", "thread_ts", "4.000000"),
			)}, nil
		}
		return apiError("thread_not_found"), nil
	}}
}

func topLevel() []Message {
	return []Message{
		{ChannelID: "C12345678", TS: "1.000000", Text: "one"},
		{ChannelID: "C12345678", TS: "2.000000", ThreadTS: "2.000000", ReplyCount: 2, Text: "root a"},
		{ChannelID: "C12345678", TS: "3.000000", Text: "three"},
		{ChannelID: "C12345678", TS: "4.000000", ThreadTS: "4.000000", ReplyCount: 1, Text: "root b"},
	}
}

// Each thread's replies follow its root, whenever they were posted.
func TestExpandThreads(t *testing.T) {
	rec := threadsFake(nil)
	got, err := ExpandThreads(context.Background(), rec.client(), "C12345678", topLevel(), false)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"1.000000", "2.000000", "2.500000", "9.000000", "3.000000", "4.000000", "4.500000"}
	if !reflect.DeepEqual(tsList(got), want) {
		t.Errorf("messages = %v, want %v", tsList(got), want)
	}
	for _, m := range got[1:4] {
		if m.ThreadTS != "2.000000" || m.ChannelID != "C12345678" {
			t.Errorf("message %s: thread_ts = %q, channel = %q", m.TS, m.ThreadTS, m.ChannelID)
		}
	}
	if got[1].ReplyCount != 2 {
		t.Errorf("the root lost its reply_count: %+v", got[1])
	}
	if n := len(rec.only("conversations.replies")); n != 2 {
		t.Errorf("%d conversations.replies calls, want one per thread", n)
	}

	// Nothing to expand: no call at all.
	rec = threadsFake(nil)
	plain := []Message{{TS: "1.000000"}, {TS: "3.000000"}}
	if got, err := ExpandThreads(context.Background(), rec.client(), "C12345678", plain, false); err != nil || !reflect.DeepEqual(got, plain) || len(rec.calls) != 0 {
		t.Errorf("no threads: %v, %v, calls = %+v", got, err, rec.calls)
	}
}

// The replies are an enrichment: a rate limit leaves the roots as they
// are and says so, dead credentials fail the command.
func TestExpandThreadsDegradesWhenRateLimited(t *testing.T) {
	limit := &slackapi.RateLimitError{Method: "conversations.replies", RetryAfter: 30 * time.Second}
	rec := threadsFake(map[string]any{"2.000000": limit, "4.000000": limit})
	ctx, warnings := enrich.Collect(context.Background())
	got, err := ExpandThreads(ctx, rec.client(), "C12345678", topLevel(), false)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, topLevel()) {
		t.Errorf("messages = %+v, want the top-level messages untouched", got)
	}
	list := warnings.List()
	if len(list) != 1 || !strings.Contains(list[0], "the replies of 2 of 2 threads could not be fetched") || !strings.Contains(list[0], "reply_count") {
		t.Errorf("warnings = %q", list)
	}

	rec = threadsFake(map[string]any{"2.000000": "invalid_auth"})
	if _, err := ExpandThreads(context.Background(), rec.client(), "C12345678", topLevel(), false); !slackapi.IsErrorCode(err, "invalid_auth") {
		t.Errorf("err = %v, want invalid_auth", err)
	}
}

func TestFormatTime(t *testing.T) {
	paris, err := time.LoadLocation("Europe/Paris")
	if err != nil {
		t.Skip(err)
	}
	for _, tc := range []struct {
		ts   string
		loc  *time.Location
		want string
	}{
		{"1770165109.628379", time.UTC, "2026-02-04T00:31:49Z"},
		{"1770165109.628379", paris, "2026-02-04T01:31:49+01:00"},
		{" 1770165109 ", time.UTC, "2026-02-04T00:31:49Z"},
		{"", time.UTC, ""},
		{"not-a-ts", time.UTC, ""},
		{"0.000000", time.UTC, ""},
	} {
		if got := FormatTime(tc.ts, tc.loc); got != tc.want {
			t.Errorf("FormatTime(%q, %v) = %q, want %q", tc.ts, tc.loc, got, tc.want)
		}
	}
	// nil is the local time zone.
	if got, want := FormatTime("1770165109.628379", nil), time.Unix(1770165109, 0).Format(time.RFC3339); got != want {
		t.Errorf("FormatTime(nil) = %q, want %q", got, want)
	}
}

// A thread root says how many replies it has, and every message when it
// was posted.
func TestToCompactTimeAndReplyCount(t *testing.T) {
	root := ToCompact(Message{ChannelID: "C1", TS: "1770165109.628379", ReplyCount: 3}, CompactOptions{Location: time.UTC})
	if root.Time != "2026-02-04T00:31:49Z" || root.ReplyCount != 3 || root.ThreadTS != "1770165109.628379" {
		t.Errorf("root = %+v", root)
	}
	reply := ToCompact(Message{ChannelID: "C1", TS: "1770165200.000100", ThreadTS: "1770165109.628379"}, CompactOptions{Location: time.UTC})
	if reply.Time != "2026-02-04T00:33:20Z" || reply.ReplyCount != 0 {
		t.Errorf("reply = %+v", reply)
	}
}
