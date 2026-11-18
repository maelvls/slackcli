// Package appinfo holds the program's identity (name, version, env-var
// prefix, on-disk locations). Every other package derives user-visible names
// from here so that the tool can be renamed in one place.
package appinfo

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"time"
)

const (
	// Name is the binary name, config directory name and keychain service.
	Name = "slackcli"
	// EnvPrefix prefixes every tool-specific environment variable.
	EnvPrefix = "SLACKCLI_"
	// Description is the one-line summary shown in help output.
	Description = "Slack automation CLI for AI agents"

	// LegacyName and LegacyEnvPrefix identify the TypeScript predecessor
	// (agent-slack). They are only used to migrate credentials and to keep
	// honouring old environment variables.
	LegacyName      = "agent-slack"
	LegacyEnvPrefix = "AGENT_SLACK_"
)

// Version is set at build time with
// -ldflags "-X github.com/maelvls/slackcli/internal/appinfo.Version=v1.2.3".
//
// A build that does not set it (or sets it to "dev")
// takes the version from the module metadata instead, when the
// Go toolchain recorded one.
var Version = "dev"

// unstamped is the value of Version in a build without ldflags (kept as a
// literal above: the linker's -X only overrides plain string variables).
const unstamped = "dev"

func init() {
	module := ""
	if info, ok := debug.ReadBuildInfo(); ok {
		module = info.Main.Version
	}
	Version = resolveVersion(Version, module)
}

// resolveVersion picks the version to display from the ldflags value and
// the main module version.
func resolveVersion(ldflags, module string) string {
	if ldflags != unstamped {
		return ldflags
	}
	if module != "" && module != "(devel)" {
		return module
	}
	return unstamped
}

// BareVersion is Version without its "v" prefix ("1.2.3", or "dev"): the
// form printed by --version and sent in the User-Agent.
func BareVersion() string { return strings.TrimPrefix(Version, "v") }

// UserAgent is sent on every HTTP request.
func UserAgent() string { return Name + "/" + BareVersion() }

// EnvName returns the full name of a tool-specific environment variable,
// e.g. EnvName("SAFE_MODE") == "SLACKCLI_SAFE_MODE".
func EnvName(suffix string) string { return EnvPrefix + suffix }

// EnvSource reads a tool-specific environment variable: it returns the name
// and the (trimmed) value of the one that is set, the current name first,
// else the legacy AGENT_SLACK_ one. Both are "" when neither is set.
func EnvSource(suffix string) (name, value string) {
	for _, prefix := range []string{EnvPrefix, LegacyEnvPrefix} {
		if v := strings.TrimSpace(os.Getenv(prefix + suffix)); v != "" {
			return prefix + suffix, v
		}
	}
	return "", ""
}

// EnvDurationMS reads a millisecond duration from a tool-specific variable.
// It returns def when the variable is unset or invalid (see
// ParseDurationMS). An invalid value is not reported here, deep in the
// code that needs the duration: the root command warns about it once, with
// CheckEnvDurationMS.
func EnvDurationMS(suffix string, def time.Duration, allowZero bool) time.Duration {
	_, raw := EnvSource(suffix)
	if raw == "" {
		return def
	}
	d, err := ParseDurationMS(raw, allowZero)
	if err != nil {
		return def
	}
	return d
}

// CheckEnvDurationMS says what is wrong with the value EnvDurationMS reads
// for suffix, naming the variable; it returns nil when the variable is
// unset or valid.
func CheckEnvDurationMS(suffix string, allowZero bool) error {
	name, raw := EnvSource(suffix)
	if raw == "" {
		return nil
	}
	if _, err := ParseDurationMS(raw, allowZero); err != nil {
		return fmt.Errorf("%s=%q is not valid: %w", name, raw, err)
	}
	return nil
}

// ParseDurationMS parses a duration given as a number of milliseconds:
// positive, or zero when allowZero is true. The error says what is
// accepted.
func ParseDurationMS(raw string, allowZero bool) (time.Duration, error) {
	accepted := "expected a positive number of milliseconds, e.g. 30000"
	if allowZero {
		accepted = "expected a number of milliseconds, 0 or more, e.g. 30000"
	}
	n, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	// The upper bound also keeps NaN and infinities out.
	if err != nil || !(n >= 0 && n <= maxDurationMS) {
		return 0, errors.New(accepted)
	}
	// Fractions of a millisecond are dropped: 0.5 is zero.
	d := time.Duration(n) * time.Millisecond
	if d == 0 && !allowZero {
		return 0, errors.New(accepted)
	}
	return d, nil
}

// maxDurationMS is the longest duration ParseDurationMS accepts: ten years,
// far below what a time.Duration can hold.
const maxDurationMS = 10 * 365 * 24 * 60 * 60 * 1000

// EnvOnUnlessOff reports whether a safety switch is on: it is on when the
// variable, under the current OR the legacy prefix, is set to anything but
// an explicit off value ("", "0", "false", "no", "off"; case-insensitive).
//
// It fails closed: a value it does not recognise ("enabled", "y", "2"…)
// turns the switch on, and one name saying "false" cannot override the
// other saying "1".
func EnvOnUnlessOff(suffix string) bool {
	for _, prefix := range []string{EnvPrefix, LegacyEnvPrefix} {
		if !isOffValue(os.Getenv(prefix + suffix)) {
			return true
		}
	}
	return false
}

func isOffValue(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "0", "false", "no", "off":
		return true
	}
	return false
}

// SafeModeEnv reports whether the environment asks for safe mode
// (SLACKCLI_SAFE_MODE or the legacy AGENT_SLACK_SAFE_MODE; see
// EnvOnUnlessOff).
func SafeModeEnv() bool { return EnvOnUnlessOff("SAFE_MODE") }

// SafeModeEnvNames returns the names of the variables that turn safe mode
// on (see EnvOnUnlessOff), the current name first. It is empty exactly
// when SafeModeEnv is false.
func SafeModeEnvNames() []string {
	var names []string
	for _, prefix := range []string{EnvPrefix, LegacyEnvPrefix} {
		if !isOffValue(os.Getenv(prefix + "SAFE_MODE")) {
			names = append(names, prefix+"SAFE_MODE")
		}
	}
	return names
}

func home() string {
	h, err := os.UserHomeDir()
	if err != nil || h == "" {
		return os.TempDir()
	}
	return h
}

// ConfigDir is where credentials.json lives: $XDG_CONFIG_HOME/slackcli or
// ~/.config/slackcli (on every OS, like the predecessor).
func ConfigDir() string {
	if x := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME")); x != "" && filepath.IsAbs(x) {
		return filepath.Join(x, Name)
	}
	return filepath.Join(home(), ".config", Name)
}

// LegacyConfigDir is the predecessor's config directory.
func LegacyConfigDir() string { return filepath.Join(home(), ".config", LegacyName) }

// CacheDir holds regenerable state (user cache, downloads): the OS user
// cache directory + "/slackcli".
func CacheDir() string {
	if d, err := os.UserCacheDir(); err == nil && d != "" {
		return filepath.Join(d, Name)
	}
	return filepath.Join(home(), "."+Name)
}

// DownloadsDir is where message attachments and canvases are downloaded.
// Paths printed to the user are absolute.
func DownloadsDir() string { return filepath.Join(CacheDir(), "downloads") }

// EnsureDownloadsDir creates DownloadsDir (0700) and returns it.
func EnsureDownloadsDir() (string, error) {
	d := DownloadsDir()
	if err := os.MkdirAll(d, 0o700); err != nil {
		return "", err
	}
	return d, nil
}
