package audit

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Transport returns a RoundTripper that sends requests with base
// (http.DefaultTransport when nil) and records each of them, redirects
// included, as a KindFile entry for method ("files.download",
// "files.upload"). Only the host of a request is recorded, never its path
// or query, which name a file or carry a signature.
//
// A request that got an answer is recorded once its body has been read to
// the end, failed to read, or was closed, so that a transfer that breaks
// halfway is a failure and its duration the whole transfer's. When
// followsRedirects is false (the caller refuses every redirect, as an
// upload does), a 3xx answer is recorded as the failure "redirect".
func Transport(base http.RoundTripper, method string, followsRedirects bool) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return &transport{base: base, method: method, followsRedirects: followsRedirects}
}

type transport struct {
	base             http.RoundTripper
	method           string
	followsRedirects bool
}

func (t *transport) RoundTrip(req *http.Request) (*http.Response, error) {
	start := time.Now()
	resp, err := t.base.RoundTrip(req)
	e := Entry{
		TS:     start.UTC().Format(time.RFC3339Nano),
		Kind:   KindFile,
		Host:   req.URL.Host,
		Method: t.method,
	}
	if err != nil {
		e.Error = ErrorClass(req.Context(), err)
		e.DurationMS = time.Since(start).Milliseconds()
		Record(e)
		return resp, err
	}
	e.Status = resp.StatusCode
	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		e.RateLimited = true
		e.RetryAfterS = retryAfterSeconds(resp.Header.Get("Retry-After"), start)
	case resp.StatusCode >= 300 && resp.StatusCode <= 399 && !t.followsRedirects:
		e.Error = "redirect"
	}
	if resp.Body == nil || resp.Body == http.NoBody {
		e.DurationMS = time.Since(start).Milliseconds()
		Record(e)
		return resp, nil
	}
	resp.Body = &recordingBody{ReadCloser: resp.Body, ctx: req.Context(), start: start, entry: e}
	return resp, nil
}

// recordingBody writes the entry of a request once its body is done with.
type recordingBody struct {
	io.ReadCloser
	ctx   context.Context
	start time.Time

	mu    sync.Mutex
	entry Entry
	done  bool
}

func (b *recordingBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	switch {
	case err == io.EOF:
		b.finish("")
	case err != nil:
		b.finish(ErrorClass(b.ctx, err))
	}
	return n, err
}

func (b *recordingBody) Close() error {
	err := b.ReadCloser.Close()
	// Closed before the end: the caller stopped reading, which is only a
	// failure of the request if its context ended.
	class := ""
	if b.ctx.Err() != nil {
		class = ErrorClass(b.ctx, b.ctx.Err())
	}
	b.finish(class)
	return err
}

// finish records the entry, once, with the error class of the transfer
// ("" for none) unless the answer itself already failed.
func (b *recordingBody) finish(class string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.done {
		return
	}
	b.done = true
	if b.entry.Error == "" {
		b.entry.Error = class
	}
	b.entry.DurationMS = time.Since(b.start).Milliseconds()
	Record(b.entry)
}

// retryAfterSeconds reads a Retry-After header, in seconds or as an HTTP
// date (relative to now); 0 when absent or unreadable.
func retryAfterSeconds(h string, now time.Time) float64 {
	h = strings.TrimSpace(h)
	if h == "" {
		return 0
	}
	if secs, err := strconv.ParseFloat(h, 64); err == nil && secs >= 0 && secs < 1e9 {
		return secs
	}
	if at, err := http.ParseTime(h); err == nil {
		return max(0, at.Sub(now).Seconds())
	}
	return 0
}

// ErrorClass names the way a request that got no answer failed:
// "cancelled", "timeout" or "network".
func ErrorClass(ctx context.Context, err error) string {
	var ne interface{ Timeout() bool }
	switch {
	case errors.Is(ctx.Err(), context.Canceled) || errors.Is(err, context.Canceled):
		return "cancelled"
	case errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()):
		return "timeout"
	}
	return "network"
}
