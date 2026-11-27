//go:build unix

package audit

import (
	"errors"
	"os"
	"syscall"
	"time"
)

// lockWait is how long a lock on the log is waited for before the line
// is dropped: a slackcli process stopped (^Z, a sleeping laptop) while it
// holds one must not hang every other command. A variable so that tests
// can shorten it.
var lockWait = 200 * time.Millisecond

// errLocked is returned when the log stayed locked for lockWait.
var errLocked = errors.New("the log is locked by another process")

// flock takes the lock how (LOCK_SH or LOCK_EX) on f, waiting at most
// lockWait for it.
func flock(f *os.File, how int) error {
	deadline := time.Now().Add(lockWait)
	for {
		err := syscall.Flock(int(f.Fd()), how|syscall.LOCK_NB)
		switch {
		case err == nil:
			return nil
		case errors.Is(err, syscall.EINTR):
			continue
		case !errors.Is(err, syscall.EWOULDBLOCK):
			return err
		case time.Now().After(deadline):
			return errLocked
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// rotate moves the full log at path, which f is open on, to <path>.1, and
// closes f. An exclusive lock on the file keeps two processes that find
// it full at the same time from both renaming: the second one sees that
// the path names another file by then, and leaves it.
func rotate(f *os.File, info os.FileInfo, path string) error {
	defer f.Close()
	if err := flock(f, syscall.LOCK_EX); err != nil {
		return err
	}
	defer unlock(f)
	return renameIfSame(info, path)
}

// lockShared takes a shared lock on f, which rotate's exclusive lock waits
// for: no file is renamed while a line is being written to it. Closing f
// releases it.
func lockShared(f *os.File) error { return flock(f, syscall.LOCK_SH) }

func unlock(f *os.File) { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }
