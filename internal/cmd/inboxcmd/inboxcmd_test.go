package inboxcmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/maelvls/slackcli/internal/auth"
	"github.com/maelvls/slackcli/internal/clictx"
	"github.com/maelvls/slackcli/internal/slackapi"
	"github.com/maelvls/slackcli/internal/testenv"
)

// harness runs the command against a fake Slack and a temporary
// credentials store holding the browser-auth workspace acme.slack.com.
type harness struct {
	rt     *clictx.Context
	stdout *bytes.Buffer

	mu      sync.Mutex
	methods []string
	queries []string
}

func newHarness(t *testing.T, serve func(method string, params map[string]any) map[string]any) *harness {
	t.Helper()
	testenv.Isolate(t)
	for _, env := range []string{"SLACK_TOKEN", "SLACK_COOKIE_D", "SLACK_COOKIE", "SLACK_WORKSPACE_URL"} {
		t.Setenv(env, "")
	}
	h := &harness{stdout: &bytes.Buffer{}}
	store := &auth.Store{Path: filepath.Join(t.TempDir(), "credentials.json")}
	if err := store.Upsert(auth.Workspace{
		URL:  "https://acme.slack.com",
		Auth: slackapi.Auth{Type: slackapi.AuthBrowser, XoxcToken: "xoxc-test", XoxdCookie: "xoxd-test"},
	}); err != nil {
		t.Fatal(err)
	}
	h.rt = &clictx.Context{
		Store:  store,
		Stdout: h.stdout,
		Stderr: io.Discard,
		ClientOptions: []slackapi.Option{slackapi.WithHandler(func(_ context.Context, method string, params map[string]any) (map[string]any, error) {
			h.mu.Lock()
			h.methods = append(h.methods, method)
			if q, ok := params["query"].(string); ok {
				h.queries = append(h.queries, q)
			}
			h.mu.Unlock()
			return serve(method, params), nil
		})},
	}
	return h
}

func (h *harness) run(args ...string) error {
	h.stdout.Reset()
	h.methods, h.queries = nil, nil
	cmd := New(h.rt)
	cmd.SetArgs(args)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	return cmd.ExecuteContext(context.Background())
}

// tsAgo is the ts of a message posted that long ago.
func tsAgo(d time.Duration) string {
	micro := time.Now().Add(-d).UnixMicro()
	return fmt.Sprintf("%d.%06d", micro/1e6, micro%1e6)
}

// serveInbox is a workspace where one DM waits: alice wrote an hour ago.
func serveInbox(dmTS string) func(method string, params map[string]any) map[string]any {
	return func(method string, params map[string]any) map[string]any {
		switch method {
		case "auth.test":
			return map[string]any{"user_id": "U000000001", "url": "https://acme.slack.com/"}
		case "search.messages":
			var matches []any
			if strings.HasPrefix(params["query"].(string), "to:") {
				matches = append(matches, map[string]any{
					"channel": map[string]any{"id": "D000000001", "is_im": true, "name": "U000000009"},
					"user":    "U000000009", "ts": dmTS,
					"permalink": "https://acme.slack.com/archives/D000000001/p" + strings.ReplaceAll(dmTS, ".", ""),
				})
			}
			return map[string]any{"messages": map[string]any{"matches": matches, "paging": map[string]any{"pages": 1.0}}}
		case "conversations.history":
			return map[string]any{"messages": []any{map[string]any{"ts": dmTS, "user": "U000000009", "text": "are you there? it has been a while"}}}
		case "users.info":
			return map[string]any{"user": map[string]any{"id": params["user"], "name": "alice", "profile": map[string]any{"display_name": "Alice"}}}
		}
		return map[string]any{"ok": false, "error": "unknown_method"}
	}
}

func TestInbox(t *testing.T) {
	dmTS := tsAgo(time.Hour)
	h := newHarness(t, serveInbox(dmTS))
	before := time.Now().Add(-3 * 24 * time.Hour)
	if err := h.run(); err != nil {
		t.Fatal(err)
	}

	// The shape is a contract: the order of the keys, "truncated" even when
	// false, and no thread_ts for the top level of a DM.
	var out struct {
		Since string `json:"since"`
	}
	if err := json.Unmarshal(h.stdout.Bytes(), &out); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, h.stdout)
	}
	want := `{"user_id":"U000000001","since":"` + out.Since + `","truncated":false,"items":[{"kind":"dm","channel_id":"D000000001","channel_name":"Alice",` +
		`"ts":"` + dmTS + `","first_ts":"` + dmTS + `","count":1,"author":{"user_id":"U000000009","name":"Alice"},"text":"are you there? it has been a while",` +
		`"permalink":"https://acme.slack.com/archives/D000000001/p` + strings.ReplaceAll(dmTS, ".", "") + `"}]}` + "\n"
	if got := h.stdout.String(); got != want {
		t.Errorf("printed:\n%s\nwant:\n%s", got, want)
	}
	// The window is the last 3 days, in UTC with milliseconds.
	since, err := time.Parse("2006-01-02T15:04:05.000Z", out.Since)
	if err != nil || since.Before(before.Truncate(time.Millisecond)) || since.After(time.Now().Add(-3*24*time.Hour)) {
		t.Errorf("since = %q (%v), want 3 days ago", out.Since, err)
	}
	if len(h.queries) != 3 {
		t.Errorf("queries = %q", h.queries)
	}
}

func TestInboxFlags(t *testing.T) {
	dmTS := tsAgo(time.Hour)
	h := newHarness(t, serveInbox(dmTS))

	// --since leaves the DM out of the window; the list is still printed.
	if err := h.run("--since", "1h", "--no-threads", "--workspace", "acme"); err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(h.stdout.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if items, ok := out["items"].([]any); !ok || len(items) != 0 || out["truncated"] != false {
		t.Errorf("printed %s", h.stdout)
	}
	if len(h.queries) != 2 || slices.ContainsFunc(h.queries, func(q string) bool { return strings.HasPrefix(q, "is:thread") }) {
		t.Errorf("queries with --no-threads = %q", h.queries)
	}
	if slices.Contains(h.methods, "conversations.history") {
		t.Errorf("a conversation outside the window was read: %v", h.methods)
	}

	if err := h.run("--since", "2d", "--max-body-chars", "7", "--max-conversations", "1"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(h.stdout.String(), `"text":"are you..."`) {
		t.Errorf("printed %s", h.stdout)
	}
}

// What cannot be honoured is refused before anything is asked of Slack.
func TestInboxRejectsBadFlags(t *testing.T) {
	h := newHarness(t, serveInbox(tsAgo(time.Hour)))
	for want, args := range map[string][]string{
		`invalid --since value "yesterday"`:   {"--since", "yesterday"},
		"invalid --max-body-chars value 0":    {"--max-body-chars", "0"},
		"invalid --max-conversations value 0": {"--max-conversations", "0"},
		"unknown command":                     {"extra"},
	} {
		err := h.run(args...)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("inbox %s: err = %v, want %q", strings.Join(args, " "), err, want)
		}
		if len(h.methods) != 0 || h.stdout.Len() != 0 {
			t.Errorf("inbox %s: calls = %v, printed %q", strings.Join(args, " "), h.methods, h.stdout)
		}
	}
}

// The inbox only reads: a write would show here.
func TestInboxOnlyReads(t *testing.T) {
	h := newHarness(t, serveInbox(tsAgo(time.Hour)))
	if err := h.run(); err != nil {
		t.Fatal(err)
	}
	for _, m := range h.methods {
		if !slices.Contains([]string{"auth.test", "search.messages", "conversations.history", "conversations.replies", "users.info"}, m) {
			t.Errorf("unexpected call %s", m)
		}
	}
}
