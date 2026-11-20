package watchcmd

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/websocket"

	"github.com/maelvls/slackcli/internal/auth"
	"github.com/maelvls/slackcli/internal/authextract"
	"github.com/maelvls/slackcli/internal/clictx"
	"github.com/maelvls/slackcli/internal/slack/channels"
	"github.com/maelvls/slackcli/internal/slack/enrich"
	"github.com/maelvls/slackcli/internal/slack/messages"
	"github.com/maelvls/slackcli/internal/slack/rtm"
	"github.com/maelvls/slackcli/internal/slackapi"
	"github.com/maelvls/slackcli/internal/testenv"
)

// events is what the fake Slack sends after hello.
var events = []string{
	`{"type":"message","channel":"C000000001","channel_type":"channel","user":"U000000001","text":"hello","ts":"1700000001.000100"}`,
	// Not new messages.
	`{"type":"message","subtype":"message_changed","hidden":true,"channel":"C000000001","ts":"1700000002.000100"}`,
	`{"type":"message","subtype":"message_replied","channel":"C000000001","ts":"1700000002.000200"}`,
	`{"type":"reaction_added","user":"U000000001","reaction":"+1"}`,
	`{"type":"message","subtype":"channel_join","channel":"C000000001","user":"U000000002","text":"joined","ts":"1700000003.000100"}`,
	// A thread reply, by a user Slack has no profile for.
	`{"type":"message","channel":"C000000001","user":"U000000002","text":"a reply\non two lines","ts":"1700000004.000100","thread_ts":"1700000001.000100"}`,
	`{"type":"message","subtype":"bot_message","channel":"C000000002","bot_id":"B000000001","username":"prow","text":"job failed","ts":"1700000005.000100"}`,
	`{"type":"message","channel":"D000000001","user":"U000000001","text":"psst","ts":"1700000006.000100"}`,
}

// harness runs the command against a fake Slack: API calls are served by
// serveAPI, and the WebSocket sends hello and events.
type harness struct {
	t  *testing.T
	rt *clictx.Context

	mu    sync.Mutex
	calls []string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	for _, env := range []string{"SLACK_TOKEN", "SLACK_COOKIE_D", "SLACK_COOKIE", "SLACK_WORKSPACE_URL"} {
		t.Setenv(env, "")
	}
	// The user cache must not be the developer's.
	testenv.Isolate(t)
	streamOptions = rtm.Options{PingInterval: 10 * time.Millisecond, ReadTimeout: time.Second}
	t.Cleanup(func() { streamOptions = rtm.Options{} })

	server := httptest.NewServer(websocket.Handler(func(ws *websocket.Conn) {
		for _, e := range append([]string{`{"type":"hello"}`}, events...) {
			_ = websocket.Message.Send(ws, e)
		}
		_, _ = io.Copy(io.Discard, ws) // until the client leaves
	}))
	t.Cleanup(server.Close)

	h := &harness{t: t}
	store := &auth.Store{Path: filepath.Join(t.TempDir(), "credentials.json")}
	if err := store.Upsert(auth.Workspace{
		URL:  "https://acme.slack.com",
		Auth: slackapi.Auth{Type: slackapi.AuthBrowser, XoxcToken: "xoxc-test", XoxdCookie: "xoxd-test"},
	}); err != nil {
		t.Fatal(err)
	}
	h.rt = &clictx.Context{
		Store:  store,
		Stderr: io.Discard,
		ClientOptions: []slackapi.Option{
			slackapi.WithLoopbackWorkspaceForTesting(),
			slackapi.WithHandler(func(_ context.Context, method string, params map[string]any) (map[string]any, error) {
				h.mu.Lock()
				h.calls = append(h.calls, method)
				h.mu.Unlock()
				switch method {
				case "rtm.connect":
					return map[string]any{"url": "ws" + strings.TrimPrefix(server.URL, "http")}, nil
				case "conversations.info":
					switch params["channel"] {
					case "C000000001":
						return map[string]any{"channel": map[string]any{"name": "general"}}, nil
					case "D000000001":
						return map[string]any{"channel": map[string]any{"is_im": true, "user": "U000000001"}}, nil
					}
					return map[string]any{"ok": false, "error": "channel_not_found"}, nil
				case "users.info":
					if params["user"] == "U000000001" {
						return map[string]any{"user": map[string]any{"id": "U000000001", "name": "alice", "profile": map[string]any{"display_name": "Alice"}}}, nil
					}
					return map[string]any{"ok": false, "error": "user_not_found"}, nil
				}
				return map[string]any{"ok": false, "error": "unknown_method"}, nil
			}),
		},
	}
	return h
}

// lineWriter collects what the command prints and calls done once it has
// printed want lines.
type lineWriter struct {
	mu   sync.Mutex
	buf  bytes.Buffer
	want int
	done func()
}

func (w *lineWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf.Write(p)
	if bytes.Count(w.buf.Bytes(), []byte("\n")) >= w.want {
		w.done()
	}
	return len(p), nil
}

// run executes "watch <args>" until it has printed want lines (the fake
// Slack then has nothing more to send), and returns them.
func (h *harness) run(want int, args ...string) []string {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out := &lineWriter{want: want, done: cancel}
	h.rt.Stdout = out
	h.calls = nil
	cmd := New(h.rt)
	cmd.SetArgs(args)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	if err := cmd.ExecuteContext(ctx); err != nil {
		h.t.Fatalf("watch %s: %v", strings.Join(args, " "), err)
	}
	lines := strings.Split(strings.TrimSuffix(out.buf.String(), "\n"), "\n")
	if len(lines) != want {
		h.t.Fatalf("watch %s printed %d lines, want %d:\n%s", strings.Join(args, " "), len(lines), want, out.buf.String())
	}
	return lines
}

func localTime(sec int64) string { return time.Unix(sec, 0).Format(time.RFC3339) }

func TestWatch(t *testing.T) {
	h := newHarness(t)
	lines := h.run(4)
	var got []map[string]any
	for _, line := range lines {
		var ev map[string]any
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("not one JSON object per line: %q", line)
		}
		got = append(got, ev)
	}
	want := []map[string]any{
		{"channel_id": "C000000001", "channel_name": "general", "channel_type": "channel", "ts": "1700000001.000100", "time": localTime(1700000001),
			"author": map[string]any{"user_id": "U000000001", "name": "Alice"}, "content": "hello"},
		{"channel_id": "C000000001", "channel_name": "general", "ts": "1700000004.000100", "time": localTime(1700000004),
			"author": map[string]any{"user_id": "U000000002"}, "content": "a reply\non two lines", "thread_ts": "1700000001.000100"},
		{"channel_id": "C000000002", "ts": "1700000005.000100", "time": localTime(1700000005),
			"author": map[string]any{"bot_id": "B000000001", "name": "prow"}, "content": "job failed", "subtype": "bot_message"},
		{"channel_id": "D000000001", "channel_name": "Alice", "channel_type": "dm", "ts": "1700000006.000100", "time": localTime(1700000006),
			"author": map[string]any{"user_id": "U000000001", "name": "Alice"}, "content": "psst"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("printed\n%s", strings.Join(lines, "\n"))
	}

	// A name is asked for once, and nothing is marked as read.
	count := map[string]int{}
	for _, c := range h.calls {
		count[c]++
	}
	// conversations.info: general (two messages, one lookup), the unknown
	// C000000002 and the DM.
	if count["rtm.connect"] != 1 || count["conversations.info"] != 3 || count["conversations.mark"] != 0 {
		t.Errorf("API calls = %v", count)
	}
}

func TestWatchTextAndFilters(t *testing.T) {
	h := newHarness(t)

	lines := h.run(6, "--text", "--max-body-chars", "7")
	clock := func(sec int64) string { return time.Unix(sec, 0).Format(time.TimeOnly) }
	want := []string{
		clock(1700000001) + " #general Alice: hello",
		clock(1700000004) + " #general (thread) U000000002: a reply",
		"    ...",
		clock(1700000005) + " #C000000002 prow: job fai",
		"    ...",
		clock(1700000006) + " @Alice Alice: psst",
	}
	if !reflect.DeepEqual(lines, want) {
		t.Errorf("--text printed\n%s", strings.Join(lines, "\n"))
	}

	lines = h.run(1, "--text", "--channel", "D000000001")
	if want := clock(1700000006) + " @Alice Alice: psst"; lines[0] != want {
		t.Errorf("--channel printed %q, want %q", lines[0], want)
	}

	lines = h.run(4, "--text", "--include-system", "--channel", "C000000001,C000000002")
	if want := clock(1700000003) + " #general U000000002: joined"; lines[1] != want {
		t.Errorf("--include-system printed %q, want %q", lines[1], want)
	}
}

func TestWatchFor(t *testing.T) {
	h := newHarness(t)
	h.rt.Stdout = io.Discard
	cmd := New(h.rt)
	cmd.SetArgs([]string{"--for", "150ms"})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	start := time.Now()
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("watch --for: %v", err)
	}
	if d := time.Since(start); d < 150*time.Millisecond || d > 5*time.Second {
		t.Errorf("watch --for 150ms ran for %s", d)
	}

	cmd = New(h.rt)
	cmd.SetArgs([]string{"--for", "0s"})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	if err := cmd.ExecuteContext(context.Background()); err == nil || !strings.Contains(err.Error(), "--for must be a positive duration") {
		t.Errorf("watch --for 0s: %v", err)
	}
	if !clictx.HasNoTimeout(New(h.rt)) {
		t.Error("watch is cut by the command timeout")
	}
}

// A name Slack does not give is not asked for again with each message, and
// a rate limited lookup, of a user as of a channel, pauses them all.
func TestNamesAreNotLookedUpWithEachMessage(t *testing.T) {
	testenv.Isolate(t)
	var (
		mu      sync.Mutex
		calls   = map[string]int{}
		limited bool
	)
	client := slackapi.NewFake(func(_ context.Context, method string, _ map[string]any) (map[string]any, error) {
		mu.Lock()
		defer mu.Unlock()
		calls[method]++
		if limited {
			return nil, &slackapi.RateLimitError{Method: method, RetryAfter: 30 * time.Second}
		}
		return map[string]any{"ok": false, "error": "not_found"}, nil
	})
	var stderr bytes.Buffer
	w := &watcher{
		rt: &clictx.Context{Stderr: &stderr}, client: client,
		channelNames: map[string]string{}, userNames: map[string]string{}, unnamedUntil: map[string]time.Time{},
	}
	ctx, incomplete := enrich.Collect(context.Background())
	channel := func(id string) {
		t.Helper()
		if name, err := w.name(ctx, w.channelNames, id, func() (string, error) { return channels.LookupName(ctx, client, id) }); name != "" || err != nil {
			t.Fatalf("name of %s = %q, %v", id, name, err)
		}
	}
	user := func(id string) {
		t.Helper()
		if name, err := w.name(ctx, w.userNames, id, func() (string, error) { return w.lookupUser(ctx, id) }); name != "" || err != nil {
			t.Fatalf("name of %s = %q, %v", id, name, err)
		}
	}
	for range 3 {
		channel("C000000009")
		user("U000000009")
	}
	if calls["conversations.info"] != 1 || calls["users.info"] != 1 {
		t.Errorf("API calls for names Slack does not give = %v, want one each", calls)
	}

	limited = true
	user("U000000008")
	user("U000000007")
	channel("C000000008")
	if calls["conversations.info"] != 1 || calls["users.info"] != 2 {
		t.Errorf("API calls once rate limited = %v, want a single users.info more", calls)
	}
	if !strings.Contains(stderr.String(), "rate limiting the name lookups") {
		t.Errorf("stderr = %q, want the warning about the rate limit", stderr.String())
	}
	if got := incomplete.List(); len(got) != 0 {
		t.Errorf("warnings left for the end of the command: %v", got)
	}
}

func TestTextLineDropsControlCharacters(t *testing.T) {
	got := textLine(Event{ChannelID: "C000000001", ChannelName: "gen\x1beral", Author: &messages.Author{Name: "Al\rice"}, Content: "a\x1b[2J\x07b\n\tc\u009b"})
	if want := "#general Alice: a[2Jb\n    \tc"; got != want {
		t.Errorf("textLine = %q, want %q", got, want)
	}
}

// Running out of --for, or being interrupted, while the command is still
// starting (here: resolving a channel name) ends it like later on: without
// an error.
func TestWatchCutShortWhileStartingIsNotAnError(t *testing.T) {
	for _, env := range []string{"SLACK_TOKEN", "SLACK_COOKIE_D", "SLACK_COOKIE", "SLACK_WORKSPACE_URL"} {
		t.Setenv(env, "")
	}
	testenv.Isolate(t)
	store := &auth.Store{Path: filepath.Join(t.TempDir(), "credentials.json")}
	if err := store.Upsert(auth.Workspace{
		URL:  "https://acme.slack.com",
		Auth: slackapi.Auth{Type: slackapi.AuthBrowser, XoxcToken: "xoxc-test", XoxdCookie: "xoxd-test"},
	}); err != nil {
		t.Fatal(err)
	}
	rt := &clictx.Context{
		Store: store, Stdout: io.Discard, Stderr: io.Discard,
		// Slack never answers: every call lasts until the command ends.
		ClientOptions: []slackapi.Option{slackapi.WithHandler(func(ctx context.Context, _ string, _ map[string]any) (map[string]any, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		})},
	}
	run := func(ctx context.Context, args ...string) error {
		cmd := New(rt)
		cmd.SetArgs(args)
		cmd.SetOut(io.Discard)
		cmd.SetErr(io.Discard)
		return cmd.ExecuteContext(ctx)
	}
	if err := run(context.Background(), "--channel", "#general", "--for", "50ms"); err != nil {
		t.Errorf("--for ran out while resolving the channel: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	if err := run(ctx, "--channel", "#general"); err != nil {
		t.Errorf("interrupted while resolving the channel: %v", err)
	}
}

// Credentials rejected in the middle of a watch are refreshed from Slack
// Desktop and the watch goes on; the messages of the meantime are lost,
// and that is said, as for any other reconnection.
func TestWatchReportsTheGapOfACredentialRefresh(t *testing.T) {
	for _, env := range []string{"SLACK_TOKEN", "SLACK_COOKIE_D", "SLACK_COOKIE", "SLACK_WORKSPACE_URL"} {
		t.Setenv(env, "")
	}
	testenv.Isolate(t)
	streamOptions = rtm.Options{PingInterval: 10 * time.Millisecond, ReadTimeout: time.Second, Backoff: func(int) time.Duration { return time.Millisecond }}
	t.Cleanup(func() { streamOptions = rtm.Options{} })

	// The first connection is dropped after hello; the second one, opened
	// with the new credentials, delivers a message.
	var conns atomic.Int32
	server := httptest.NewServer(websocket.Handler(func(ws *websocket.Conn) {
		_ = websocket.Message.Send(ws, `{"type":"hello"}`)
		if conns.Add(1) == 1 {
			return
		}
		_ = websocket.Message.Send(ws, `{"type":"message","channel":"C000000001","user":"U000000001","text":"back","ts":"1700000001.000100"}`)
		_, _ = io.Copy(io.Discard, ws)
	}))
	t.Cleanup(server.Close)

	store := &auth.Store{Path: filepath.Join(t.TempDir(), "credentials.json")}
	if err := store.Upsert(auth.Workspace{
		URL:  "https://acme.slack.com",
		Auth: slackapi.Auth{Type: slackapi.AuthBrowser, XoxcToken: "xoxc-old", XoxdCookie: "xoxd-old"},
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var stderr bytes.Buffer
	var connects atomic.Int32
	rt := &clictx.Context{
		Store:  store,
		Stdout: &lineWriter{want: 1, done: cancel},
		Stderr: &stderr,
		Desktop: func(context.Context) (*authextract.Extracted, error) {
			return &authextract.Extracted{CookieD: "xoxd-new", Teams: []authextract.Team{{URL: "https://acme.slack.com/", Name: "acme", Token: "xoxc-new"}}}, nil
		},
		ClientOptions: []slackapi.Option{
			slackapi.WithLoopbackWorkspaceForTesting(),
			slackapi.WithHandler(func(_ context.Context, method string, _ map[string]any) (map[string]any, error) {
				if method != "rtm.connect" {
					return map[string]any{"ok": false, "error": "unknown_method"}, nil
				}
				// The session expires after the first connection, until
				// it is refreshed (the third call).
				if connects.Add(1) == 2 {
					return map[string]any{"ok": false, "error": "invalid_auth"}, nil
				}
				return map[string]any{"url": "ws" + strings.TrimPrefix(server.URL, "http")}, nil
			}),
		},
	}
	cmd := New(rt)
	cmd.SetArgs(nil)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	if err := cmd.ExecuteContext(ctx); err != nil {
		t.Fatalf("watch: %v", err)
	}
	if got := stderr.String(); strings.Count(got, "Connected to") != 1 || !strings.Contains(got, "Reconnected after") {
		t.Errorf("stderr does not report the gap of the refresh:\n%s", got)
	}
}
