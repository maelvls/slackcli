package auth

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/maelvls/slackcli/internal/appinfo"
)

// Timings of the advisory lock on the credentials file; variables so that
// tests can shorten them.
var (
	// lockWait is how long a writer waits for another one to finish.
	lockWait = 10 * time.Second
	// lockStale is the age after which a lock file is taken to have been
	// left behind by a process that died, and is removed. A holder renews
	// its lock every third of that for as long as it holds it (a save may
	// wait on the keychain for a long time), so the lock of a live process
	// never gets that old.
	lockStale = 30 * time.Second
	lockPoll  = 20 * time.Millisecond
)

// lockFile takes the advisory lock that serialises read-modify-write
// cycles on the credentials file across processes: a "<path>.lock" file
// created with O_EXCL, which works the same on every OS and file system.
// The returned function releases it.
//
// It waits (up to lockWait) only while another process holds the lock; any
// other failure, such as a directory that is not writable, is returned at
// once.
func lockFile(path string) (unlock func(), err error) {
	lock := path + ".lock"
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("creating the directory of the credentials file: %w", err)
	}
	token := newLockToken()
	deadline := time.Now().Add(lockWait)
	for {
		err := createLock(lock, token)
		if err == nil {
			return holdLock(lock, token), nil
		}
		if !lockHeld(lock, err) {
			// The lock may have gone between the refusal and the look at
			// it (see lockHeld): only a refusal that lasts is final.
			if transientRefusal(err) && time.Now().Before(deadline.Add(lockTransient-lockWait)) {
				time.Sleep(lockPoll)
				continue
			}
			return nil, fmt.Errorf("creating the lock file of the credentials file (%s; is its directory writable?): %w", lock, err)
		}
		if removeStaleLock(lock) {
			continue
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("the credentials file is locked by another %s process (%s%s, waited %s): retry, or delete that lock file if no other %s is running",
				appinfo.Name, lock, lockHolder(lock), lockWait, appinfo.Name)
		}
		time.Sleep(lockPoll)
	}
}

// newLockToken returns what a holder writes in its lock file: its PID, for
// whoever looks at a lock that stays, and a nonce that tells its lock from
// any other one, including a later lock of a process with the same PID.
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

// lockTransient is how long a refusal that may come from a lock file being
// deleted is retried before it is taken for a directory that cannot be
// written.
const lockTransient = time.Second

// transientRefusal reports whether a failed creation may be due to a lock
// file that was being deleted, which Windows reports as "access denied".
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
// reports whether it did.
//
// Checking the age of the lock and removing it are two steps: a waiter
// that removed the lock on the strength of a check made before another
// waiter took the lock over would remove that waiter's new lock. So both
// steps are done while holding a second O_EXCL file, the guard: whoever
// removes a lock has checked, since the previous removal, that it is stale.
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

// holdLock keeps the lock file fresh until the returned function, which
// releases the lock, is called.
func holdLock(lock, token string) (unlock func()) {
	owned := func() bool {
		raw, err := os.ReadFile(lock)
		return err == nil && string(raw) == token
	}
	renew := time.NewTicker(max(lockStale/3, time.Millisecond))
	stop, stopped := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(stopped)
		for {
			select {
			case <-stop:
				return
			case now := <-renew.C:
				if owned() {
					_ = os.Chtimes(lock, now, now)
				}
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			renew.Stop()
			close(stop)
			<-stopped
			// A holder that was frozen for longer than lockStale may have
			// lost its lock to another process: that lock is not its to
			// remove.
			if owned() {
				_ = os.Remove(lock)
			}
		})
	}
}

// lockHolder describes the process that wrote the lock file (", held by
// PID 123"), or is empty when the file does not say.
func lockHolder(lock string) string {
	raw, err := os.ReadFile(lock)
	if err != nil {
		return ""
	}
	pid, _, _ := strings.Cut(strings.TrimSpace(string(raw)), " ")
	if _, err := strconv.Atoi(pid); err != nil {
		return ""
	}
	return ", held by PID " + pid
}
