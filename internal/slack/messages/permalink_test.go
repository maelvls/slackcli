package messages

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/maelvls/slackcli/internal/slack/enrich"
	"github.com/maelvls/slackcli/internal/slackapi"
)

func TestSentPermalink(t *testing.T) {
	const channel, ts, thread = "C12345678", "1770165109.628379", "1770160000.000001"
	var calls []map[string]any
	var failure error
	client := slackapi.NewFake(func(_ context.Context, method string, params map[string]any) (map[string]any, error) {
		if method != "chat.getPermalink" {
			t.Errorf("unexpected call to %s", method)
		}
		calls = append(calls, params)
		if failure != nil {
			return nil, failure
		}
		return map[string]any{"ok": true, "permalink": "https://real.slack.com/archives/C12345678/p1770165109628379"}, nil
	})
	ctx, warnings := enrich.Collect(context.Background())

	// With a workspace URL the permalink is built, without a call.
	got := SentPermalink(ctx, client, "https://acme.slack.com", channel, ts, thread)
	if want := "https://acme.slack.com/archives/C12345678/p1770165109628379?thread_ts=1770160000.000001&cid=C12345678"; got != want {
		t.Errorf("built permalink = %q, want %q", got, want)
	}
	if got := SentPermalink(ctx, client, "", channel, "", ""); got != "" || len(calls) != 0 {
		t.Errorf("no ts: permalink = %q, calls = %v", got, calls)
	}

	// Without one, Slack is asked.
	if got := SentPermalink(ctx, client, "", channel, ts, ""); got != "https://real.slack.com/archives/C12345678/p1770165109628379" {
		t.Errorf("fetched permalink = %q", got)
	}
	if want := []map[string]any{{"channel": channel, "message_ts": ts}}; !reflect.DeepEqual(calls, want) {
		t.Errorf("calls = %v, want %v", calls, want)
	}
	if list := warnings.List(); len(list) != 0 {
		t.Errorf("warnings = %q", list)
	}

	// Whatever the failure, it is a warning and no permalink.
	for _, failure = range []error{
		&slackapi.APIError{Method: "chat.getPermalink", Code: "invalid_auth"},
		errors.New("connection reset"),
		context.Canceled,
	} {
		ctx, warnings := enrich.Collect(context.Background())
		if got := SentPermalink(ctx, client, "", channel, ts, ""); got != "" {
			t.Errorf("%v: permalink = %q", failure, got)
		}
		list := warnings.List()
		if len(list) != 1 || !strings.Contains(list[0], "the message was sent (ts "+ts+" in "+channel+")") || !strings.Contains(list[0], "Do not send it again") {
			t.Errorf("%v: warnings = %q", failure, list)
		}
	}
}
