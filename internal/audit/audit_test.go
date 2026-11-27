package audit

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/maelvls/slackcli/internal/testenv"
)

// useLog points the log at a fresh file below a directory that does not
// exist yet, and returns its path.
func useLog(t *testing.T) string {
	t.Helper()
	testenv.Isolate(t)
	path := filepath.Join(t.TempDir(), "sub", "audit.jsonl")
	t.Setenv("SLACKCLI_AUDIT_LOG", path)
	t.Setenv("AGENT_SLACK_AUDIT_LOG", "")
	t.Cleanup(Reset)
	return path
}

func readLines(t *testing.T, path string) []Entry {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []Entry
	for _, line := range bytes.Split(bytes.TrimSpace(data), []byte("\n")) {
		var e Entry
		if err := json.Unmarshal(line, &e); err != nil {
			t.Fatalf("line %q: %v", line, err)
		}
		out = append(out, e)
	}
	return out
}

func TestPath(t *testing.T) {
	home := testenv.Isolate(t)
	t.Setenv("SLACKCLI_AUDIT_LOG", "")
	t.Setenv("AGENT_SLACK_AUDIT_LOG", "")

	// Next to credentials.json: $XDG_CONFIG_HOME/slackcli, which Isolate
	// sets. A test binary does not write it unless asked to.
	want := filepath.Join(home, ".config", "slackcli", "audit.jsonl")
	if path, enabled := Path(); path != want || enabled {
		t.Errorf("Path() = %q, %v; want %q, false (test binary)", path, enabled, want)
	}

	// Without XDG_CONFIG_HOME: ~/.config/slackcli on every OS, not
	// ~/Library/Application Support.
	t.Setenv("XDG_CONFIG_HOME", "")
	if path, _ := Path(); path != want {
		t.Errorf("Path() without XDG_CONFIG_HOME = %q, want %q", path, want)
	}
	xdg := filepath.Join(home, "xdg")
	t.Setenv("XDG_CONFIG_HOME", xdg)
	if path, _ := Path(); path != filepath.Join(xdg, "slackcli", "audit.jsonl") {
		t.Errorf("Path() with XDG_CONFIG_HOME = %q", path)
	}

	custom := filepath.Join(home, "elsewhere.jsonl")
	t.Setenv("SLACKCLI_AUDIT_LOG", custom)
	if path, enabled := Path(); path != custom || !enabled {
		t.Errorf("Path() with the variable = %q, %v; want %q, true", path, enabled, custom)
	}

	for _, off := range []string{"off", "OFF", "0", "false", "no"} {
		t.Setenv("SLACKCLI_AUDIT_LOG", off)
		if path, enabled := Path(); enabled || path != filepath.Join(xdg, "slackcli", "audit.jsonl") {
			t.Errorf("Path() with %q = %q, %v; want the default path, false", off, path, enabled)
		}
	}
}

func TestRecordWritesOneSafeLine(t *testing.T) {
	path := useLog(t)
	SetCommand("message send", []string{
		"--safe-mode", "message", "send", "C0123ABCD", "hello world, my secret plan",
		"--thread-ts", "1700000000.123456", "--text=another secret", "--limit", "20",
		"https://acme.slack.com/archives/C0123ABCD/p1700000000123456?thread_ts=1.2&cid=x",
		"https://files.slack.com/files-pri/T0123ABCD-F0123ABCD/payroll.pdf",
		"xoxc-1234-abcd", "-n5",
	}, testFlags)
	ok := true
	Record(Entry{Kind: KindAPI, Host: "acme.slack.com", Method: "chat.postMessage", Status: 200, OK: &ok, DurationMS: 12, Attempt: 1})

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("file mode = %o, want 600", perm)
	}
	if dir, err := os.Stat(filepath.Dir(path)); err != nil || dir.Mode().Perm() != 0o700 {
		t.Errorf("directory: %v, mode %o, want 700", err, dir.Mode().Perm())
	}
	raw, _ := os.ReadFile(path)
	for _, secret := range []string{"hello", "secret", "xoxc-1234", "payroll", "thread_ts", "cid="} {
		if bytes.Contains(raw, []byte(secret)) {
			t.Errorf("the log holds %q:\n%s", secret, raw)
		}
	}

	lines := readLines(t, path)
	if len(lines) != 1 {
		t.Fatalf("%d lines, want 1", len(lines))
	}
	e := lines[0]
	if e.TS == "" || e.PID != os.Getpid() || e.PPID != os.Getppid() {
		t.Errorf("ts/pid/ppid = %q/%d/%d", e.TS, e.PID, e.PPID)
	}
	if e.Cmd != "message send" || e.Method != "chat.postMessage" || e.Status != 200 || e.OK == nil || !*e.OK || e.Attempt != 1 {
		t.Errorf("entry = %+v", e)
	}
	wantArgs := []string{
		"--safe-mode", "message", "send", "C0123ABCD", Placeholder,
		"--thread-ts", Placeholder, "--text", "--limit", Placeholder,
		"https://acme.slack.com/archives/C0123ABCD/p1700000000123456",
		"https://files.slack.com/files-pri/T0123ABCD-F0123ABCD/" + Placeholder,
		Placeholder, "-n",
	}
	if !slices.Equal(e.Args, wantArgs) {
		t.Errorf("args =\n%q\nwant\n%q", e.Args, wantArgs)
	}
}

// testFlags knows the flags of the tests: those that take a value and
// the others.
func testFlags(name string, short bool) (known, takesValue bool) {
	if short {
		return name == "n" || name == "v", name == "n"
	}
	switch name {
	case "thread-ts", "text", "limit":
		return true, true
	case "safe-mode", "force":
		return true, false
	}
	return false, false
}

func knowsAll(string, bool) (bool, bool) { return true, false }

func TestSanitizeArgsKeepsOnlyKnownFlags(t *testing.T) {
	for _, tc := range []struct {
		args, want []string
	}{
		// The value of a flag is a value, even when it looks like a flag.
		{[]string{"--text", "--confidential acquisition details"}, []string{"--text", Placeholder}},
		{[]string{"--text", "--force"}, []string{"--text", Placeholder}},
		{[]string{"-n", "-v"}, []string{"-n", Placeholder}},
		{[]string{"-n", "20"}, []string{"-n", Placeholder}},
		// Message text shaped like a number or an ID.
		{[]string{"--text", "123456"}, []string{"--text", Placeholder}},
		{[]string{"--text", "C0123ABCD"}, []string{"--text", Placeholder}},
		{[]string{"C0123ABCD", "--", "-1", "123456", "C0123ABCD", "1700000000.123456"}, []string{"C0123ABCD", "--", Placeholder, Placeholder, Placeholder, Placeholder}},
		// After "--", everything is positional.
		{[]string{"C0123ABCD", "--", "--confidential acquisition details", "--force", "-v"}, []string{"C0123ABCD", "--", Placeholder, Placeholder, Placeholder}},
		// Dash text that is no flag of the command.
		{[]string{"--> see below", "--confidential", "-1 for me", "- item"}, []string{Placeholder, Placeholder, Placeholder, Placeholder}},
		// A boolean flag takes no value: what follows is positional.
		{[]string{"--force", "C0123ABCD", "-v", "secret"}, []string{"--force", "C0123ABCD", "-v", Placeholder}},
		{[]string{"--text=--secret", "--limit=5", "-n5", "-n=5"}, []string{"--text", "--limit", "-n", "-n"}},
		{[]string{"-5"}, []string{"-5"}},
	} {
		if got := SanitizeArgs("", tc.args, testFlags); !slices.Equal(got, tc.want) {
			t.Errorf("SanitizeArgs(%q) = %q, want %q", tc.args, got, tc.want)
		}
	}
	if got := SanitizeArgs("", []string{"--force", "--text"}, nil); !slices.Equal(got, []string{Placeholder, Placeholder}) {
		t.Errorf("with no lookup: %q", got)
	}
}

func TestExistingLogIsMadePrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no mode bits")
	}
	path := useLog(t)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	Record(Entry{Kind: KindAPI, Method: "auth.test"})
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode = %o, want 600", perm)
	}
	if n := len(readLines(t, path)); n != 1 {
		t.Errorf("%d lines, want 1", n)
	}
}

func TestSanitizeArgsLimitsTheLength(t *testing.T) {
	args := make([]string, 50)
	for i := range args {
		args[i] = "--flag" + fmt.Sprint(i)
	}
	got := SanitizeArgs("", args, knowsAll)
	if len(got) != maxArgs+1 || got[maxArgs] != Placeholder+"+18" {
		t.Errorf("got %d args, last %q", len(got), got[len(got)-1])
	}
	long := "--" + strings.Repeat("x", 500)
	if got := SanitizeArgs("", []string{long}, knowsAll); len(got[0]) > maxArgLen+len(Placeholder) {
		t.Errorf("a long argument is kept whole (%d bytes)", len(got[0]))
	}
	// A word of the command later on the line is not kept for that.
	if got := SanitizeArgs("search", []string{"search", "search"}, nil); !slices.Equal(got, []string{"search", Placeholder}) {
		t.Errorf("got %q", got)
	}
}

func TestOffWritesNothing(t *testing.T) {
	path := useLog(t)
	t.Setenv("SLACKCLI_AUDIT_LOG", "off")
	Record(Entry{Kind: KindAPI, Method: "auth.test"})
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("the log was written: %v", err)
	}
	if def, _ := Path(); fileExists(def) {
		t.Errorf("the default log %s was written", def)
	}
}

func TestTestBinaryWritesNothingByDefault(t *testing.T) {
	testenv.Isolate(t)
	t.Setenv("SLACKCLI_AUDIT_LOG", "")
	t.Setenv("AGENT_SLACK_AUDIT_LOG", "")
	Record(Entry{Kind: KindAPI, Method: "auth.test"})
	if def, _ := Path(); fileExists(def) {
		t.Errorf("the default log %s was written by a test", def)
	}
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

func TestUnwritableLogIsIgnored(t *testing.T) {
	dir := t.TempDir()
	// A directory where the file should be: the write fails, quietly.
	t.Setenv("SLACKCLI_AUDIT_LOG", dir)
	Record(Entry{Kind: KindAPI, Method: "auth.test"})
}

func TestRotation(t *testing.T) {
	path := useLog(t)
	old := maxSize
	maxSize = 1000
	t.Cleanup(func() { maxSize = old })

	for i := range 30 {
		Record(Entry{Kind: KindAPI, Method: fmt.Sprintf("m%02d", i)})
	}
	cur, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	rot, err := os.Stat(path + ".1")
	if err != nil {
		t.Fatalf("not rotated: %v", err)
	}
	if cur.Size() > maxSize || rot.Size() > maxSize {
		t.Errorf("sizes %d and %d, want at most %d", cur.Size(), rot.Size(), maxSize)
	}
	if rot.Mode().Perm() != 0o600 {
		t.Errorf("rotated file mode %o", rot.Mode().Perm())
	}
	// Read sees the rotated part first, and the last line written last.
	entries, skipped, err := Read(path, zeroTime)
	if err != nil || skipped != 0 {
		t.Fatalf("Read: %v, %d skipped", err, skipped)
	}
	if len(entries) < 2 || entries[len(entries)-1].Method != "m29" {
		t.Fatalf("entries end with %+v", entries[len(entries)-1])
	}
	for i := 1; i < len(entries); i++ {
		if entries[i-1].Method >= entries[i].Method {
			t.Fatalf("out of order: %s then %s", entries[i-1].Method, entries[i].Method)
		}
	}
}

func TestConcurrentWritesKeepLinesWhole(t *testing.T) {
	path := useLog(t)
	SetCommand("message list", []string{"message", "list", "C0123ABCD"}, nil)
	var wg sync.WaitGroup
	for g := range 20 {
		wg.Go(func() {
			for i := range 25 {
				Record(Entry{Kind: KindAPI, Method: fmt.Sprintf("g%d.m%d", g, i), Host: "acme.slack.com"})
			}
		})
	}
	wg.Wait()
	if got := len(readLines(t, path)); got != 500 {
		t.Errorf("%d lines, want 500", got)
	}
}

func TestTransportRecordsHostNotPath(t *testing.T) {
	path := useLog(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/limited" {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte("data"))
	}))
	t.Cleanup(srv.Close)
	hc := &http.Client{Transport: Transport(nil, "files.download", true)}
	for _, p := range []string{"/files-pri/T1-F1/payroll.pdf?sig=abc", "/limited"} {
		resp, err := hc.Get(srv.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	// A request that gets no answer.
	if resp, err := (&http.Client{Transport: Transport(nil, "files.upload", false)}).Get("http://127.0.0.1:1/x"); err == nil {
		resp.Body.Close()
	}

	raw, _ := os.ReadFile(path)
	if bytes.Contains(raw, []byte("payroll")) || bytes.Contains(raw, []byte("sig=")) {
		t.Errorf("the log holds the path or query:\n%s", raw)
	}
	lines := readLines(t, path)
	if len(lines) != 3 {
		t.Fatalf("%d lines, want 3:\n%s", len(lines), raw)
	}
	host := strings.TrimPrefix(srv.URL, "http://")
	if e := lines[0]; e.Kind != KindFile || e.Method != "files.download" || e.Host != host || e.Status != 200 || e.Failed() {
		t.Errorf("first = %+v", e)
	}
	if e := lines[1]; e.Status != 429 || !e.RateLimited || !e.Failed() {
		t.Errorf("second = %+v", e)
	}
	if e := lines[2]; e.Method != "files.upload" || e.Error != "network" || e.Status != 0 {
		t.Errorf("third = %+v", e)
	}
}

func TestTail(t *testing.T) {
	path := useLog(t)
	if lines, err := Tail(path, 5); err != nil || len(lines) != 0 {
		t.Fatalf("Tail of no log = %q, %v", lines, err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".1", []byte("a\nb\nc\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("d\ne\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for n, want := range map[int][]string{1: {"e"}, 2: {"d", "e"}, 4: {"b", "c", "d", "e"}, 10: {"a", "b", "c", "d", "e"}} {
		if got, err := Tail(path, n); err != nil || !slices.Equal(got, want) {
			t.Errorf("Tail(%d) = %q, %v; want %q", n, got, err, want)
		}
	}
}

func BenchmarkRecord(b *testing.B) {
	b.Setenv("SLACKCLI_AUDIT_LOG", filepath.Join(b.TempDir(), "audit.jsonl"))
	SetCommand("message list", []string{"message", "list", "C0123ABCD", "--limit", "20"}, testFlags)
	b.Cleanup(Reset)
	ok := true
	for b.Loop() {
		Record(Entry{Kind: KindAPI, Host: "acme.slack.com", Method: "conversations.history", Status: 200, OK: &ok, Attempt: 1})
	}
}
