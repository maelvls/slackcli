package slackapi

import (
	"context"
	"time"
)

type commandDeadlineKey struct{}

// WithCommandDeadline tells whoever works under ctx when the command it
// works for will be cut off (the command timeout of the root). deadline
// is asked each time, and answers false when there is no limit (any
// more): a command that turns out to wait on a human has none.
//
// It is not a context deadline, on purpose: the command timeout is kept
// by the root's watchdog, which reports it in its own words, and ctx is
// cancelled, not expired, when it strikes.
func WithCommandDeadline(ctx context.Context, deadline func() (time.Time, bool)) context.Context {
	return context.WithValue(ctx, commandDeadlineKey{}, deadline)
}

// TimeLeft returns how long the command of ctx has left before it is cut
// off, and whether it has a limit at all (see WithCommandDeadline). It
// is for deciding whether something that may last (one more try of a
// call that timed out) is worth starting.
func TimeLeft(ctx context.Context) (left time.Duration, limited bool) {
	deadline, _ := ctx.Value(commandDeadlineKey{}).(func() (time.Time, bool))
	if deadline == nil {
		return 0, false
	}
	at, limited := deadline()
	if !limited {
		return 0, false
	}
	return time.Until(at), true
}
