package slackapi

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/maelvls/slackcli/internal/trace"
)

// liveTrace turns the live trace on for the test and returns what it
// writes.
func liveTrace(t *testing.T) *bytes.Buffer {
	t.Helper()
	// Registered first, so that it runs once the variable is back to
	// what it was: Reset reads it.
	t.Cleanup(func() { trace.Reset(nil) })
	t.Setenv("SLACKCLI_DEBUG", "1")
	var out bytes.Buffer
	trace.Reset(&out)
	return &out
}

func checkTrace(t *testing.T, out string, want ...string) {
	t.Helper()
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if len(lines) != len(want) {
		t.Fatalf("got %d lines, want %d:\n%s", len(lines), len(want), out)
	}
	for i, re := range want {
		if !regexp.MustCompile(`^\[slackcli \+\S+\] ` + re + `$`).MatchString(lines[i]) {
			t.Errorf("line %d = %q, want %s", i, lines[i], re)
		}
	}
}

// Every attempt of a call and every rate-limit wait is a line of the
// trace, and none of them shows the token, the cookie or a parameter.
func TestTraceOfARateLimitedCall(t *testing.T) {
	t.Setenv("SLACKCLI_RATE_LIMIT_MAX_WAIT_MS", "30000")
	out := liveTrace(t)
	calls := 0
	c := browserClient(t, func(w http.ResponseWriter, _ *http.Request) {
		if calls++; calls == 1 {
			w.Header().Set("Retry-After", "2")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = io.WriteString(w, `{"ok":true}`)
	}, WithSleep(func(context.Context, time.Duration) error { return nil }))

	if _, err := c.API(context.Background(), "conversations.history", map[string]any{"channel": "C0SECRET"}); err != nil {
		t.Fatal(err)
	}
	checkTrace(t, out.String(),
		`api conversations\.history: \S+, failed: HTTP 429, Retry-After 2s`,
		`rate-limit wait conversations\.history: \S+`,
		`api conversations\.history \(attempt 2\): \S+, HTTP 200`,
	)
	for _, secret := range []string{"xoxc-test", "xoxd-", "C0SECRET"} {
		if strings.Contains(out.String(), secret) {
			t.Errorf("the trace shows %q:\n%s", secret, out)
		}
	}
	if got := trace.Summary(); !strings.Contains(got, "api conversations.history ") || !strings.Contains(got, "(2 calls, 1 failed)") || !strings.Contains(got, "rate-limit wait ") {
		t.Errorf("summary = %q", got)
	}
}

func TestTraceOfFailedCalls(t *testing.T) {
	t.Setenv("SLACKCLI_API_TIMEOUT_MS", "30")
	out := liveTrace(t)

	refused := browserClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"ok":false,"error":"channel_not_found"}`)
	})
	if _, err := refused.API(context.Background(), "conversations.info", nil); err == nil {
		t.Fatal("no error")
	}

	release := make(chan struct{})
	silent := browserClient(t, func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	})
	t.Cleanup(func() { close(release) })
	if _, err := silent.API(context.Background(), "conversations.history", nil); err == nil {
		t.Fatal("no error")
	}

	// The command was cancelled (its own timeout, Ctrl-C) during a call.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := silent.API(ctx, "users.info", nil); err == nil {
		t.Fatal("no error")
	}

	checkTrace(t, out.String(),
		`api conversations\.info: \S+, failed: HTTP 200, channel_not_found`,
		`api conversations\.history: \S+, failed: no answer after 30ms`,
		`api users\.info: \S+, failed: cancelled: context canceled`,
	)
}
