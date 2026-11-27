//go:build !unix

package audit

import "os"

// rotate moves the full log at path to <path>.1, after closing f (an open
// file cannot be renamed on Windows). Without a lock, two processes may
// rotate at the same time, and one rotated file be lost.
func rotate(f *os.File, info os.FileInfo, path string) error {
	f.Close()
	return renameIfSame(info, path)
}
