package testenv

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/maelvls/slackcli/internal/appinfo"
)

// Every directory the tool derives from the environment must end up below
// the isolated home, whatever the OS (os.UserCacheDir reads %LocalAppData%
// on Windows, $HOME on macOS, $XDG_CACHE_HOME on other Unixes).
func TestIsolate(t *testing.T) {
	home := Isolate(t)
	if info, err := os.Stat(home); err != nil || !info.IsDir() {
		t.Fatalf("home %s is not a directory: %v", home, err)
	}
	userHome, err := os.UserHomeDir()
	if err != nil || userHome != home {
		t.Errorf("os.UserHomeDir() = %q, %v; want %q", userHome, err, home)
	}
	userCache, err := os.UserCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	userConfig, err := os.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	prefix := home + string(filepath.Separator)
	for name, dir := range map[string]string{
		"os.UserCacheDir":         userCache,
		"os.UserConfigDir":        userConfig,
		"appinfo.ConfigDir":       appinfo.ConfigDir(),
		"appinfo.LegacyConfigDir": appinfo.LegacyConfigDir(),
		"appinfo.CacheDir":        appinfo.CacheDir(),
		"appinfo.DownloadsDir":    appinfo.DownloadsDir(),
	} {
		if !strings.HasPrefix(dir, prefix) {
			t.Errorf("%s = %q, not below the isolated home %q", name, dir, home)
		}
	}
}

func TestIsolateGivesEachTestItsOwnHome(t *testing.T) {
	var homes []string
	for range 2 {
		t.Run("", func(t *testing.T) { homes = append(homes, Isolate(t)) })
	}
	if homes[0] == homes[1] {
		t.Errorf("two tests share the home %q", homes[0])
	}
}
