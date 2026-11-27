package audit

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

type timeoutCause struct{}

func (timeoutCause) Error() string { return "stalled" }
func (timeoutCause) Timeout() bool { return true }

// A request cancelled because of a timeout, given as the cause of the
// cancellation (a stall watchdog), is a "timeout"; one cancelled by the
// user, or for another cause, stays "cancelled".
func TestErrorClassLooksAtTheCause(t *testing.T) {
	cancelledWith := func(cause error) context.Context {
		ctx, cancel := context.WithCancelCause(context.Background())
		cancel(cause)
		return ctx
	}
	for _, tc := range []struct {
		name string
		ctx  context.Context
		want string
	}{
		{"cause with Timeout", cancelledWith(timeoutCause{}), "timeout"},
		{"cause DeadlineExceeded", cancelledWith(context.DeadlineExceeded), "timeout"},
		{"deadline", expired(t), "timeout"},
		// A fan-out cancels the other requests with the error of the
		// first one: they were cancelled, not timed out.
		{"cause wrapping a timeout", cancelledWith(fmt.Errorf("calling Slack: %w", timeoutCause{})), "cancelled"},
		{"no cause (interrupt)", cancelledWith(nil), "cancelled"},
		{"other cause", cancelledWith(errors.New("interrupt signal received")), "cancelled"},
		{"live context", context.Background(), "cancelled"},
	} {
		if got := ErrorClass(tc.ctx, fmt.Errorf("post: %w", context.Canceled)); got != tc.want {
			t.Errorf("%s: ErrorClass = %q, want %q", tc.name, got, tc.want)
		}
	}
	// An error on a live context, with no timeout in it, is a network one.
	if got := ErrorClass(context.Background(), errors.New("connection reset")); got != "network" {
		t.Errorf("network error: %q", got)
	}
}

// expired is a context whose deadline has passed.
func expired(t *testing.T) context.Context {
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	t.Cleanup(cancel)
	return ctx
}
