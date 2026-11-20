package trace

import (
	"bytes"
	"errors"
	"strings"
	"testing"
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
