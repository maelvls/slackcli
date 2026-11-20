//go:build unix

package auth

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/maelvls/slackcli/internal/trace"
)

// A security(1) that the timeout killed must not keep the call waiting for
// what it started: a child that inherited its output (as a helper showing
// a prompt would) keeps the pipes open for as long as it lives, and
// without a WaitDelay the call would last as long.
func TestRunSecurityDoesNotWaitForWhatAKilledSecurityLeftBehind(t *testing.T) {
	dir := t.TempDir()
	started := filepath.Join(dir, "started")
	// The child outlives the test by five seconds at most.
	script := "#!/bin/sh\nsleep 5 &\necho > '" + started + "'\nsleep 5\n"
	if err := os.WriteFile(filepath.Join(dir, "security"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	// The timeout, once the child is there.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// On a loaded Mac the first run of a script that was just written
	// can take seconds: wait for it as long as it takes, within reason.
	var (
		killed  time.Time
		running bool
	)
	go func() {
		for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
			if _, err := os.Stat(started); err == nil {
				running = true
				break
			}
		}
		killed = time.Now()
		cancel()
	}()
	res := runSecurity(ctx, "", "find-generic-password", "-s", "service", "-a", "account", "-w")
	returned := time.Now()
	if ctx.Err() == nil {
		t.Fatalf("the fake security ended on its own: %+v", res)
	}
	if !running {
		t.Skip("the fake security did not start its child within 30s: nothing was tested")
	}
	// Without the WaitDelay (1 s) it is the 5 s of the child.
	if elapsed := returned.Sub(killed); elapsed > 3*time.Second {
		t.Errorf("returned %s after security was killed: it waited for its child", elapsed)
	}
	if _, err := keychainGetResult(res, true, time.Second); err == nil {
		t.Error("a keychain that did not answer is not an absent secret")
	}
}

// The trace says how a keychain call ended, by its status and nothing
// else: a failure is not a success, an absent item is not a failure, and
// what security(1) wrote is not shown.
func TestRunSecurityTracesHowTheCallEnded(t *testing.T) {
	dir := t.TempDir()
	// The fake exits with the status named by its last argument, or, as
	// `security -i`, reports a failed sub-command and exits with 0.
	script := `#!/bin/sh
echo "secret on stdout"
echo "secret on stderr" >&2
if [ "$1" = "-i" ]; then exit 0; fi
for last; do :; done
exit "$last"
`
	if err := os.WriteFile(filepath.Join(dir, "security"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	// Registered first, so that it runs once the variable is back to
	// what it was: Reset reads it.
	t.Cleanup(func() { trace.Reset(nil) })
	t.Setenv("SLACKCLI_DEBUG", "1")
	var out bytes.Buffer
	trace.Reset(&out)

	ctx := context.Background()
	runSecurity(ctx, "", "find-generic-password", "0")
	runSecurity(ctx, "", "find-generic-password", "44")
	runSecurity(ctx, "", "find-generic-password", "51")
	runSecurity(ctx, "add-generic-password …\n", "-i")
	t.Setenv("PATH", t.TempDir()) // no security at all
	runSecurity(ctx, "", "delete-generic-password", "0")

	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	want := []string{
		`keychain find-generic-password: \S+$`,
		`keychain find-generic-password: \S+, no such item$`,
		`keychain find-generic-password: \S+, failed: security exited with status 51$`,
		`keychain set: \S+, failed: security reported an error$`,
		`keychain delete-generic-password: \S+, failed: security could not be run$`,
	}
	if len(lines) != len(want) {
		t.Fatalf("trace:\n%s", out.String())
	}
	for i, re := range want {
		if !regexp.MustCompile(re).MatchString(lines[i]) {
			t.Errorf("line %d = %q, want %s", i, lines[i], re)
		}
	}
	if strings.Contains(out.String(), "secret") {
		t.Errorf("the trace shows the output of security:\n%s", out.String())
	}
	if got := trace.Summary(); !strings.Contains(got, "(5 calls, 3 failed)") {
		t.Errorf("summary = %q", got)
	}
}
