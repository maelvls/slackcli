//go:build unix

package audit

import (
	"os"
	"syscall"
)

// rotate moves the full log at path, which f is open on, to <path>.1, and
// closes f. An exclusive lock on the file keeps two processes that find
// it full at the same time from both renaming: the second one sees that
// the path names another file by then, and leaves it.
func rotate(f *os.File, info os.FileInfo, path string) error {
	defer f.Close()
	fd := int(f.Fd())
	if err := syscall.Flock(fd, syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(fd, syscall.LOCK_UN)
	return renameIfSame(info, path)
}

// lockShared takes a shared lock on f, which rotate's exclusive lock waits
// for: no file is renamed while a line is being written to it. Closing f
// releases it.
func lockShared(f *os.File) error { return syscall.Flock(int(f.Fd()), syscall.LOCK_SH) }

func unlock(f *os.File) { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }
