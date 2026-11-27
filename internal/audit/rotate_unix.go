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
