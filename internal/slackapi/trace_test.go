package slackapi

import (
	"bytes"
	"context"
	"errors"
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

// A call that is returned as an error is a failed span, whatever the HTTP
// status: an HTTP error without a code, a redirect, a 200 that is not
// JSON or does not say ok.
func TestTraceOfAnswersThatAreErrors(t *testing.T) {
	out := liveTrace(t)
	for _, answer := range []struct {
		status int
		body   string
	}{
		{503, "<html>Service Unavailable</html>"},
		{302, ""},
		{200, "<html>Sign in to the Wi-Fi</html>"},
		{200, `{"ok":false}`},
		{503, `{"ok":false,"error":"backend_failure"}`},
		{200, `{"ok":true}`},
	} {
		c := browserClient(t, func(w http.ResponseWriter, _ *http.Request) {
			if answer.status == 302 {
				w.Header().Set("Location", "https://elsewhere.example.com/secret-path")
			}
			w.WriteHeader(answer.status)
			_, _ = io.WriteString(w, answer.body)
		})
		_, err := c.API(context.Background(), "auth.test", nil)
		if failed := answer.body != `{"ok":true}`; (err != nil) != failed {
			t.Errorf("%d %q: err = %v", answer.status, answer.body, err)
		}
	}
	checkTrace(t, out.String(),
		`api auth\.test: \S+, failed: HTTP 503`,
		`api auth\.test: \S+, failed: HTTP 302, a redirect`,
		`api auth\.test: \S+, failed: HTTP 200, not JSON`,
		`api auth\.test: \S+, failed: HTTP 200, not ok`,
		`api auth\.test: \S+, failed: HTTP 503, backend_failure`,
		`api auth\.test: \S+, HTTP 200`,
	)
	if got := trace.Summary(); !strings.Contains(got, "(6 calls, 5 failed)") {
		t.Errorf("summary = %q", got)
	}
}

// An HTTP 5xx is Slack failing, with or without an error code of its own,
// known or not: the error keeps the status, and is transient.
func TestACodedServerErrorIsTransient(t *testing.T) {
	answer := func(status int, body string) error {
		c := browserClient(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
			_, _ = io.WriteString(w, body)
		})
		_, err := c.API(context.Background(), "users.lookupByEmail", nil)
		return err
	}
	err := answer(503, `{"ok":false,"error":"backend_failure"}`)
	var api *APIError
	if !errors.As(err, &api) || api.Code != "backend_failure" || api.Status != 503 || !IsTransient(err) {
		t.Errorf("503 backend_failure: %#v, transient = %v", err, IsTransient(err))
	}
	for body, transient := range map[string]bool{
		`{"ok":false,"error":"users_not_found"}`: false,
		`{"ok":false,"error":"internal_error"}`:  true,
	} {
		err := answer(200, body)
		if !errors.As(err, &api) || api.Status != 200 || IsTransient(err) != transient {
			t.Errorf("200 %s: %#v, transient = %v", body, err, IsTransient(err))
		}
	}
	if err := answer(404, `{"ok":false,"error":"unknown_method"}`); IsTransient(err) {
		t.Errorf("a coded 404 is transient: %v", err)
	}
}

// A caller with a retry policy of its own turns the client's off: the
// two would multiply.
func TestWithoutRateLimitRetry(t *testing.T) {
	t.Setenv("SLACKCLI_RATE_LIMIT_MAX_WAIT_MS", "30000")
	calls := 0
	var slept []time.Duration
	c := browserClient(t, func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.Header().Set("Retry-After", "2")
		w.WriteHeader(http.StatusTooManyRequests)
	}, WithSleep(func(_ context.Context, d time.Duration) error { slept = append(slept, d); return nil }))

	_, err := c.API(WithoutRateLimitRetry(context.Background()), "users.lookupByEmail", nil)
	var rl *RateLimitError
	if !errors.As(err, &rl) || rl.RetryAfter != 2*time.Second || calls != 1 || len(slept) != 0 {
		t.Errorf("err = %v after %d calls and %v of waiting", err, calls, slept)
	}

	// Without it, the client retries by itself.
	calls = 0
	if _, err := c.API(context.Background(), "users.lookupByEmail", nil); err == nil || calls != maxRateLimitRetry+1 || len(slept) != maxRateLimitRetry {
		t.Errorf("err = %v after %d calls and %v of waiting", err, calls, slept)
	}
}
