package enrich

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/maelvls/slackcli/internal/appinfo"
	"github.com/maelvls/slackcli/internal/slackapi"
)

func rateLimit(method string) error {
	return &slackapi.RateLimitError{Method: method, RetryAfter: time.Minute}
}

func authError() error {
	_, err := slackapi.NewFake(func(context.Context, string, map[string]any) (map[string]any, error) {
		return map[string]any{"ok": false, "error": "invalid_auth"}, nil
	}).API(context.Background(), "auth.test", nil)
	return err
}

func TestMustAbort(t *testing.T) {
	ctx := context.Background()
	cancelled, cancel := context.WithCancel(ctx)
	cancel()

	if !MustAbort(ctx, authError()) || !MustAbort(ctx, fmt.Errorf("wrapped: %w", authError())) {
		t.Error("dead credentials do not abort")
	}
	if !MustAbort(cancelled, errors.New("anything")) {
		t.Error("an error under a cancelled context does not abort")
	}
	// A rate limit is not a reason to lose the result.
	for _, err := range []error{nil, rateLimit("users.info"), errors.New("connection reset"), context.DeadlineExceeded} {
		if MustAbort(ctx, err) {
			t.Errorf("MustAbort(%v) = true", err)
		}
	}
	if MustAbort(cancelled, nil) {
		t.Error("no error, yet abort")
	}

	if !IsRateLimit(rateLimit("x")) || !IsRateLimit(fmt.Errorf("wrapped: %w", rateLimit("x"))) || IsRateLimit(authError()) || IsRateLimit(nil) {
		t.Error("IsRateLimit is wrong")
	}
}

// A required lookup aborts on what aborts an optional one, and on a rate
// limit too: "not found" must not be concluded from a call Slack refused.
func TestMustAbortRequired(t *testing.T) {
	ctx := context.Background()
	cancelled, cancel := context.WithCancel(ctx)
	cancel()

	for name, err := range map[string]error{
		"dead credentials":   authError(),
		"wrapped auth error": fmt.Errorf("wrapped: %w", authError()),
		"rate limit":         rateLimit("conversations.replies"),
		"wrapped rate limit": fmt.Errorf("wrapped: %w", rateLimit("conversations.replies")),
		// The code some methods answer in the body of an HTTP 200, for an
		// *APIError that did not come from the client (which gives it as
		// a *RateLimitError).
		"ratelimited code":  fmt.Errorf("wrapped: %w", &slackapi.APIError{Method: "conversations.replies", Code: RateLimitedCode}),
		"rate_limited code": fmt.Errorf("wrapped: %w", &slackapi.APIError{Method: "chat.postMessage", Code: slackapi.PostingRateLimitedCode}),
	} {
		if !MustAbortRequired(ctx, err) {
			t.Errorf("%s does not abort a required lookup", name)
		}
	}
	if !MustAbortRequired(cancelled, errors.New("anything")) {
		t.Error("an error under a cancelled context does not abort")
	}
	// Everything else is "not found this way": the next lookup is tried.
	notFound := &slackapi.APIError{Method: "conversations.replies", Code: "thread_not_found"}
	for _, err := range []error{nil, notFound, errors.New("connection reset"), context.DeadlineExceeded} {
		if MustAbortRequired(ctx, err) {
			t.Errorf("MustAbortRequired(%v) = true", err)
		}
	}
	if MustAbortRequired(cancelled, nil) {
		t.Error("no error, yet abort")
	}
}

func TestGate(t *testing.T) {
	var g Gate
	if g.Closed() {
		t.Fatal("the zero gate is closed")
	}
	if g.Failed(nil) || g.Failed(errors.New("user_not_found")) || g.Closed() {
		t.Fatal("an ordinary failure closed the gate")
	}
	if n, err := g.Skipped(); n != 0 || err != nil {
		t.Fatalf("Skipped = %d, %v", n, err)
	}

	first := rateLimit("users.info")
	if !g.Failed(first) || !g.Failed(rateLimit("users.info")) {
		t.Fatal("a rate limit was not recognised")
	}
	if !g.Closed() || !g.Closed() {
		t.Fatal("the gate is still open after a rate limit")
	}
	// Two refused, two not even tried; the first rate limit is the cause.
	if n, err := g.Skipped(); n != 4 || err != first {
		t.Errorf("Skipped = %d, %v", n, err)
	}

	// Safe for concurrent use.
	var wg sync.WaitGroup
	var shared Gate
	for range 50 {
		wg.Go(func() {
			if !shared.Closed() {
				shared.Failed(rateLimit("files.info"))
			}
		})
	}
	wg.Wait()
	if n, _ := shared.Skipped(); n != 50 {
		t.Errorf("Skipped = %d, want 50", n)
	}
}

func TestWarnings(t *testing.T) {
	// Without a collector, a warning is dropped.
	Warnf(context.Background(), "nobody listens")

	ctx, outer := Collect(context.Background())
	Warnf(ctx, "%d users missing", 3)
	Warnf(ctx, "%d users missing", 3) // a retried command repeats itself
	Warnf(ctx, "files incomplete")

	// A function with its own Warnings field collects for itself.
	nested, inner := Collect(ctx)
	Warnf(nested, "inner only")

	if got, want := outer.List(), []string{"3 users missing", "files incomplete"}; !reflect.DeepEqual(got, want) {
		t.Errorf("outer = %q, want %q", got, want)
	}
	if got, want := inner.List(), []string{"inner only"}; !reflect.DeepEqual(got, want) {
		t.Errorf("inner = %q, want %q", got, want)
	}

	if hint := Hint(); !strings.Contains(hint, appinfo.EnvName("RATE_LIMIT_MAX_WAIT_MS")) {
		t.Errorf("hint = %q", hint)
	}
}

// A warning is "<what is missing> (<Cause>): <Hint>". The rate limit error
// and the hint both say which variable to set: it must be said once.
func TestCauseLeavesTheAdviceToHint(t *testing.T) {
	env := appinfo.EnvName("RATE_LIMIT_MAX_WAIT_MS")
	for _, err := range []error{rateLimit("users.info"), fmt.Errorf("looking up U1: %w", rateLimit("users.info"))} {
		if !strings.Contains(err.Error(), env) {
			t.Fatalf("the error itself no longer names %s: the test proves nothing: %v", env, err)
		}
		cause := Cause(err)
		if cause != "Slack API call users.info was rate limited; retry after 1m0s" {
			t.Errorf("Cause(%v) = %q", err, cause)
		}
		if warning := fmt.Sprintf("3 of 9 users were not resolved (%s): %s", cause, Hint()); strings.Count(warning, env) != 1 {
			t.Errorf("the warning names %s %d times, want once: %q", env, strings.Count(warning, env), warning)
		}
	}
	coded := fmt.Errorf("looking up U1: %w", &slackapi.APIError{Method: "users.info", Code: RateLimitedCode})
	if got := Cause(coded); got != "Slack API call users.info was rate limited" {
		t.Errorf("Cause(%v) = %q", coded, got)
	}
	if got := Cause(errors.New("boom")); got != "boom" {
		t.Errorf("Cause of another error = %q, want it as it is", got)
	}
	if got := Cause(nil); got != "" {
		t.Errorf("Cause(nil) = %q", got)
	}
}
