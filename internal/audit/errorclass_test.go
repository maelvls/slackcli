package audit

import (
	"context"
	"errors"
	"fmt"
	"testing"
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
		{"cause wrapping DeadlineExceeded", cancelledWith(fmt.Errorf("idle: %w", context.DeadlineExceeded)), "timeout"},
		{"no cause (interrupt)", cancelledWith(nil), "cancelled"},
		{"other cause", cancelledWith(errors.New("interrupt signal received")), "cancelled"},
		{"live context", context.Background(), "cancelled"},
	} {
		if got := ErrorClass(tc.ctx, fmt.Errorf("post: %w", context.Canceled)); got != tc.want {
			t.Errorf("%s: ErrorClass = %q, want %q", tc.name, got, tc.want)
		}
	}
	// A cause on a context that is still live is not looked at.
	if got := ErrorClass(context.Background(), errors.New("connection reset")); got != "network" {
		t.Errorf("network error: %q", got)
	}
}
