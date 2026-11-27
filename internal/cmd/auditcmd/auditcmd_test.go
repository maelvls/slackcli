package auditcmd

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/maelvls/slackcli/internal/clictx"
	"github.com/maelvls/slackcli/internal/testenv"
)

// run runs "audit <args>" and returns what it printed.
func run(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	rt := &clictx.Context{Stdout: &out, Stderr: io.Discard}
	cmd := New(rt)
	cmd.SetArgs(args)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	err := cmd.Execute()
	return out.String(), err
}

func writeLog(t *testing.T, lines ...string) string {
	t.Helper()
	testenv.Isolate(t)
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	t.Setenv("SLACKCLI_AUDIT_LOG", path)
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestPath(t *testing.T) {
	path := writeLog(t)
	out, err := run(t, "path")
	if err != nil {
		t.Fatal(err)
	}
	want, _ := json.Marshal(struct {
		Path    string `json:"path"`
		Enabled bool   `json:"enabled"`
	}{path, true})
	if strings.TrimSpace(out) != string(want) {
		t.Errorf("out = %s, want %s", out, want)
	}
	t.Setenv("SLACKCLI_AUDIT_LOG", "off")
	if out, _ := run(t, "path"); !strings.Contains(out, `"enabled":false`) {
		t.Errorf("off: %s", out)
	}
}

func TestTail(t *testing.T) {
	writeLog(t, `{"n":1}`, `{"n":2}`, `{"n":3}`)
	if out, err := run(t, "tail", "-n", "2"); err != nil || out != "{\"n\":2}\n{\"n\":3}\n" {
		t.Errorf("tail -n 2 = %q, %v", out, err)
	}
	if out, err := run(t, "tail"); err != nil || strings.Count(out, "\n") != 3 {
		t.Errorf("tail = %q, %v", out, err)
	}
	if _, err := run(t, "tail", "-n", "0"); err == nil {
		t.Error("tail -n 0 succeeded")
	}
}

func TestStats(t *testing.T) {
	now = func() time.Time { return time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC) }
	t.Cleanup(func() { now = time.Now })
	path := writeLog(t,
		`{"ts":"2026-11-25T10:00:00Z","pid":9,"kind":"api","method":"too.old"}`,
		`{"ts":"2026-11-27T11:10:00Z","pid":1,"parent":"node","cmd":"inbox","kind":"api","method":"search.messages","status":200,"ok":true}`,
		`{"ts":"2026-11-27T11:10:01Z","pid":1,"parent":"node","cmd":"inbox","kind":"api","method":"search.messages","status":429,"rate_limited":true,"retry_after_s":30,"error":"ratelimited"}`,
		`garbage`,
	)
	out, err := run(t, "stats", "--since", "24h")
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if got["path"] != path || got["since"] != "2026-11-26T12:00:00Z" || got["total"] != 2.0 || got["rate_limited"] != 1.0 || got["errors"] != 1.0 || got["skipped"] != 1.0 {
		t.Errorf("stats = %s", out)
	}
	if !strings.Contains(out, `"by_source":[{"parent":"node","cmd":"inbox","count":2,"processes":1}]`) {
		t.Errorf("by_source: %s", out)
	}

	// Nothing in the period: the lists are there, empty.
	out, err = run(t, "stats", "--since", "1m")
	if err != nil || !strings.Contains(out, `"total":0`) || !strings.Contains(out, `"by_method":[]`) {
		t.Errorf("empty stats = %s, %v", out, err)
	}
	if _, err := run(t, "stats", "--since", "soon"); err == nil {
		t.Error("--since soon succeeded")
	}
}
