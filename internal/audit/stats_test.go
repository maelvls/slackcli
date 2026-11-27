package audit

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

var zeroTime time.Time

func TestSummarize(t *testing.T) {
	yes, no := true, false
	at := func(s string) string { return "2026-11-27T" + s + "Z" }
	entries := []Entry{
		{TS: at("10:00:00"), PID: 1, Parent: "sh", Ancestors: []string{"node", "launchd"}, Cmd: "inbox", Method: "search.messages", Host: "a.slack.com", Status: 200, OK: &yes},
		{TS: at("10:00:20"), PID: 1, Parent: "node", Cmd: "inbox", Method: "users.info", Host: "a.slack.com", Status: 200, OK: &yes},
		{TS: at("10:00:40"), PID: 2, Parent: "node", Cmd: "inbox", Method: "search.messages", Host: "a.slack.com", Status: 429, RateLimited: true, RetryAfterS: 30, Error: "ratelimited"},
		{TS: at("10:00:59.999"), PID: 3, Parent: "node", Cmd: "inbox", Method: "search.messages", Host: "a.slack.com", Status: 200, OK: &no, Error: "invalid_auth"},
		{TS: at("10:01:00"), PID: 4, Parent: "zsh", Cmd: "message list", Method: "conversations.history", Host: "a.slack.com", Status: 200, OK: &yes},
		{TS: at("11:30:00"), PID: 5, Parent: "zsh", Cmd: "message get", Kind: KindFile, Method: "files.download", Host: "files.slack.com", Status: 200},
		{TS: at("11:31:00"), PID: 5, Parent: "zsh", Cmd: "message get", Kind: KindFile, Method: "files.download", Host: "files.slack.com", Error: "network"},
	}
	s := Summarize(entries)

	if s.Total != 7 || s.Processes != 5 || s.Errors != 3 || s.RateLimited != 1 {
		t.Errorf("total=%d processes=%d errors=%d rate_limited=%d", s.Total, s.Processes, s.Errors, s.RateLimited)
	}
	if s.From != at("10:00:00") || s.To != at("11:31:00") {
		t.Errorf("from %s to %s", s.From, s.To)
	}
	// 10:00:00 to 10:00:59.999 is 4 requests in a minute; 10:01:00 is
	// the 61st second.
	if s.PeakMinute != (Bucket{Start: at("10:00:00"), Count: 4}) {
		t.Errorf("peak minute = %+v", s.PeakMinute)
	}
	wantMethods := []Count{
		{Key: "search.messages", Count: 3, Errors: 2},
		{Key: "files.download", Count: 2, Errors: 1},
		{Key: "conversations.history", Count: 1},
		{Key: "users.info", Count: 1},
	}
	if !reflect.DeepEqual(s.ByMethod, wantMethods) {
		t.Errorf("by_method = %+v", s.ByMethod)
	}
	wantSources := []Source{
		{Program: "node", Cmd: "inbox", Count: 4, Processes: 3},
		{Program: "zsh", Cmd: "message get", Count: 2, Processes: 1},
		{Program: "zsh", Cmd: "message list", Count: 1, Processes: 1},
	}
	if !reflect.DeepEqual(s.BySource, wantSources) {
		t.Errorf("by_source = %+v", s.BySource)
	}
	wantErrors := []Count{
		{Key: "invalid_auth", Count: 1, Errors: 1},
		{Key: "network", Count: 1, Errors: 1},
		{Key: "ratelimited", Count: 1, Errors: 1},
	}
	if !reflect.DeepEqual(s.ByError, wantErrors) {
		t.Errorf("by_error = %+v", s.ByError)
	}
	wantHours := []Bucket{{Start: at("10:00:00"), Count: 5}, {Start: at("11:00:00"), Count: 2}}
	if !reflect.DeepEqual(s.ByHour, wantHours) {
		t.Errorf("by_hour = %+v", s.ByHour)
	}
	if !reflect.DeepEqual(s.ByHost, []Count{{Key: "a.slack.com", Count: 5, Errors: 2}, {Key: "files.slack.com", Count: 2, Errors: 1}}) {
		t.Errorf("by_host = %+v", s.ByHost)
	}

	if empty := Summarize(nil); empty.Total != 0 || empty.PeakMinute.Count != 0 || empty.From != "" {
		t.Errorf("Summarize(nil) = %+v", empty)
	}
}

func TestProgramLooksThroughShellsAndWrappers(t *testing.T) {
	for _, tc := range []struct {
		parent    string
		ancestors []string
		want      string
	}{
		{"node", []string{"launchd"}, "node"},
		{"sh", []string{"node", "launchd"}, "node"},
		{"timeout", []string{"env", "bash", "-zsh", "kb-daemon", "launchd"}, "kb-daemon"},
		{"zsh", []string{"-zsh", "bash"}, "zsh"}, // only shells: the direct parent
		{"zsh", nil, "zsh"},
		{"", nil, ""},
	} {
		if got := Program(Entry{Parent: tc.parent, Ancestors: tc.ancestors}); got != tc.want {
			t.Errorf("Program(%q, %q) = %q, want %q", tc.parent, tc.ancestors, got, tc.want)
		}
	}
}

func TestReadFiltersAndSkips(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.jsonl")
	lines := []string{
		`{"ts":"2026-11-26T09:00:00Z","pid":1,"kind":"api","method":"old"}`,
		`not json`,
		`{"ts":"not a time","method":"x"}`,
		``,
		`{"ts":"2026-11-27T09:00:00.5Z","pid":1,"kind":"api","method":"new"}`,
		`{"ts":"2026-11-27T09:00:01Z","pid":1,"kind":"api","method":"newer"}`, // no final newline
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o600); err != nil {
		t.Fatal(err)
	}
	since := time.Date(2026, 11, 27, 0, 0, 0, 0, time.UTC)
	entries, skipped, err := Read(path, since)
	if err != nil {
		t.Fatal(err)
	}
	if skipped != 2 || len(entries) != 2 || entries[0].Method != "new" || entries[1].Method != "newer" {
		t.Errorf("entries = %+v, skipped = %d", entries, skipped)
	}
	if entries, _, err := Read(filepath.Join(dir, "none.jsonl"), zeroTime); err != nil || len(entries) != 0 {
		t.Errorf("Read of no log = %v, %v", entries, err)
	}
}

func TestParseSince(t *testing.T) {
	for raw, want := range map[string]time.Duration{"1h": time.Hour, "90m": 90 * time.Minute, "24h": 24 * time.Hour, "7d": 7 * 24 * time.Hour, "0.5d": 12 * time.Hour} {
		if got, err := ParseSince(raw); err != nil || got != want {
			t.Errorf("ParseSince(%q) = %v, %v; want %v", raw, got, err, want)
		}
	}
	for _, raw := range []string{"", "0", "-1h", "d", "xd", "1y", "0d"} {
		if _, err := ParseSince(raw); err == nil {
			t.Errorf("ParseSince(%q) succeeded", raw)
		}
	}
}
