package composecmd

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/maelvls/slackcli/internal/appinfo"
	"github.com/maelvls/slackcli/internal/slack/enrich"
)

// The result is printed in the order the help gives, not with its keys
// sorted as a map would be.
func TestResultIsPrintedInTheDocumentedOrder(t *testing.T) {
	keyOrder := func(t *testing.T, doc string, keys ...string) {
		t.Helper()
		at := -1
		for _, k := range keys {
			i := strings.Index(doc, `"`+k+`":`)
			if i <= at {
				t.Fatalf("%q is missing or out of order, want %v in:\n%s", k, keys, doc)
			}
			at = i
		}
		if n := strings.Count(doc, `":`); n < len(keys) {
			t.Errorf("%d keys printed, want %d: %s", n, len(keys), doc)
		}
	}

	t.Run("CI", func(t *testing.T) {
		h := newHarness(t)
		t.Setenv("CI", "1")
		h.mustRun("C12345678", "hello", "--thread-ts", "1770160500.000100")
		keyOrder(t, h.stdout.String(), "ok", "sent", "editor", "channel_id", "ts", "thread_ts", "permalink")
		for _, absent := range []string{"cancelled", "reason", "text"} {
			if strings.Contains(h.stdout.String(), `"`+absent+`"`) {
				t.Errorf("%q is printed: %s", absent, h.stdout)
			}
		}
	})
	t.Run("sent from the editor", func(t *testing.T) {
		h := newHarness(t)
		h.human = sends("reviewed")
		h.mustRun("#general", "hello")
		keyOrder(t, h.stdout.String(), "ok", "sent", "channel_id", "ts", "permalink", "text")
	})
	t.Run("cancelled", func(t *testing.T) {
		h := newHarness(t)
		h.human = cancels
		h.mustRun("#general", "hello")
		keyOrder(t, h.stdout.String(), "ok", "cancelled", "reason")
		if strings.Contains(h.stdout.String(), `"sent"`) {
			t.Errorf(`"sent" is printed for a cancelled message: %s`, h.stdout)
		}
	})
}

// A token from SLACK_TOKEN names no workspace, so the permalink cannot be
// built: Slack is asked for it, as "message send" does. The message is
// sent by then: a permalink that cannot be fetched costs a warning, never
// the command, and never a second post.
func TestPermalinkWithoutAWorkspace(t *testing.T) {
	const permalink = "https://real.slack.com/archives/C12345678/p1770170000000200"
	for _, tc := range []struct {
		name    string
		answer  func() (map[string]any, error)
		want    string
		warning string
	}{
		{"Slack gives it", func() (map[string]any, error) { return map[string]any{"ok": true, "permalink": permalink}, nil }, permalink, ""},
		{"Slack refuses", func() (map[string]any, error) { return map[string]any{"ok": false, "error": "channel_not_found"}, nil }, "", "channel_not_found"},
		// An auth error must not have the send retried with fresh
		// credentials: that would post twice.
		{"auth error", func() (map[string]any, error) { return map[string]any{"ok": false, "error": "invalid_auth"}, nil }, "", "invalid_auth"},
		{"network error", func() (map[string]any, error) { return nil, errors.New("connection reset") }, "", "connection reset"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			t.Setenv("SLACK_TOKEN", "xoxb-env")
			t.Setenv("CI", "1")
			h.getPermalink = tc.answer
			ctx, warnings := enrich.Collect(context.Background())
			out, err := h.c.compose(ctx, "C12345678", "hello", Options{})
			if err != nil {
				t.Fatalf("the send failed: %v", err)
			}
			if want := (Result{OK: true, Sent: true, Editor: "skipped", ChannelID: "C12345678", TS: "1770170000.000200", Permalink: tc.want}); out != want {
				t.Errorf("result = %+v\nwant     %+v", out, want)
			}
			h.posted() // exactly one chat.postMessage
			calls := h.callsTo("chat.getPermalink")
			if len(calls) != 1 || calls[0].params["channel"] != "C12345678" || calls[0].params["message_ts"] != "1770170000.000200" {
				t.Errorf("chat.getPermalink calls = %v", calls)
			}
			list := warnings.List()
			switch {
			case tc.warning == "" && len(list) != 0:
				t.Errorf("warnings = %q", list)
			case tc.warning != "" && (len(list) != 1 || !strings.Contains(list[0], tc.warning) || !strings.Contains(list[0], "the message was sent") || !strings.Contains(list[0], "Do not send it again")):
				t.Errorf("warnings = %q", list)
			}
		})
	}

	// A workspace that is known is not asked about.
	h := newHarness(t)
	t.Setenv("CI", "1")
	out := h.mustRun("C12345678", "hello")
	if out["permalink"] != workspaceURL+"/archives/C12345678/p1770170000000200" || len(h.callsTo("chat.getPermalink")) != 0 {
		t.Errorf("output = %v, chat.getPermalink calls = %v", out, h.callsTo("chat.getPermalink"))
	}
}

// The refusals of safe mode name what turned it on, as every other
// command's do, rather than list all the ways it can be.
func TestSafeModeErrorsNameTheirSource(t *testing.T) {
	env := appinfo.EnvName("SAFE_MODE")
	for _, tc := range []struct {
		name   string
		enable func(h *harness)
		want   string
		absent string
	}{
		{"flag", func(h *harness) { h.rt.SafeModeFlag = true }, "safe mode is active (turned on by --safe-mode) and", env},
		{"environment", func(h *harness) { h.t.Setenv(env, "1") }, "safe mode is active (turned on by " + env + ") and", "--safe-mode"},
	} {
		t.Run(tc.name+" in CI", func(t *testing.T) {
			h := newHarness(t)
			t.Setenv("CI", "1")
			tc.enable(h)
			_, err := h.run("#general", "hello")
			if err == nil || !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), tc.absent) {
				t.Errorf("err = %v\nwant it to contain %q and not %q", err, tc.want, tc.absent)
			}
			h.wantNoSlackCalls()
		})
		t.Run(tc.name+" without a browser", func(t *testing.T) {
			h := newHarness(t)
			tc.enable(h)
			h.openErr = errors.New("no browser")
			_, err := h.run("#general", "hello")
			if err == nil || !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), tc.absent) {
				t.Errorf("err = %v\nwant it to contain %q and not %q", err, tc.want, tc.absent)
			}
			h.wantNothingPosted()
		})
	}
}

// When no browser could be opened, stderr must not go on to say that the
// editor is open in one: the human has to open the printed link.
func TestNoBrowserHintIsTruthful(t *testing.T) {
	h := newHarness(t)
	h.openErr = errors.New("no graphical session")
	h.c.humanAtStderr = func() bool { return true }
	h.human = cancels
	terminal := &terminalReader{h: h}
	h.rt.Stderr = terminal

	h.mustRun("#general", "hello")
	stderr := terminal.String()
	if strings.Contains(stderr, "is open in the browser") {
		t.Errorf("stderr claims that a browser shows the editor: %q", stderr)
	}
	for _, want := range []string{"Could not open a browser (no graphical session)", "Open this one-time link yourself", "Compose editor for #general is not open yet: it waits for a human to open the link above"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr does not contain %q: %q", want, stderr)
		}
	}
}
