// Package testenv isolates tests from the developer's machine. It is only
// imported by tests.
package testenv

import (
	"path/filepath"
	"testing"
)

// Isolate points every per-user directory at a fresh temporary directory
// for the duration of the test and returns it (the fake home directory).
//
// After it, appinfo.ConfigDir, appinfo.CacheDir and appinfo.DownloadsDir
// are below the returned directory on every OS, so that a test never reads
// or writes the real configuration, cache or downloads. It covers what
// os.UserHomeDir, os.UserCacheDir and os.UserConfigDir read: HOME and the
// XDG variables on Unix, USERPROFILE, LocalAppData and AppData on Windows
// (setting HOME alone leaves the real %LocalAppData% in use there).
//
// It uses t.Setenv, so the test must not be parallel.
func Isolate(t testing.TB) (home string) {
	t.Helper()
	home = t.TempDir()
	for name, dir := range map[string]string{
		"HOME":            home,
		"USERPROFILE":     home,
		"XDG_CONFIG_HOME": filepath.Join(home, ".config"),
		"XDG_CACHE_HOME":  filepath.Join(home, ".cache"),
		"LocalAppData":    filepath.Join(home, "AppData", "Local"),
		"AppData":         filepath.Join(home, "AppData", "Roaming"),
	} {
		t.Setenv(name, dir)
	}
	return home
}
