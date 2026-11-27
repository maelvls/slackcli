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

// A holder frozen for longer than lockStale has its lock taken over. If
// the takeover, and a new lock, came between its unlock seeing its own
// token and removing the file, it would remove that new lock, which is
// not its own, and a third process could take the lock while the new
// holder still writes: the unlock and the takeover exclude each other.
func TestUnlockDoesNotRemoveALockTakenOverMeanwhile(t *testing.T) {
	shortTimings(t)
	path := filepath.Join(t.TempDir(), "names.json")
	lock := path + ".lock"
	unlock := Lock(path)
	old := time.Now().Add(-2 * lockStale)
	if err := os.Chtimes(lock, old, old); err != nil { // the holder froze
		t.Fatal(err)
	}
	const newToken = "456 new\n"
	tookOver := false
	releaseHook = func() {
		// Another invocation, waiting for the lock, finds it stale.
		if removeStaleLock(lock) {
			tookOver = true
			if err := createLock(lock, newToken); err != nil {
				t.Errorf("taking the lock after the takeover: %v", err)
			}
		}
	}
	t.Cleanup(func() { releaseHook = nil })
	unlock()
	raw, err := os.ReadFile(lock)
	switch {
	case tookOver && err != nil:
		t.Errorf("the unlock removed the lock that had taken over its own: %v", err)
	case tookOver && string(raw) != newToken:
		t.Errorf("lock = %q, want the new holder's", raw)
	case !tookOver && err == nil:
		t.Errorf("the unlock left its own lock behind: %q", raw)
	}
}

// An unlock that cannot have the guard does not hang: it leaves its lock
// to go stale rather than remove it unguarded.
func TestUnlockWithoutTheGuard(t *testing.T) {
	shortTimings(t)
	path := filepath.Join(t.TempDir(), "names.json")
	unlock := Lock(path)
	if err := os.WriteFile(path+".lock.takeover", nil, 0o600); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	unlock()
	if d := time.Since(start); d < lockWait || d > lockWait+time.Second {
		t.Errorf("waited %s, want about %s", d, lockWait)
	}
	if _, err := os.Stat(path + ".lock"); err != nil {
		t.Errorf("the lock was removed without the guard: %v", err)
	}
}

// A stale guard that cannot be removed (here a directory that is not
// empty) does not make the unlock spin: it waits at most lockWait, and
// leaves its lock to go stale.
func TestUnlockWithAStaleGuardThatCannotBeRemoved(t *testing.T) {
	shortTimings(t)
	path := filepath.Join(t.TempDir(), "names.json")
	unlock := Lock(path)
	guard := path + ".lock.takeover"
	if err := os.MkdirAll(filepath.Join(guard, "not-empty"), 0o700); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * lockStale)
	if err := os.Chtimes(guard, old, old); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		unlock()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(lockWait + time.Second):
		t.Fatal("the unlock did not return: it spins on the stale guard")
	}
	if _, err := os.Stat(path + ".lock"); err != nil {
		t.Errorf("the lock was removed without the guard: %v", err)
	}
}

// The same guard, or a stale lock that cannot be removed, does not make
// Lock spin either: it gives up after lockWait.
func TestLockWithAStaleLockThatCannotBeRemoved(t *testing.T) {
	shortTimings(t)
	path := filepath.Join(t.TempDir(), "names.json")
	old := time.Now().Add(-2 * lockStale)
	for _, dir := range []string{path + ".lock", path + ".lock.takeover"} {
		if err := os.MkdirAll(filepath.Join(dir, "not-empty"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(dir, old, old); err != nil {
			t.Fatal(err)
		}
	}
	done := make(chan struct{})
	go func() {
		Lock(path)()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(lockWait + time.Second):
		t.Fatal("Lock did not return: it spins on the stale lock")
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

// Readers do not take the lock. On Windows a file that is open cannot be
// replaced (access denied, sharing violation) until it is closed: the
// write must wait a little for the reader rather than be dropped.
func TestWriteWhileTheFileIsOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "names.json")
	Write(path, []byte("old\n"))
	reader, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	closed := make(chan struct{})
	time.AfterFunc(100*time.Millisecond, func() {
		reader.Close()
		close(closed)
	})
	Write(path, []byte("new\n"))
	<-closed
	if data, _ := os.ReadFile(path); string(data) != "new\n" {
		t.Errorf("file = %q, want the new content", data)
	}
	if entries, _ := os.ReadDir(filepath.Dir(path)); len(entries) != 1 {
		t.Errorf("files left behind: %v", entries)
	}
}
