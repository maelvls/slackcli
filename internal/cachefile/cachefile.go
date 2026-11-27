// Package cachefile holds what the on-disk caches share (the users, the
// conversation names): one file per workspace in the cache directory,
// readable by its owner only, updated under a lock (see Lock) and replaced
// atomically, so that concurrent invocations can share it.
package cachefile

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
)

// Path returns the file of the cache called name for a workspace, in dir,
// or "" when the workspace is unknown (nothing is cached then). The file
// name holds a hash of the workspace host, not the host itself.
func Path(dir, name, workspaceURL string) string {
	key := workspaceKey(workspaceURL)
	if key == "" {
		return ""
	}
	return filepath.Join(dir, name+"-"+key+".json")
}

func workspaceKey(workspaceURL string) string {
	source := strings.ToLower(strings.TrimSpace(workspaceURL))
	if u, err := url.Parse(source); err == nil && u.Hostname() != "" {
		source = u.Hostname()
	}
	if source == "" || source == "unknown" {
		return ""
	}
	sum := sha256.Sum256([]byte(source))
	return hex.EncodeToString(sum[:])[:16]
}

// Write replaces the file at path with data, best effort: a cache that
// cannot be written is only a cache that does not save anything. The
// directory is created 0700 and the file 0600, as the caches hold names
// that are not public (private channels, people). The file is written
// next to path and renamed over it, so that a reader never sees a
// half-written file.
func Write(path string, data []byte) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	tmp, err := os.CreateTemp(dir, "."+strings.TrimSuffix(filepath.Base(path), ".json")+"-*.tmp") // created 0600
	if err != nil {
		return
	}
	defer os.Remove(tmp.Name()) // no-op once renamed
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return
	}
	if err := tmp.Close(); err != nil {
		return
	}
	// The readers of a cache do not take its lock, and on Windows a file
	// that is open cannot be replaced until it is closed: the rename is
	// tried again for a short while, as for the credentials file (package
	// auth), rather than the update dropped.
	const attempts, pause = 50, 10 * time.Millisecond
	err = os.Rename(tmp.Name(), path)
	for i := 1; err != nil && i < attempts && renameBlockedByReader(err); i++ {
		time.Sleep(pause)
		err = os.Rename(tmp.Name(), path)
	}
}

// renameBlockedByReader reports whether a rename failed because another
// process or goroutine has the destination open, which only Windows minds
// (ERROR_ACCESS_DENIED or ERROR_SHARING_VIOLATION).
func renameBlockedByReader(err error) bool {
	const accessDenied, sharingViolation = syscall.Errno(5), syscall.Errno(32)
	var errno syscall.Errno
	return runtime.GOOS == "windows" && errors.As(err, &errno) &&
		(errno == accessDenied || errno == sharingViolation)
}
