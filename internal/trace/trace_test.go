package trace

import (
	"bytes"
	"errors"
	"strings"
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
	w.writing <- struct{}{}
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

	go Begin("api users.info", "").End("HTTP 200")
	<-out.writing
	go Logf("an event")
	<-out.writing

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
	close(out.release)
}
