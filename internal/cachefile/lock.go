package cachefile

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"time"
)

// Timings of the lock of a cache file; variables so that tests can
// shorten them.
var (
	// lockWait is how long an invocation waits for another one to finish
	// updating the file, before it goes on without the lock.
	lockWait = 2 * time.Second
	// lockStale is the age after which a lock file is taken to have been
	// left behind by a process that died, and is removed. A lock is only
	// held for the read and the write of a small file.
	lockStale = 10 * time.Second
	lockPoll  = 5 * time.Millisecond
)

// Lock takes the advisory lock that makes the read, merge and write of a
// cache file one step across processes, so that two invocations adding
// different entries at the same time both keep theirs. The returned
// function releases it.
//
// It follows the lock of the credentials file (package auth): a
// "<path>.lock" file created with O_EXCL, which works the same on every OS
// and file system, Windows included. A cache must never make a command
// fail or hang, so this one is best effort: when the lock cannot be had
// within lockWait, or cannot be created at all (a directory that is not
// writable), Lock returns an unlock that does nothing, and the caller
// updates the file without it, as before there was a lock: the file is
// still replaced atomically, but an update made meanwhile by another
// invocation may be lost.
func Lock(path string) (unlock func()) {
	noop := func() {}
	lock := path + ".lock"
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return noop
	}
	token := newLockToken()
	deadline := time.Now().Add(lockWait)
	for {
		err := createLock(lock, token)
		if err == nil {
			return func() {
				// A holder that was frozen for longer than lockStale may
				// have lost its lock to another process: that lock is not
				// its to remove.
				if raw, err := os.ReadFile(lock); err == nil && string(raw) == token {
					_ = os.Remove(lock)
				}
			}
		}
		if !lockHeld(lock, err) && !transientRefusal(err) {
			return noop
		}
		if removeStaleLock(lock) {
			continue
		}
		if time.Now().After(deadline) {
			return noop
		}
		time.Sleep(lockPoll)
	}
}

// newLockToken returns what a holder writes in its lock file: its PID, and
// a nonce that tells its lock from any other one.
func newLockToken() string {
	var nonce [8]byte
	_, _ = rand.Read(nonce[:])
	return strconv.Itoa(os.Getpid()) + " " + hex.EncodeToString(nonce[:]) + "\n"
}

// createLock creates the lock file, which must not exist, holding token.
func createLock(lock, token string) error {
	f, err := os.OpenFile(lock, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, err = f.WriteString(token)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		// Without its token the lock could not be released.
		_ = os.Remove(lock)
	}
	return err
}

// lockHeld reports whether creating the lock file failed because there is
// one already, as opposed to because it cannot be created at all.
func lockHeld(lock string, createErr error) bool {
	if errors.Is(createErr, fs.ErrExist) {
		return true
	}
	// On Windows, creating a file that is being deleted fails with "access
	// denied" rather than "exists".
	if runtime.GOOS == "windows" && errors.Is(createErr, fs.ErrPermission) {
		_, err := os.Lstat(lock)
		return !errors.Is(err, fs.ErrNotExist)
	}
	return false
}

// transientRefusal reports whether a failed creation may be due to a lock
// file that was being deleted, which Windows reports as "access denied":
// it is retried until lockWait, like a held lock.
func transientRefusal(createErr error) bool {
	return runtime.GOOS == "windows" && errors.Is(createErr, fs.ErrPermission)
}

// isStale reports whether the file was last written longer than lockStale
// ago.
func isStale(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && time.Since(fi.ModTime()) > lockStale
}

// removeStaleLock removes a lock left behind by a process that died, and
// reports whether it did. As for the credentials lock, the age is checked
// and the lock removed while holding a second O_EXCL file, the guard, so
// that a waiter never removes the lock another waiter has just taken.
func removeStaleLock(lock string) bool {
	if !isStale(lock) {
		return false
	}
	guard := lock + ".takeover"
	f, err := os.OpenFile(guard, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		// Another waiter is taking the lock over. A guard is held for the
		// time of two system calls: an old one belongs to a process that
		// died right then.
		if isStale(guard) {
			_ = os.Remove(guard)
		}
		return false
	}
	_ = f.Close()
	defer os.Remove(guard)
	return isStale(lock) && os.Remove(lock) == nil
}
