package audit

import (
	"context"
	"errors"
	"net/http"
	"time"
)

// Transport returns a RoundTripper that sends requests with base
// (http.DefaultTransport when nil) and records each of them, redirects
// included, as a KindFile entry for method ("files.download",
// "files.upload"). Only the host of a request is recorded, never its path
// or query, which name a file or carry a signature.
func Transport(base http.RoundTripper, method string) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return &transport{base: base, method: method}
}

type transport struct {
	base   http.RoundTripper
	method string
}

func (t *transport) RoundTrip(req *http.Request) (*http.Response, error) {
	start := time.Now()
	resp, err := t.base.RoundTrip(req)
	e := Entry{
		TS:         start.UTC().Format(time.RFC3339Nano),
		Kind:       KindFile,
		Host:       req.URL.Host,
		Method:     t.method,
		DurationMS: time.Since(start).Milliseconds(),
	}
	switch {
	case err != nil:
		e.Error = ErrorClass(req.Context(), err)
	default:
		e.Status = resp.StatusCode
		if resp.StatusCode == http.StatusTooManyRequests {
			e.RateLimited = true
		}
	}
	Record(e)
	return resp, err
}

// ErrorClass names the way a request that got no answer failed:
// "cancelled", "timeout" or "network". A request whose context was
// cancelled because of a timeout, given as the cause of the cancellation
// (context.WithCancelCause, as a stall watchdog does), is a "timeout": the
// cause is one when it is context.DeadlineExceeded itself or has a Timeout
// method that returns true. The cause itself, not what it wraps: a request
// cancelled because another one failed with a timeout (a fan-out cancels
// the others with the first error) was cancelled, not timed out.
func ErrorClass(ctx context.Context, err error) string {
	if ctx.Err() != nil && isTimeoutCause(context.Cause(ctx)) {
		return "timeout"
	}
	var ne interface{ Timeout() bool }
	switch {
	case ctx.Err() == context.Canceled || errors.Is(err, context.Canceled):
		return "cancelled"
	case errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()):
		return "timeout"
	}
	return "network"
}

// isTimeoutCause tells whether cause, that of a cancellation, is itself a
// timeout (see ErrorClass).
func isTimeoutCause(cause error) bool {
	if cause == context.DeadlineExceeded {
		return true
	}
	t, ok := cause.(interface{ Timeout() bool })
	return ok && t.Timeout()
}
