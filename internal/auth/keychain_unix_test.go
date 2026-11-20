//go:build unix

package auth

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
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
