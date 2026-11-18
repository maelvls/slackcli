// Package testenv isolates tests from the developer's machine. It is only
// imported by tests.
package testenv

import (
	"bytes"
	"encoding/json"
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

// CompactJSON returns an indented JSON document the way a command prints
// it when its output is not a terminal: on one line, followed by a
// newline. Tests write the output they expect indented, for the reader.
func CompactJSON(t testing.TB, indented string) string {
	t.Helper()
	var out bytes.Buffer
	if err := json.Compact(&out, []byte(indented)); err != nil {
		t.Fatalf("the expected output is not valid JSON: %v\n%s", err, indented)
	}
	return out.String() + "\n"
}
