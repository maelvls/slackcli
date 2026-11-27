//go:build unix

package audit

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// A log that another process keeps locked (one stopped while it writes or
// rotates) costs a command at most lockWait: the line is dropped.
func TestLockedLogDoesNotHang(t *testing.T) {
	path := useLog(t)
	old := lockWait
	lockWait = 20 * time.Millisecond
	t.Cleanup(func() { lockWait = old })

	Record(Entry{Kind: KindAPI, Method: "first"})
	// flock locks belong to an open file: another one conflicts, as
	// another process's would.
	holder, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	if err := syscall.Flock(int(holder.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	Record(Entry{Kind: KindAPI, Method: "dropped"})
	if d := time.Since(start); d > time.Second {
		t.Fatalf("Record waited %s on a locked log", d)
	}
	if n := len(readLines(t, path)); n != 1 {
		t.Errorf("%d lines, want 1 (the second dropped)", n)
	}

	// Once the lock is gone, lines are written again.
	_ = syscall.Flock(int(holder.Fd()), syscall.LOCK_UN)
	Record(Entry{Kind: KindAPI, Method: "third"})
	if n := len(readLines(t, path)); n != 2 {
		t.Errorf("%d lines, want 2", n)
	}
}

// A secret in the error code an answer gave is redacted.
func TestErrorIsRedacted(t *testing.T) {
	path := useLog(t)
	Record(Entry{Kind: KindAPI, Method: "m", Error: "bad xoxb-1234-secret"})
	lines := readLines(t, path)
	if len(lines) != 1 || lines[0].Error != "bad [REDACTED]" {
		t.Fatalf("lines = %+v", lines)
	}
	_ = filepath.Base(path)
}
