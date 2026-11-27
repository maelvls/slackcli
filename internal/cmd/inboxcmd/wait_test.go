package inboxcmd

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/websocket"

	"github.com/maelvls/slackcli/internal/slack/rtm"
	"github.com/maelvls/slackcli/internal/slackapi"
)

// waitHarness runs "inbox wait" against a fake Slack whose WebSocket sends
// hello, then events.
func waitHarness(t *testing.T, events []string, serve func(method string, params map[string]any) map[string]any) *harness {
	t.Helper()
	streamOptions = rtm.Options{PingInterval: 10 * time.Millisecond, ReadTimeout: time.Second}
	settle, clockStep, indexLag, retryDelay = 50*time.Millisecond, 10*time.Millisecond, 20*time.Millisecond, 20*time.Millisecond
	t.Cleanup(func() {
		streamOptions, settle, clockStep, indexLag, retryDelay = rtm.Options{}, 2*time.Second, 5*time.Second, 15*time.Second, 30*time.Second
	})

	server := httptest.NewServer(websocket.Handler(func(ws *websocket.Conn) {
		for _, e := range append([]string{`{"type":"hello"}`}, events...) {
			_ = websocket.Message.Send(ws, e)
		}
		_, _ = io.Copy(io.Discard, ws)
	}))
	t.Cleanup(server.Close)
	h := newHarness(t, func(method string, params map[string]any) map[string]any {
		switch method {
		case "auth.test":
			return map[string]any{"user_id": "U000000001", "url": "https://acme.slack.com/"}
		case "rtm.connect":
			if serve != nil {
				if resp := serve(method, params); resp != nil {
					return resp
				}
			}
			return map[string]any{"url": "ws" + strings.TrimPrefix(server.URL, "http")}
		case "users.info":
			return map[string]any{"user": map[string]any{"id": params["user"], "name": "alice", "profile": map[string]any{"display_name": "Alice"}}}
		case "conversations.info":
			return map[string]any{"channel": map[string]any{"name": "general"}}
		}
		if serve != nil {
			if resp := serve(method, params); resp != nil {
				return resp
			}
		}
		if method == "search.messages" {
			return map[string]any{"messages": map[string]any{"matches": []any{}, "paging": map[string]any{"pages": 1.0}}}
		}
		return map[string]any{"ok": false, "error": "unknown_method"}
	})
	h.rt.ClientOptions = append(h.rt.ClientOptions, slackapi.WithLoopbackWorkspaceForTesting())
	return h
}

func (h *harness) runWait(t *testing.T, args ...string) map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	h.stdout.Reset()
	cmd := New(h.rt)
	cmd.SetArgs(append([]string{"wait"}, args...))
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	if err := cmd.ExecuteContext(ctx); err != nil {
		t.Fatalf("inbox wait %v: %v", args, err)
	}
	var out map[string]any
	if err := json.Unmarshal(h.stdout.Bytes(), &out); err != nil {
		t.Fatalf("not JSON: %q", h.stdout.String())
	}
	return out
}

func (h *harness) count(method string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, m := range h.methods {
		if m == method {
			n++
		}
	}
	return n
}

func event(fields string) string { return `{"type":"message",` + fields + `}` }

func TestWaitDM(t *testing.T) {
	ts := tsAgo(-time.Minute) // after the start of the command
	events := []string{
		// Noise: a channel message without mention, a bot, my own message,
		// an edit.
		event(`"channel":"C000000001","channel_type":"channel","user":"U000000009","text":"hi all","ts":"` + tsAgo(-10*time.Second) + `"`),
		event(`"subtype":"bot_message","channel":"D000000002","bot_id":"B1","text":"beep","ts":"` + tsAgo(-11*time.Second) + `"`),
		event(`"channel":"D000000002","user":"U000000001","text":"me","ts":"` + tsAgo(-12*time.Second) + `"`),
		event(`"subtype":"message_changed","hidden":true,"channel":"D000000002","ts":"` + tsAgo(-13*time.Second) + `"`),
		event(`"channel":"D000000003","channel_type":"im","user":"U000000009","text":"ping","ts":"` + ts + `"`),
	}
	h := waitHarness(t, events, func(method string, params map[string]any) map[string]any {
		if method == "conversations.history" && params["channel"] == "D000000003" {
			return map[string]any{"messages": []any{map[string]any{"ts": ts, "user": "U000000009", "text": "ping"}}}
		}
		return nil
	})
	out := h.runWait(t)
	items := out["items"].([]any)
	if len(items) != 1 || out["after"] != ts || out["user_id"] != "U000000001" {
		t.Fatalf("printed %s", h.stdout.String())
	}
	item := items[0].(map[string]any)
	if item["kind"] != "dm" || item["channel_id"] != "D000000003" || item["channel_name"] != "Alice" || item["text"] != "ping" || item["count"] != 1.0 {
		t.Errorf("item = %v", item)
	}
	// The noise cost no read; the search ran once, for the index lag,
	// before printing (no --after: no catch-up at the start).
	if n := h.count("conversations.history") + h.count("conversations.replies"); n != 1 {
		t.Errorf("reads = %d, calls %v", n, h.methods)
	}
	if n := h.count("search.messages"); n != 3 {
		t.Errorf("search.messages called %d times, want the 3 of one catch-up: %v", n, h.methods)
	}
}

func TestWaitThreads(t *testing.T) {
	root := tsAgo(time.Hour)
	r1, r2, mine, r3, mention := tsAgo(-10*time.Second), tsAgo(-20*time.Second), tsAgo(-30*time.Second), tsAgo(-40*time.Second), tsAgo(-50*time.Second)
	events := []string{
		// Two replies in a thread I am not in: one read.
		event(`"channel":"C000000001","channel_type":"channel","user":"U000000009","text":"r1","ts":"` + r1 + `","thread_ts":"` + root + `"`),
		event(`"channel":"C000000001","channel_type":"channel","user":"U000000009","text":"r2","ts":"` + r2 + `","thread_ts":"` + root + `"`),
		// I reply in another thread, then someone replies there: an item.
		event(`"channel":"C000000001","channel_type":"channel","user":"U000000001","text":"mine","ts":"` + mine + `","thread_ts":"` + root + `9"`),
		event(`"channel":"C000000001","channel_type":"channel","user":"U000000009","text":"r3","ts":"` + r3 + `","thread_ts":"` + root + `9"`),
		// A mention at the top level of a channel: an item too.
		event(`"channel":"C000000002","channel_type":"channel","user":"U000000008","text":"<@U000000001> look","ts":"` + mention + `"`),
	}
	h := waitHarness(t, events, func(method string, params map[string]any) map[string]any {
		switch {
		case method == "conversations.replies" && params["ts"] == root:
			return map[string]any{"messages": []any{
				map[string]any{"ts": root, "user": "U000000009", "text": "root"},
				map[string]any{"ts": r1, "user": "U000000009", "text": "r1"},
			}}
		case method == "conversations.replies" && params["ts"] == root+"9":
			return map[string]any{"messages": []any{
				map[string]any{"ts": root + "9", "user": "U000000007", "text": "root"},
				map[string]any{"ts": mine, "user": "U000000001", "text": "mine"},
				map[string]any{"ts": r3, "user": "U000000009", "text": "r3"},
			}}
		case method == "conversations.replies" && params["ts"] == mention:
			return map[string]any{"messages": []any{map[string]any{"ts": mention, "user": "U000000008", "text": "<@U000000001> look"}}}
		case method == "conversations.history":
			return map[string]any{"messages": []any{}}
		}
		return nil
	})
	out := h.runWait(t)
	var kinds []string
	for _, it := range out["items"].([]any) {
		kinds = append(kinds, it.(map[string]any)["kind"].(string))
	}
	if !slices.Equal(kinds, []string{"mention", "thread"}) || out["after"] != mention {
		t.Fatalf("printed %s", h.stdout.String())
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	replies := 0
	for _, m := range h.methods {
		if m == "conversations.replies" {
			replies++
		}
	}
	// root (once, for two replies), root9, the mention.
	if replies != 3 {
		t.Errorf("conversations.replies called %d times: %v", replies, h.methods)
	}
}

func TestWaitCatchUp(t *testing.T) {
	after, dmTS, oldTS := tsAgo(2*time.Hour), tsAgo(time.Hour), tsAgo(3*time.Hour)
	h := waitHarness(t, nil, func(method string, params map[string]any) map[string]any {
		switch method {
		case "search.messages":
			var matches []any
			if strings.HasPrefix(params["query"].(string), "to:") {
				for _, ts := range []string{dmTS, oldTS} {
					matches = append(matches, map[string]any{
						"channel": map[string]any{"id": "D000000001", "is_im": true, "name": "U000000009"},
						"user":    "U000000009", "ts": ts,
						"permalink": "https://acme.slack.com/archives/D000000001/p" + strings.ReplaceAll(ts, ".", ""),
					})
				}
			}
			return map[string]any{"messages": map[string]any{"matches": matches, "paging": map[string]any{"pages": 1.0}}}
		case "conversations.history":
			return map[string]any{"messages": []any{
				map[string]any{"ts": dmTS, "user": "U000000009", "text": "new"},
				map[string]any{"ts": oldTS, "user": "U000000009", "text": "old"},
			}}
		}
		return nil
	})
	out := h.runWait(t, "--after", after)
	items := out["items"].([]any)
	if len(items) != 1 || out["after"] != dmTS {
		t.Fatalf("printed %s", h.stdout.String())
	}
	// Only what came after --after counts.
	if item := items[0].(map[string]any); item["count"] != 1.0 || item["text"] != "new" {
		t.Errorf("item = %v", item)
	}
}

func TestWaitTimeout(t *testing.T) {
	h := waitHarness(t, nil, nil)
	after := tsAgo(-time.Hour) // nothing can be after it
	out := h.runWait(t, "--after", after, "--timeout", "50ms")
	if out["timed_out"] != true || out["after"] != after || len(out["items"].([]any)) != 0 {
		t.Fatalf("printed %s", h.stdout.String())
	}
}

func TestWaitRejectsBadFlags(t *testing.T) {
	h := waitHarness(t, nil, nil)
	for _, args := range [][]string{{"--after", "yesterday"}, {"--timeout", "-1s"}, {"--max-conversations", "0"}} {
		cmd := New(h.rt)
		cmd.SetArgs(append([]string{"wait"}, args...))
		cmd.SetOut(io.Discard)
		cmd.SetErr(io.Discard)
		if err := cmd.ExecuteContext(context.Background()); err == nil {
			t.Errorf("inbox wait %v: no error", args)
		}
	}
}

// dmMatch is a search match for a DM from U000000009.
func dmMatch(channel, ts string) map[string]any {
	return map[string]any{
		"channel": map[string]any{"id": channel, "is_im": true, "name": "U000000009"},
		"user":    "U000000009", "ts": ts,
		"permalink": "https://acme.slack.com/archives/" + channel + "/p" + strings.ReplaceAll(ts, ".", ""),
	}
}

// A catch-up still running when the stream has an item is waited for:
// the backlog it finds is printed, not lost behind the cursor.
func TestWaitSlowCatchUpIsNotCut(t *testing.T) {
	after, oldDM, mention := tsAgo(3*time.Hour), tsAgo(2*time.Hour), tsAgo(-time.Minute)
	events := []string{event(`"channel":"C000000002","channel_type":"channel","user":"U000000008","text":"<@U000000001> look","ts":"` + mention + `"`)}
	h := waitHarness(t, events, func(method string, params map[string]any) map[string]any {
		switch method {
		case "search.messages":
			time.Sleep(300 * time.Millisecond) // far longer than settle
			var matches []any
			if strings.HasPrefix(params["query"].(string), "to:") {
				matches = append(matches, dmMatch("D000000001", oldDM))
			}
			return map[string]any{"messages": map[string]any{"matches": matches, "paging": map[string]any{"pages": 1.0}}}
		case "conversations.history":
			if params["channel"] == "D000000001" {
				return map[string]any{"messages": []any{map[string]any{"ts": oldDM, "user": "U000000009", "text": "old"}}}
			}
			return map[string]any{"messages": []any{}}
		case "conversations.replies":
			return map[string]any{"messages": []any{map[string]any{"ts": mention, "user": "U000000008", "text": "<@U000000001> look"}}}
		}
		return nil
	})
	out := h.runWait(t, "--after", after)
	if items := out["items"].([]any); len(items) != 2 || out["after"] != mention {
		t.Fatalf("printed %s", h.stdout.String())
	}
}

// A message that cannot be judged because Slack failed does not end the
// wait: it is looked for again through the search.
func TestWaitTransientError(t *testing.T) {
	dm1, dm2 := tsAgo(-time.Minute), tsAgo(-2*time.Minute)
	events := []string{
		event(`"channel":"D000000001","channel_type":"im","user":"U000000009","text":"one","ts":"` + dm1 + `"`),
		event(`"channel":"D000000002","channel_type":"im","user":"U000000009","text":"two","ts":"` + dm2 + `"`),
	}
	var failed atomic.Bool
	h := waitHarness(t, events, func(method string, params map[string]any) map[string]any {
		switch method {
		case "conversations.history":
			if params["channel"] == "D000000001" {
				if failed.CompareAndSwap(false, true) {
					return map[string]any{"ok": false, "error": "internal_error"}
				}
				return map[string]any{"messages": []any{map[string]any{"ts": dm1, "user": "U000000009", "text": "one"}}}
			}
			return map[string]any{"messages": []any{map[string]any{"ts": dm2, "user": "U000000009", "text": "two"}}}
		case "search.messages":
			var matches []any
			if strings.HasPrefix(params["query"].(string), "to:") {
				matches = append(matches, dmMatch("D000000001", dm1))
			}
			return map[string]any{"messages": map[string]any{"matches": matches, "paging": map[string]any{"pages": 1.0}}}
		}
		return nil
	})
	out := h.runWait(t)
	if items := out["items"].([]any); len(items) != 2 || out["after"] != dm2 {
		t.Fatalf("printed %s", h.stdout.String())
	}
}

// When the last search fails, the items are printed but the cursor stays
// where it was, so that the next run looks again.
func TestWaitCursorStaysWhenSearchFails(t *testing.T) {
	dm := tsAgo(-time.Minute)
	events := []string{event(`"channel":"D000000001","channel_type":"im","user":"U000000009","text":"hi","ts":"` + dm + `"`)}
	h := waitHarness(t, events, func(method string, params map[string]any) map[string]any {
		switch method {
		case "conversations.history":
			return map[string]any{"messages": []any{map[string]any{"ts": dm, "user": "U000000009", "text": "hi"}}}
		case "search.messages":
			return map[string]any{"ok": false, "error": "missing_scope"}
		}
		return nil
	})
	out := h.runWait(t)
	if items := out["items"].([]any); len(items) != 1 || out["after"] == dm || out["after"] == "" {
		t.Fatalf("printed %s", h.stdout.String())
	}
	if hints, _ := out["hints"].([]any); len(hints) == 0 || !strings.Contains(hints[0].(string), "Warning") {
		t.Errorf("no warning: %s", h.stdout.String())
	}
}

// lockedBuffer is a bytes.Buffer that goroutines may write to.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// A first rtm.connect that Slack rate limits does not end the wait (an
// agent that runs it in a loop would stop being woken up): it is tried
// again, which is said on stderr.
func TestWaitRetriesARateLimitedFirstConnection(t *testing.T) {
	ts := tsAgo(-time.Minute)
	events := []string{event(`"channel":"D000000003","channel_type":"im","user":"U000000009","text":"ping","ts":"` + ts + `"`)}
	var connects atomic.Int32
	h := waitHarness(t, events, func(method string, params map[string]any) map[string]any {
		switch {
		case method == "rtm.connect" && connects.Add(1) == 1:
			return map[string]any{"ok": false, "error": "ratelimited"}
		case method == "conversations.history" && params["channel"] == "D000000003":
			return map[string]any{"messages": []any{map[string]any{"ts": ts, "user": "U000000009", "text": "ping"}}}
		}
		return nil
	})
	streamOptions.Backoff = func(int) time.Duration { return time.Millisecond }
	stderr := &lockedBuffer{}
	h.rt.Stderr = stderr
	out := h.runWait(t)
	if items := out["items"].([]any); len(items) != 1 || out["after"] != ts || connects.Load() != 2 {
		t.Fatalf("printed %s after %d connections", h.stdout.String(), connects.Load())
	}
	if got := stderr.String(); !strings.Contains(got, "Cannot connect to Slack yet") || !strings.Contains(got, "ratelimited") {
		t.Errorf("stderr:\n%s", got)
	}
}

// A token that cannot use the real-time API ends the wait at once.
func TestWaitFailsAtOnceWhenTheFirstConnectionIsRefused(t *testing.T) {
	h := waitHarness(t, nil, func(method string, _ map[string]any) map[string]any {
		if method == "rtm.connect" {
			return map[string]any{"ok": false, "error": "not_allowed_token_type"}
		}
		return nil
	})
	streamOptions.Backoff = func(int) time.Duration { return time.Millisecond }
	err := h.run("wait", "--timeout", "5s") // not hanging if tried again
	if !slackapi.IsErrorCode(err, "not_allowed_token_type") || h.count("rtm.connect") != 1 {
		t.Errorf("inbox wait = %v after %d connections", err, h.count("rtm.connect"))
	}
}

// A --timeout that runs out before any connection was ready is an error,
// not timed_out: nothing was listened to.
func TestWaitTimeoutThatNeverConnectedFails(t *testing.T) {
	h := waitHarness(t, nil, func(method string, _ map[string]any) map[string]any {
		if method == "rtm.connect" {
			return map[string]any{"ok": false, "error": "internal_error"}
		}
		return nil
	})
	streamOptions.Backoff = func(int) time.Duration { return 5 * time.Millisecond }
	err := h.run("wait", "--timeout", "100ms")
	if err == nil || !strings.Contains(err.Error(), "could not connect to Slack within 100ms") || h.stdout.Len() != 0 {
		t.Errorf("inbox wait = %v, stdout %q", err, h.stdout.String())
	}
}
