package messagecmd

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/maelvls/slackcli/internal/cmd/cmdutil"
	"github.com/maelvls/slackcli/internal/slack/enrich"
)

// runCollecting is harness.run with the warnings of optional lookups
// collected, as the root command does.
func (h *harness) runCollecting(args ...string) (map[string]any, []string, error) {
	h.t.Helper()
	h.stdout.Reset()
	h.stderr.Reset()
	h.mu.Lock()
	h.calls = nil
	h.mu.Unlock()
	cmd := h.command()
	cmd.SetArgs(args)
	ctx, warnings := enrich.Collect(context.Background())
	if err := cmd.ExecuteContext(ctx); err != nil {
		return nil, warnings.List(), err
	}
	var out map[string]any
	if err := json.Unmarshal(h.stdout.Bytes(), &out); err != nil {
		h.t.Fatalf("output is not JSON: %v\n%s", err, h.stdout)
	}
	return out, warnings.List(), nil
}

// A token from SLACK_TOKEN names no workspace, so the permalink cannot be
// built: Slack is asked for it.
func TestSendPermalinkWithoutAWorkspace(t *testing.T) {
	h := newHarness(t)
	t.Setenv("SLACK_TOKEN", "xoxb-env")
	h.fixtures["chat.getPermalink"] = func(p map[string]any) (map[string]any, error) {
		return map[string]any{"ok": true, "permalink": "https://real.slack.com/archives/" + p["channel"].(string) + "/p1770165109628379"}, nil
	}
	out, warnings, err := h.runCollecting("send", testChannel, "hi")
	if err != nil || len(warnings) != 0 {
		t.Fatalf("err = %v, warnings = %q", err, warnings)
	}
	if out["permalink"] != "https://real.slack.com/archives/C12345678/p1770165109628379" || out["ts"] != testTS {
		t.Errorf("output = %v", out)
	}
	h.wantCalls(
		call{"chat.postMessage", h.callsTo("chat.postMessage")[0].params},
		call{"chat.getPermalink", map[string]any{"channel": testChannel, "message_ts": testTS}},
	)

	// With a workspace, whether a URL or the name of a stored one, it is
	// built without a call; both give the same.
	for _, workspace := range []string{testWorkspace, "workspace"} {
		out, _, err := h.runCollecting("send", testChannel, "hi", "--workspace", workspace)
		if err != nil || out["permalink"] != testURL || len(h.callsTo("chat.getPermalink")) != 0 {
			t.Errorf("--workspace %s: output = %v, err = %v, calls = %v", workspace, out, err, h.methods())
		}
	}

	// A workspace that designates nothing is refused before anything is
	// sent, instead of being ignored.
	for _, workspace := range []string{"https://evil.example.com", "acme"} {
		_, _, err := h.runCollecting("send", testChannel, "hi", "--workspace", workspace)
		if err == nil || !strings.Contains(err.Error(), "--workspace must be an https Slack workspace URL") || len(h.methods()) != 0 {
			t.Errorf("--workspace %s: err = %v, calls = %v", workspace, err, h.methods())
		}
	}
	t.Setenv("SLACK_WORKSPACE_URL", "nonsense")
	if _, _, err := h.runCollecting("send", testChannel, "hi"); err == nil || !strings.Contains(err.Error(), "SLACK_WORKSPACE_URL must be an https Slack workspace URL") || len(h.methods()) != 0 {
		t.Errorf("SLACK_WORKSPACE_URL=nonsense: err = %v, calls = %v", err, h.methods())
	}
}

// The message is sent: a permalink that cannot be fetched costs a warning,
// never the command, whatever the failure.
func TestSendSucceedsWithoutItsPermalink(t *testing.T) {
	for name, failure := range map[string]error{
		"Slack error":  apiError("chat.getPermalink", "channel_not_found"),
		"auth error":   apiError("chat.getPermalink", "invalid_auth"),
		"other error":  errors.New("connection reset"),
		"cancellation": context.Canceled,
	} {
		h := newHarness(t)
		t.Setenv("SLACK_TOKEN", "xoxb-env")
		h.fixtures["chat.getPermalink"] = func(map[string]any) (map[string]any, error) { return nil, failure }
		out, warnings, err := h.runCollecting("send", testChannel, "hi")
		if err != nil {
			t.Errorf("%s: the send failed: %v", name, err)
			continue
		}
		if _, has := out["permalink"]; has || out["ts"] != testTS || out["ok"] != true {
			t.Errorf("%s: output = %v", name, out)
		}
		if len(warnings) != 1 || !strings.Contains(warnings[0], "the message was sent (ts "+testTS+" in "+testChannel+"), but its permalink could not be fetched") ||
			!strings.Contains(warnings[0], "Do not send it again") {
			t.Errorf("%s: warnings = %q", name, warnings)
		}
		if n := len(h.callsTo("chat.postMessage")); n != 1 {
			t.Errorf("%s: %d chat.postMessage calls, want 1", name, n)
		}
	}
}

// "send" without text says so in the words of its usage line and, when
// flags were given, that one of them may be the text.
func TestSendWithoutTextMentionsDashText(t *testing.T) {
	h := newHarness(t)
	err := h.mustFail("[text] is required unless --attach or --blocks is given", "send", testChannel)
	if strings.Contains(err.Error(), `"--"`) {
		t.Errorf("no flag was given, yet the error talks about flags: %v", err)
	}
	h.mustFail(`if the text is a single word starting with a dash, which is read as a flag, put it after "--": message send <target> -- "-1"`,
		"send", testChannel, "--no-unfurl")
	h.wantCalls()
}

// A single word starting with a dash is read as a flag; the error says to
// put it after "--", which works.
func TestSendDashWordNeedsTheTerminator(t *testing.T) {
	h := newHarness(t)
	err := h.mustFail("unknown shorthand flag: '1' in -1", "send", testChannel, "-1")
	var usage *cmdutil.UsageError
	if !errors.As(err, &usage) || !strings.Contains(usage.Message(), `put it after "--" so that it is not read as a flag: message send <target> -- "-1"`) {
		t.Errorf("err = %v, want a usage error saying to use --", err)
	}
	h.wantCalls()

	for _, text := range []string{"-1", "-h", "--safe-mode"} {
		h.mustRun("send", testChannel, "--", text)
		if got := h.callsTo("chat.postMessage"); len(got) != 1 || got[0].params["text"] != text {
			t.Errorf("-- %s: chat.postMessage calls = %v", text, got)
		}
	}
}

func TestListRejectsContradictions(t *testing.T) {
	const other = "1770160000.000001"
	for _, tt := range []struct {
		name string
		args []string
		want string
	}{
		{"inverted bounds", []string{testChannel, "--oldest", "1770165109", "--latest", "1770165100"}, "--oldest 1770165109 is not before --latest 1770165100"},
		{"equal bounds", []string{testChannel, "--oldest", "1770165109.000100", "--latest", "1770165109.000100"}, "is not before --latest"},
		{"--ts and another --thread-ts", []string{testChannel, "--thread-ts", other, "--ts", testTS},
			"--ts " + testTS + " and --thread-ts " + other + " both say which thread to list: pass only one of them"},
		{"invalid --ts next to --thread-ts", []string{testChannel, "--thread-ts", other, "--ts", "nope"}, "invalid --ts"},
		{"--thread-ts against the thread of the URL", []string{testURL + "?thread_ts=" + other + "&cid=" + testChannel, "--thread-ts", testTS},
			"--thread-ts " + testTS + " contradicts the message URL, whose thread is " + other},
	} {
		h := newHarness(t)
		h.mustFail(tt.want, append([]string{"list"}, tt.args...)...)
		if len(h.methods()) != 0 {
			t.Errorf("%s: API calls %v", tt.name, h.methods())
		}
	}

	// What does not contradict still works.
	h := newHarness(t)
	h.fixtures["conversations.replies"] = func(map[string]any) (map[string]any, error) {
		return map[string]any{"ok": true, "messages": []any{map[string]any{"ts": testTS, "text": "x"}}}, nil
	}
	for _, args := range [][]string{
		{testChannel, "--thread-ts", testTS, "--ts", testTS},
		{testURL + "?thread_ts=" + other + "&cid=" + testChannel, "--thread-ts", other},
		{testURL, "--thread-ts", other},
	} {
		h.mustRun(append([]string{"list"}, args...)...)
	}
	h.fixtures["conversations.history"] = func(map[string]any) (map[string]any, error) {
		return map[string]any{"ok": true, "messages": []any{}}, nil
	}
	h.mustRun("list", testChannel, "--oldest", "1770165100", "--latest", "1770165109")
}

func TestBoundBefore(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{"1770165100", "1770165109", true},
		{"1770165109.000100", "1770165109.000200", true},
		{"1770165109", "1770165109.000001", true},
		{"1770165109", "1770165109", false},
		{"1770165109.628379", "1770165109", false},
	} {
		if got := boundBefore(tc.a, tc.b); got != tc.want {
			t.Errorf("boundBefore(%s, %s) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}
