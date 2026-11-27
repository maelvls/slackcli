package trace

import (
	"bytes"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestSummaryGroupsSpansAndNamesWhatIsRunning(t *testing.T) {
	t.Setenv("SLACKCLI_DEBUG", "")
	t.Setenv("AGENT_SLACK_DEBUG", "")
	var out bytes.Buffer
	Reset(&out)
	t.Cleanup(func() { Reset(nil) })

	if got := Summary(); got != "" {
		t.Errorf("empty summary = %q", got)
	}
	Begin("api users.info", "").End("")
	Begin("api users.info", "").EndErr(errors.New("ratelimited"))
	Begin("keychain", "get").End("")
	Begin("api conversations.history", "attempt 1")

	got := Summary()
	for _, want := range []string{"time spent: ", "api users.info ", "(2 calls, 1 failed)", "keychain ", "still running: api conversations.history attempt 1 for "} {
		if !strings.Contains(got, want) {
			t.Errorf("summary %q lacks %q", got, want)
		}
	}
	if out.Len() != 0 {
		t.Errorf("without SLACKCLI_DEBUG nothing is written, got %q", out.String())
	}
}

func TestLiveTraceWritesOneLinePerSpan(t *testing.T) {
	t.Setenv("SLACKCLI_DEBUG", "1")
	var out bytes.Buffer
	Reset(&out)
	t.Cleanup(func() { Reset(nil) })

	s := Begin("api chat.postMessage", "attempt 1")
	s.End("HTTP 200")
	s.End("twice") // a span ends once
	Begin("download", "F123").Fail("HTTP 404")
	Logf("rate limited for %s", "2s")

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("got %d lines: %q", len(lines), out.String())
	}
	for i, want := range []string{"api chat.postMessage attempt 1: ", "download F123: ", "rate limited for 2s"} {
		if !strings.HasPrefix(lines[i], "[slackcli +") || !strings.Contains(lines[i], want) {
			t.Errorf("line %d = %q, want it to contain %q", i, lines[i], want)
		}
	}
	if !strings.HasSuffix(lines[0], ", HTTP 200") || !strings.HasSuffix(lines[1], ", failed: HTTP 404") {
		t.Errorf("notes: %q", lines)
	}
}

// What an error quotes is not for a trace: no path or query of a URL, no
// bytes of a response, no name.
func TestSafeNote(t *testing.T) {
	for in, want := range map[string]string{
		"context canceled": "context canceled",
		`Post "https://slack.com/api/conversations.history?token=xoxc-1": EOF`:                                 `Post "…": EOF`,
		"fetching https://files.slack.com/files-pri/T1-F2/secret-plan.pdf?t=xoxe-1 failed":                     "fetching https://files.slack.com/… failed",
		`Downloaded HTML instead of file (auth likely failed). First bytes: "<html><title>Acme \"x\"</title>"`: `Downloaded HTML instead of file (auth likely failed). First bytes: "…"`,
		`Invalid file URL "https://example.com/a b`:                                                            `Invalid file URL "…"`,
		"writing /Users/me/Library/Caches/slackcli/downloads/Secret plan: v2.pdf: no space left on device":     "writing …: no space left on device",
		`open C:\Users\me\Secret plan.pdf: Access is denied.`:                                                  "open …: Access is denied.",
		"rename ~/x/Secret plan.pdf.tmp /tmp/Secret plan.pdf: permission denied":                               "rename …: permission denied",
		"HTTP 200, channel_not_found":                                                                          "HTTP 200, channel_not_found",
		"no answer after 20s (locked, or waiting on a prompt?)":                                                "no answer after 20s (locked, or waiting on a prompt?)",
		"first line\nsecond line":                                                                              "first line",
		strings.Repeat("é", 200):                                                                               strings.Repeat("é", 120) + "…",
	} {
		if got := SafeNote(in); got != want {
			t.Errorf("SafeNote(%q) = %q, want %q", in, got, want)
		}
	}

	t.Setenv("SLACKCLI_DEBUG", "1")
	var out bytes.Buffer
	Reset(&out)
	t.Cleanup(func() { Reset(nil) })
	Begin("download", "").EndErr(errors.New(`Get "https://files.slack.com/files-pri/T1-F2/secret-plan.pdf?t=xoxe-1": EOF`))
	if got := out.String(); strings.Contains(got, "secret-plan") || strings.Contains(got, "xoxe-1") || !strings.Contains(got, `download: 0s, failed: Get "…": EOF`) {
		t.Errorf("trace = %q", got)
	}
}

// blockedWriter is a stderr that is a full pipe nobody reads.
type blockedWriter struct{ writing, release chan struct{} }

func (w blockedWriter) Write(p []byte) (int, error) {
	select {
	case w.writing <- struct{}{}:
	case <-w.release:
	}
	<-w.release
	return len(p), nil
}

// A trace line that cannot be written does not hold the record: the
// message of a timeout is made from it.
func TestSummaryDoesNotWaitForALineBeingWritten(t *testing.T) {
	t.Setenv("SLACKCLI_DEBUG", "1")
	out := blockedWriter{writing: make(chan struct{}), release: make(chan struct{})}
	Reset(out)
	t.Cleanup(func() { Reset(nil) })
	// The writes are let through, and waited for, before the test ends
	// (cleanups run last registered first, so before the Reset above):
	// a line still on its way would otherwise end up in the trace of the
	// next test.
	var writers sync.WaitGroup
	t.Cleanup(func() {
		close(out.release)
		writers.Wait()
	})

	writers.Go(func() { Begin("api users.info", "").End("HTTP 200") })
	<-out.writing                           // the line is being written, and is stuck
	writers.Go(func() { Logf("an event") }) // waits its turn behind it

	got := make(chan string)
	go func() {
		Begin("keychain", "set") // nor does it keep spans from starting
		got <- Summary()
	}()
	select {
	case summary := <-got:
		if !strings.Contains(summary, "api users.info ") || !strings.Contains(summary, "still running: keychain set for ") {
			t.Errorf("summary = %q", summary)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Summary waits for the write")
	}
}

// The lines of the live trace are written one at a time and in the order
// of the events, also when the writer is slow and the events come from
// several goroutines.
func TestLiveTraceLinesAreWrittenInOrderAndOneAtATime(t *testing.T) {
	t.Setenv("SLACKCLI_DEBUG", "1")
	t.Cleanup(func() { Reset(nil) })

	// A plain buffer, which must not be written to by two goroutines at
	// once: the race detector is the judge.
	var buf bytes.Buffer
	Reset(&buf)
	var wg sync.WaitGroup
	for i := range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			Begin("api users.info", "").End("HTTP 200")
			Logf("event %d", i)
		}()
	}
	wg.Wait()
	if n := strings.Count(buf.String(), "\n"); n != 100 {
		t.Errorf("%d lines, want 100", n)
	}

	// Events that happen while a line is being written wait their turn,
	// and come out in the order they happened in.
	out := &slowWriter{writing: make(chan struct{}, 1), release: make(chan struct{})}
	Reset(out)
	queued := func() int {
		rec.mu.Lock()
		defer rec.mu.Unlock()
		return len(rec.pending)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		Logf("event 0")
	}()
	<-out.writing // "event 0" is being written, and is stuck
	for i := 1; i <= 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			Logf("event %d", i)
		}()
		for deadline := time.Now().Add(5 * time.Second); queued() != i; time.Sleep(time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("event %d was not queued", i)
			}
		}
	}
	close(out.release)
	wg.Wait()
	var got []string
	for _, line := range strings.Split(strings.TrimSuffix(out.buf.String(), "\n"), "\n") {
		_, event, _ := strings.Cut(line, "] ")
		got = append(got, event)
	}
	if want := []string{"event 0", "event 1", "event 2", "event 3", "event 4", "event 5"}; !slices.Equal(got, want) {
		t.Errorf("lines = %q, want %q", got, want)
	}
}

// slowWriter is stuck in its first write until released.
type slowWriter struct {
	buf              bytes.Buffer
	writing, release chan struct{}
}

func (w *slowWriter) Write(p []byte) (int, error) {
	select {
	case w.writing <- struct{}{}:
	default:
	}
	<-w.release
	return w.buf.Write(p)
}
