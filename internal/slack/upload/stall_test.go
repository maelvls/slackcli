package upload

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/maelvls/slackcli/internal/slackapi"
)

// withStallTimeout shortens the stall timeout for the test.
func withStallTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	old, oldTail := stallTimeout, stallTailMin
	stallTimeout = func() time.Duration { return d }
	stallTailMin = d
	t.Cleanup(func() { stallTimeout, stallTailMin = old, oldTail })
}

// byteServer is a pre-signed upload endpoint served by handler, and a
// client for it. handler is given a channel closed when the test ends.
func byteServer(t *testing.T, handler func(w http.ResponseWriter, r *http.Request, done <-chan struct{})) (*httptest.Server, *slackapi.Client) {
	t.Helper()
	done := make(chan struct{})
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler(w, r, done)
	}))
	t.Cleanup(srv.Close)
	// Runs before srv.Close (cleanups run last-in first-out), which waits
	// for the handlers.
	t.Cleanup(func() { close(done) })
	client := slackapi.NewFake(func(context.Context, string, map[string]any) (map[string]any, error) {
		return map[string]any{"ok": true}, nil
	}, slackapi.WithHTTPClient(srv.Client()))
	return srv, client
}

// An endpoint that takes the whole file and then never answers must not
// hang the command: the upload fails once the stall timeout went by
// without an answer, and says what happened and what to do.
func TestUploadFailsWhenTheAnswerNeverComes(t *testing.T) {
	withStallTimeout(t, 200*time.Millisecond)
	srv, client := byteServer(t, func(w http.ResponseWriter, r *http.Request, done <-chan struct{}) {
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-done:
		case <-r.Context().Done():
		}
	})
	content := strings.Repeat("x", 10_000)
	start := time.Now()
	err := postBytes(context.Background(), client, srv.URL+"/f", strings.NewReader(content), int64(len(content)))
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("no error")
	}
	if elapsed > 3*time.Second {
		t.Errorf("failed after %s, want about the stall timeout", elapsed)
	}
	for _, want := range []string{"the upload stalled", "no progress for 200ms", "all 10000 bytes were handed to the network", "never answered", "run the command again", "SLACKCLI_API_TIMEOUT_MS"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not say %q: %v", want, err)
		}
	}
	if strings.Contains(err.Error(), "/f") {
		t.Errorf("error names the upload URL: %v", err)
	}
}

// An endpoint that stops taking the bytes of the file, as a dead
// connection does once the socket buffers are full, fails the same way,
// and says how far the upload got.
func TestUploadFailsWhenTheBodyStops(t *testing.T) {
	withStallTimeout(t, 300*time.Millisecond)
	srv, client := byteServer(t, func(w http.ResponseWriter, r *http.Request, done <-chan struct{}) {
		// Never reads the body.
		select {
		case <-done:
		case <-r.Context().Done():
		}
	})
	// Far more than the socket buffers of a loopback connection hold.
	const size = 256 << 20
	body := io.LimitReader(zeros{}, size)
	start := time.Now()
	err := postBytes(context.Background(), client, srv.URL+"/f", body, size)
	if err == nil || !strings.Contains(err.Error(), "the upload stalled") || !strings.Contains(err.Error(), "of 256.0 MB were sent") {
		t.Fatalf("err = %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("failed after %s, want about the stall timeout", elapsed)
	}
}

type zeros struct{}

func (zeros) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

// A slow upload that keeps making progress is never cut short, however
// long it takes in all.
func TestSlowButProgressingUploadDoesNotTimeOut(t *testing.T) {
	withStallTimeout(t, 300*time.Millisecond)
	got := make(chan string, 1)
	srv, client := byteServer(t, func(w http.ResponseWriter, r *http.Request, _ <-chan struct{}) {
		b, _ := io.ReadAll(r.Body)
		got <- string(b)
		// The answer itself takes less than the stall timeout.
		time.Sleep(100 * time.Millisecond)
	})
	// 10 chunks 100ms apart: about three times the stall timeout in all.
	body := &slowReader{chunks: 10, every: 100 * time.Millisecond}
	if err := postBytes(context.Background(), client, srv.URL+"/f", body, 10); err != nil {
		t.Fatal(err)
	}
	if b := <-got; b != strings.Repeat("s", 10) {
		t.Errorf("received %q", b)
	}
}

type slowReader struct {
	chunks int
	every  time.Duration
}

func (s *slowReader) Read(p []byte) (int, error) {
	if s.chunks == 0 {
		return 0, io.EOF
	}
	time.Sleep(s.every)
	s.chunks--
	p[0] = 's'
	return 1, nil
}

// After the last byte was taken, the answer gets the longer tail wait,
// not the stall timeout: the end of a large file may still be draining
// from the socket buffers of a slow link.
func TestAnswerAfterTheLastByteGetsTheTailWait(t *testing.T) {
	withStallTimeout(t, 200*time.Millisecond)
	stallTailMin = 2 * time.Second
	srv, client := byteServer(t, func(w http.ResponseWriter, r *http.Request, _ <-chan struct{}) {
		_, _ = io.Copy(io.Discard, r.Body)
		// Longer than the stall timeout, shorter than the tail.
		time.Sleep(600 * time.Millisecond)
	})
	content := strings.Repeat("x", 10_000)
	if err := postBytes(context.Background(), client, srv.URL+"/f", strings.NewReader(content), int64(len(content))); err != nil {
		t.Fatal(err)
	}
}
