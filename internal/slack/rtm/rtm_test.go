package rtm

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/websocket"

	"github.com/maelvls/slackcli/internal/slackapi"
)

// fakeSlack is a WebSocket server and the rtm.connect that points at it.
// Each connection is served by the next of the given functions; the last
// one serves every further connection.
type fakeSlack struct {
	t      *testing.T
	server *httptest.Server
	conns  atomic.Int32

	mu      sync.Mutex
	headers []http.Header
}

func newFakeSlack(t *testing.T, serve ...func(ws *websocket.Conn)) (*fakeSlack, *slackapi.Client) {
	t.Helper()
	f := &fakeSlack{t: t}
	f.server = httptest.NewServer(websocket.Handler(func(ws *websocket.Conn) {
		n := int(f.conns.Add(1))
		f.mu.Lock()
		f.headers = append(f.headers, ws.Request().Header)
		f.mu.Unlock()
		serve[min(n, len(serve))-1](ws)
	}))
	t.Cleanup(f.server.Close)
	client := slackapi.NewFake(func(_ context.Context, method string, _ map[string]any) (map[string]any, error) {
		if method != "rtm.connect" {
			t.Errorf("unexpected API call %s", method)
		}
		return map[string]any{"ok": true, "url": "ws" + strings.TrimPrefix(f.server.URL, "http") + "/websocket/secret"}, nil
	}, slackapi.WithLoopbackWorkspaceForTesting())
	return f, client
}

func send(ws *websocket.Conn, events ...string) {
	for _, e := range events {
		_ = websocket.Message.Send(ws, e)
	}
}

// hold keeps the connection open until the client leaves, answering pings.
func hold(ws *websocket.Conn) {
	for {
		var msg map[string]any
		if websocket.JSON.Receive(ws, &msg) != nil {
			return
		}
		if msg["type"] == "ping" {
			_ = websocket.JSON.Send(ws, map[string]any{"type": "pong", "reply_to": msg["id"]})
		}
	}
}

var fast = Options{
	PingInterval: 10 * time.Millisecond,
	ReadTimeout:  200 * time.Millisecond,
	Backoff:      func(int) time.Duration { return time.Millisecond },
}

func TestStreamPassesEventsOnAndReconnects(t *testing.T) {
	f, client := newFakeSlack(t,
		// Lost without a word.
		func(ws *websocket.Conn) {
			send(ws, `{"type":"hello"}`, `{"type":"message","text":"one"}`, `not JSON`, `{"ok":true,"reply_to":1}`)
		},
		// Slack says it is going away.
		func(ws *websocket.Conn) {
			send(ws, `{"type":"hello"}`, `{"type":"message","text":"two"}`, `{"type":"goodbye"}`)
			hold(ws)
		},
		func(ws *websocket.Conn) {
			send(ws, `{"type":"hello"}`, `{"type":"message","text":"three"}`)
			hold(ws)
		},
	)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var got []string
	var gaps []time.Duration
	disconnects := 0
	opts := fast
	opts.OnConnect = func(gap time.Duration) { gaps = append(gaps, gap) }
	opts.OnDisconnect = func(error) { disconnects++ }
	err := Stream(ctx, client, opts, func(event map[string]any) error {
		got = append(got, event["text"].(string))
		if len(got) == 3 {
			cancel()
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Stream = %v, want nil once the context is done", err)
	}
	if strings.Join(got, " ") != "one two three" {
		t.Errorf("events = %v", got)
	}
	if len(gaps) != 3 || gaps[0] != 0 || gaps[1] <= 0 || gaps[2] <= 0 || disconnects != 2 {
		t.Errorf("gaps = %v, disconnects = %d", gaps, disconnects)
	}
	// A browser session is recognised by its cookie and its Origin.
	f.mu.Lock()
	defer f.mu.Unlock()
	if h := f.headers[0]; h.Get("Cookie") != "d=xoxd-test" || h.Get("Origin") != "https://app.slack.com" {
		t.Errorf("handshake headers = %v", h)
	}
}

func TestStreamPingsAndGivesUpOnASilentConnection(t *testing.T) {
	pinged := make(chan struct{}, 1)
	f, client := newFakeSlack(t,
		// Reads the ping, never answers: the connection is dead.
		func(ws *websocket.Conn) {
			send(ws, `{"type":"hello"}`)
			var msg map[string]any
			if websocket.JSON.Receive(ws, &msg) == nil && msg["type"] == "ping" {
				pinged <- struct{}{}
			}
			time.Sleep(time.Second)
		},
		func(ws *websocket.Conn) {
			send(ws, `{"type":"hello"}`, `{"type":"message"}`)
			hold(ws)
		},
	)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := Stream(ctx, client, fast, func(map[string]any) error { cancel(); return nil }); err != nil {
		t.Fatal(err)
	}
	select {
	case <-pinged:
	default:
		t.Error("no ping was sent")
	}
	if n := f.conns.Load(); n != 2 {
		t.Errorf("connections = %d, want 2", n)
	}
}

func TestStreamReturnsTheErrorOfTheHandler(t *testing.T) {
	_, client := newFakeSlack(t, func(ws *websocket.Conn) {
		send(ws, `{"type":"hello"}`, `{"type":"message"}`)
		hold(ws)
	})
	boom := errors.New("boom")
	if err := Stream(context.Background(), client, fast, func(map[string]any) error { return boom }); err != boom {
		t.Errorf("Stream = %v, want the handler's error", err)
	}
}

func TestStreamFailsWhenTheFirstConnectionDoes(t *testing.T) {
	// rtm.connect refused.
	client := slackapi.NewFake(func(context.Context, string, map[string]any) (map[string]any, error) {
		return map[string]any{"ok": false, "error": "not_allowed_token_type"}, nil
	})
	err := Stream(context.Background(), client, fast, nil)
	if err == nil || !strings.Contains(err.Error(), "not_allowed_token_type") || !strings.Contains(err.Error(), "token of a Slack app") {
		t.Errorf("Stream = %v", err)
	}

	// The WebSocket is not Slack's: the credentials are not sent.
	client = slackapi.NewFake(func(context.Context, string, map[string]any) (map[string]any, error) {
		return map[string]any{"ok": true, "url": "wss://example.com/websocket/secret"}, nil
	})
	err = Stream(context.Background(), client, fast, nil)
	if err == nil || !strings.Contains(err.Error(), "refusing to open the WebSocket at wss://example.com") || strings.Contains(err.Error(), "secret") {
		t.Errorf("Stream = %v", err)
	}

	// Slack closes before saying hello.
	_, client = newFakeSlack(t, func(*websocket.Conn) {})
	if err := Stream(context.Background(), client, fast, nil); err == nil {
		t.Error("Stream = nil for a connection closed before hello")
	}
}

func TestStreamEndsWhenCredentialsAreRejectedLater(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(websocket.Handler(func(ws *websocket.Conn) { send(ws, `{"type":"hello"}`) }))
	defer server.Close()
	client := slackapi.NewFake(func(context.Context, string, map[string]any) (map[string]any, error) {
		if calls.Add(1) > 1 {
			return map[string]any{"ok": false, "error": "invalid_auth"}, nil
		}
		return map[string]any{"ok": true, "url": "ws" + strings.TrimPrefix(server.URL, "http")}, nil
	}, slackapi.WithLoopbackWorkspaceForTesting())
	err := Stream(context.Background(), client, fast, func(map[string]any) error { return nil })
	if !slackapi.IsAuthError(err) {
		t.Errorf("Stream = %v, want the auth error", err)
	}
}

// A rate limited rtm.connect is not asked for again before the time Slack
// gave, whatever the backoff says.
func TestStreamWaitsForTheRateLimitBeforeReconnecting(t *testing.T) {
	const retryAfter = 300 * time.Millisecond
	var (
		calls     atomic.Int32
		limitedAt time.Time
		waited    time.Duration
	)
	server := httptest.NewServer(websocket.Handler(func(ws *websocket.Conn) { send(ws, `{"type":"hello"}`) }))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client := slackapi.NewFake(func(context.Context, string, map[string]any) (map[string]any, error) {
		switch calls.Add(1) {
		case 1:
		case 2:
			limitedAt = time.Now()
			return nil, &slackapi.RateLimitError{Method: "rtm.connect", RetryAfter: retryAfter}
		default:
			waited = time.Since(limitedAt)
			cancel()
		}
		return map[string]any{"ok": true, "url": "ws" + strings.TrimPrefix(server.URL, "http")}, nil
	}, slackapi.WithLoopbackWorkspaceForTesting())
	if err := Stream(ctx, client, fast, func(map[string]any) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if waited < retryAfter {
		t.Errorf("reconnected %s after a rate limit of %s", waited, retryAfter)
	}
}

func TestBackoff(t *testing.T) {
	for attempt, want := range map[int]time.Duration{1: time.Second, 2: 2 * time.Second, 5: 16 * time.Second, 6: maxBackoff, 500: maxBackoff} {
		if got := backoff(attempt); got != want {
			t.Errorf("backoff(%d) = %s, want %s", attempt, got, want)
		}
	}
}
