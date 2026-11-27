package cachefile

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// shortTimings makes the lock give up and take over quickly.
func shortTimings(t *testing.T) {
	t.Helper()
	wait, stale, poll := lockWait, lockStale, lockPoll
	lockWait, lockStale, lockPoll = 200*time.Millisecond, time.Second, time.Millisecond
	t.Cleanup(func() { lockWait, lockStale, lockPoll = wait, stale, poll })
}

func readCounter(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatalf("counter file: %q", data)
	}
	return n
}

// Concurrent invocations each read the file, add to it and write it back:
// the lock makes those three steps one, so that no update is lost.
func TestLockSerializesReadModifyWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache", "counter.json")
	const writers = 40
	var wg sync.WaitGroup
	for range writers {
		wg.Go(func() {
			unlock := Lock(path)
			defer unlock()
			n := readCounter(t, path)
			time.Sleep(100 * time.Microsecond) // widen the window
			Write(path, []byte(strconv.Itoa(n+1)+"\n"))
		})
	}
	wg.Wait()
	if n := readCounter(t, path); n != writers {
		t.Errorf("counter = %d, want %d: updates were lost", n, writers)
	}
	if entries, _ := os.ReadDir(filepath.Dir(path)); len(entries) != 1 {
		t.Errorf("files left behind: %v", entries)
	}
}

// A lock that is held for too long never blocks a command for more than
// lockWait: it goes on without the lock, as it did before there was one.
func TestLockGivesUpAfterAWhile(t *testing.T) {
	shortTimings(t)
	path := filepath.Join(t.TempDir(), "names.json")
	held := Lock(path)
	defer held()
	start := time.Now()
	unlock := Lock(path)
	if d := time.Since(start); d < lockWait || d > lockWait+time.Second {
		t.Errorf("waited %s, want about %s", d, lockWait)
	}
	unlock()
	// Not the holder's lock: still there.
	if _, err := os.Stat(path + ".lock"); err != nil {
		t.Errorf("the other holder's lock was removed: %v", err)
	}
	held()
	if _, err := os.Stat(path + ".lock"); !os.IsNotExist(err) {
		t.Errorf("lock left behind: %v", err)
	}
}

// A lock left behind by a process that died is taken over.
func TestLockTakesOverAStaleLock(t *testing.T) {
	shortTimings(t)
	path := filepath.Join(t.TempDir(), "names.json")
	if err := os.WriteFile(path+".lock", []byte("123 dead\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * lockStale)
	if err := os.Chtimes(path+".lock", old, old); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	unlock := Lock(path)
	if d := time.Since(start); d >= lockWait {
		t.Errorf("waited %s for a stale lock", d)
	}
	raw, _ := os.ReadFile(path + ".lock")
	if strings.Contains(string(raw), "dead") {
		t.Error("the stale lock is still there")
	}
	unlock()
	if entries, _ := os.ReadDir(filepath.Dir(path)); len(entries) != 0 {
		t.Errorf("files left behind: %v", entries)
	}
}

// A directory that cannot be written is no reason to wait.
func TestLockInAnUnwritableDirectory(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("a file, not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	Lock(filepath.Join(blocker, "cache", "names.json"))()
	if d := time.Since(start); d > lockWait/2 {
		t.Errorf("waited %s", d)
	}
}
