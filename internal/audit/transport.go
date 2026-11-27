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
// "cancelled", "timeout" or "network".
func ErrorClass(ctx context.Context, err error) string {
	var ne interface{ Timeout() bool }
	switch {
	case ctx.Err() == context.Canceled || errors.Is(err, context.Canceled):
		return "cancelled"
	case errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()):
		return "timeout"
	}
	return "network"
}
