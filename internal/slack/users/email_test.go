package users

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/maelvls/slackcli/internal/slackapi"
)

// flakyLookup is a workspace whose users.lookupByEmail fails the first
// `failures` times with fail(), and whose directory cannot be scanned. It
// returns the recorder and the pauses the client was asked to make.
func flakyLookup(failures int, fail func() error) (*recorder, *slackapi.Client, *[]time.Duration) {
	alice := member("U00000001A", "alice", "profile", profile("email", "alice@example.com"))
	rec := hugeWorkspace(directory{byEmail: map[string]map[string]any{"alice@example.com": alice}})
	serve := rec.serve
	seen := 0
	rec.serve = func(method string, params map[string]any) (map[string]any, error) {
		if method == "users.lookupByEmail" {
			if seen++; seen <= failures {
				return nil, fail()
			}
		}
		return serve(method, params)
	}
	var slept []time.Duration
	client := slackapi.NewFake(func(_ context.Context, method string, params map[string]any) (map[string]any, error) {
		rec.mu.Lock()
		rec.calls = append(rec.calls, call{method, params})
		rec.mu.Unlock()
		return rec.serve(method, params)
	}, slackapi.WithSleep(func(_ context.Context, d time.Duration) error {
		slept = append(slept, d)
		return nil
	}))
	return rec, client, &slept
}

// A transient failure of users.lookupByEmail is retried: it must not send
// an email address into the directory scan, which a large workspace
// cannot finish (issue #7).
func TestLookupByEmailRetriesTransientFailures(t *testing.T) {
	ctx := context.Background()
	rateLimited := func() error {
		return &slackapi.RateLimitError{Method: "users.lookupByEmail", RetryAfter: 2 * time.Second}
	}
	timedOut := func() error {
		return &slackapi.TimeoutError{Method: "users.lookupByEmail", Timeout: 20 * time.Second}
	}

	t.Run("a rate limit is waited out, as long as Slack asks", func(t *testing.T) {
		rec, client, slept := flakyLookup(2, rateLimited)
		got, err := ResolveUserID(ctx, client, "alice@example.com")
		if err != nil || got != "U00000001A" {
			t.Fatalf("got %q, %v", got, err)
		}
		if want := []string{"users.lookupByEmail", "users.lookupByEmail", "users.lookupByEmail"}; !reflect.DeepEqual(rec.methods(), want) {
			t.Errorf("methods = %v, want %v (and no users.list)", rec.methods(), want)
		}
		if want := []time.Duration{2 * time.Second, 2 * time.Second}; !reflect.DeepEqual(*slept, want) {
			t.Errorf("waited %v, want %v", *slept, want)
		}
	})

	t.Run("a timeout is retried once", func(t *testing.T) {
		rec, client, _ := flakyLookup(1, timedOut)
		if got, err := ResolveUserID(ctx, client, "alice@example.com"); err != nil || got != "U00000001A" {
			t.Fatalf("got %q, %v", got, err)
		}
		if n := len(rec.only("users.lookupByEmail")); n != 2 {
			t.Errorf("%d lookups, want 2", n)
		}
	})

	t.Run("a network error and an error of Slack's own are retried", func(t *testing.T) {
		for _, fail := range []func() error{
			func() error {
				return &slackapi.NetworkError{Method: "users.lookupByEmail", Err: errors.New("connection reset")}
			},
			func() error { return &slackapi.HTTPStatusError{Method: "users.lookupByEmail", Status: 503} },
			func() error {
				return &slackapi.APIError{Method: "users.lookupByEmail", Code: "internal_error"}
			},
		} {
			_, client, _ := flakyLookup(1, fail)
			if got, err := ResolveUserID(ctx, client, "alice@example.com"); err != nil || got != "U00000001A" {
				t.Errorf("after %v: got %q, %v", fail(), got, err)
			}
		}
	})

	t.Run("a lookup that keeps failing is an error about the lookup, not a scan", func(t *testing.T) {
		for name, fail := range map[string]func() error{"rate limit": rateLimited, "timeout": timedOut} {
			rec, client, _ := flakyLookup(99, fail)
			_, err := ResolveUserID(ctx, client, "alice@example.com")
			if err == nil {
				t.Fatalf("%s: no error", name)
			}
			if n := len(rec.only("users.list")); n != 0 {
				t.Errorf("%s: the directory was scanned (%d users.list calls)", name, n)
			}
			for _, want := range []string{`"alice@example.com"`, "users.lookupByEmail", "temporary", "user ID (U…)"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("%s: error does not mention %q: %v", name, want, err)
				}
			}
			// The input was an email: do not advise to pass one.
			for _, bad := range []string{"email address instead", "too large"} {
				if strings.Contains(err.Error(), bad) {
					t.Errorf("%s: error says %q: %v", name, bad, err)
				}
			}
			var nf *NotFoundError
			if errors.As(err, &nf) {
				t.Errorf("%s: a failed lookup is not a NotFoundError", name)
			}
		}
		_, client, _ := flakyLookup(99, rateLimited)
		_, err := ResolveUserID(ctx, client, "alice@example.com")
		var rl *slackapi.RateLimitError
		if !errors.As(err, &rl) {
			t.Errorf("the rate limit is not wrapped: %v", err)
		}
	})

	t.Run("a wait Slack asks for that is too long is not made", func(t *testing.T) {
		rec, client, slept := flakyLookup(99, func() error {
			return &slackapi.RateLimitError{Method: "users.lookupByEmail", RetryAfter: time.Minute}
		})
		if _, err := ResolveUserID(ctx, client, "alice@example.com"); err == nil || !strings.Contains(err.Error(), "retry after 1m0s") {
			t.Errorf("err = %v", err)
		}
		if len(*slept) != 0 || len(rec.only("users.lookupByEmail")) != 1 {
			t.Errorf("waited %v over %d lookups", *slept, len(rec.only("users.lookupByEmail")))
		}
	})

	t.Run("a cancelled command stops the retries", func(t *testing.T) {
		ctx, cancel := context.WithCancel(ctx)
		rec, client, _ := flakyLookup(99, func() error {
			cancel()
			return context.Canceled
		})
		if _, err := ResolveUserID(ctx, client, "alice@example.com"); !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v", err)
		}
		if n := len(rec.calls); n != 1 {
			t.Errorf("%d calls after the cancellation, want 1", n)
		}
	})
}

// When users.lookupByEmail is refused for good (missing scope), the email
// is still looked for in the directory; if that cannot be scanned either,
// the error does not suggest passing an email (issue #7).
func TestRefusedEmailLookupInAHugeWorkspace(t *testing.T) {
	ctx := context.Background()
	rec := hugeWorkspace(directory{lookupErr: "missing_scope"})
	_, err := ResolveUserID(ctx, rec.client(), "alice@example.com")
	var rl *slackapi.RateLimitError
	if !errors.As(err, &rl) {
		t.Fatalf("err = %v, want it to wrap the rate limit of users.list", err)
	}
	if want := []string{"users.lookupByEmail", "users.list"}; !reflect.DeepEqual(rec.methods(), want) {
		t.Errorf("methods = %v, want %v: a refusal is not retried", rec.methods(), want)
	}
	for _, want := range []string{`"alice@example.com"`, "users.lookupByEmail: missing_scope", "user ID (U…)"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q: %v", want, err)
		}
	}
	if strings.Contains(err.Error(), "or the email address instead") {
		t.Errorf("error suggests passing an email for an email: %v", err)
	}

	// With a handle next to it, the advice about emails is corrected
	// rather than repeated.
	rec = hugeWorkspace(directory{lookupErr: "missing_scope"})
	_, err = ResolveUserIDs(ctx, rec.client(), []string{"alice@example.com", "Bob Builder"})
	if err == nil || strings.Contains(err.Error(), "or the email address instead") || !strings.Contains(err.Error(), "an email address is no help here") {
		t.Errorf("err = %v", err)
	}

	// A handle alone keeps the advice: an email would work for it.
	rec = hugeWorkspace(directory{})
	_, err = ResolveUserID(ctx, rec.client(), "Bob Builder")
	if err == nil || !strings.Contains(err.Error(), "or the email address instead") {
		t.Errorf("err = %v", err)
	}
}
