package appinfo

import (
	"slices"
	"strings"
	"testing"
	"time"
)

func TestSafeModeEnvFailsClosed(t *testing.T) {
	cases := []struct {
		current, legacy string
		want            bool
	}{
		{"", "", false},
		{"0", "", false},
		{"false", "", false},
		{" FALSE ", "", false},
		{"no", "off", false},
		{"Off", "No", false},
		{"1", "", true},
		{"true", "", true},
		// Values that are not a recognised "on" must not disable safe mode.
		{"enabled", "", true},
		{"y", "", true},
		{"t", "", true},
		{"2", "", true},
		{"always", "", true},
		// Either name turns it on; the other cannot turn it off.
		{"false", "1", true},
		{"0", "enabled", true},
		{"", "1", true},
		{"1", "false", true},
	}
	for _, c := range cases {
		t.Setenv(EnvPrefix+"SAFE_MODE", c.current)
		t.Setenv(LegacyEnvPrefix+"SAFE_MODE", c.legacy)
		// The names are those of the variables that turn it on.
		var wantNames []string
		if !isOffValue(c.current) {
			wantNames = append(wantNames, "SLACKCLI_SAFE_MODE")
		}
		if !isOffValue(c.legacy) {
			wantNames = append(wantNames, "AGENT_SLACK_SAFE_MODE")
		}
		if got := SafeModeEnvNames(); !slices.Equal(got, wantNames) || (len(got) > 0) != c.want {
			t.Errorf("%sSAFE_MODE=%q %sSAFE_MODE=%q: SafeModeEnvNames() = %q, want %q",
				EnvPrefix, c.current, LegacyEnvPrefix, c.legacy, got, wantNames)
		}
		if got := SafeModeEnv(); got != c.want {
			t.Errorf("%sSAFE_MODE=%q %sSAFE_MODE=%q: SafeModeEnv() = %v, want %v",
				EnvPrefix, c.current, LegacyEnvPrefix, c.legacy, got, c.want)
		}
	}
}

func TestResolveVersion(t *testing.T) {
	cases := []struct {
		ldflags, module string
		want            string
	}{
		{"v1.2.3", "(devel)", "v1.2.3"},
		{"v1.2.3", "v9.9.9", "v1.2.3"},
		// No ldflags: the version the Go toolchain recorded, if any.
		{"dev", "v1.2.3", "v1.2.3"},
		{"dev", "(devel)", "dev"},
		{"dev", "", "dev"},
	}
	for _, c := range cases {
		if got := resolveVersion(c.ldflags, c.module); got != c.want {
			t.Errorf("resolveVersion(%q, %q) = %q, want %q", c.ldflags, c.module, got, c.want)
		}
	}
}

func TestBareVersion(t *testing.T) {
	old := Version
	t.Cleanup(func() { Version = old })

	Version = "v1.2.3"
	if got := BareVersion(); got != "1.2.3" {
		t.Errorf("BareVersion() = %q", got)
	}
	if got := UserAgent(); got != Name+"/1.2.3" {
		t.Errorf("UserAgent() = %q", got)
	}
}

// An invalid duration falls back to the default, and CheckEnvDurationMS
// says which variable is wrong and what is accepted.
func TestEnvDurationMS(t *testing.T) {
	const def = 7 * time.Second
	for _, c := range []struct {
		value     string
		allowZero bool
		want      time.Duration
		valid     bool
	}{
		{"", false, def, true},
		{"1500", false, 1500 * time.Millisecond, true},
		{" 250 ", false, 250 * time.Millisecond, true},
		{"0.5", true, 0, true},
		{"0", true, 0, true},
		{"0", false, def, false},
		{"0.5", false, def, false},
		{"-1", false, def, false},
		{"-1", true, def, false},
		{"abc", false, def, false},
		{"abc", true, def, false},
		{"30s", false, def, false},
		{"NaN", false, def, false},
		{"Inf", false, def, false},
		{"1e300", false, def, false},
	} {
		t.Setenv(EnvPrefix+"API_TIMEOUT_MS", c.value)
		t.Setenv(LegacyEnvPrefix+"API_TIMEOUT_MS", "")
		if got := EnvDurationMS("API_TIMEOUT_MS", def, c.allowZero); got != c.want {
			t.Errorf("%q (allowZero=%v): EnvDurationMS = %s, want %s", c.value, c.allowZero, got, c.want)
		}
		err := CheckEnvDurationMS("API_TIMEOUT_MS", c.allowZero)
		if (err == nil) != c.valid {
			t.Errorf("%q (allowZero=%v): CheckEnvDurationMS = %v, want valid = %v", c.value, c.allowZero, err, c.valid)
		}
		if err != nil && (!strings.Contains(err.Error(), `SLACKCLI_API_TIMEOUT_MS="`+c.value+`"`) || !strings.Contains(err.Error(), "milliseconds")) {
			t.Errorf("%q: the error does not name the variable and what is accepted: %v", c.value, err)
		}
	}

	// The legacy name is read, and named, when the current one is unset.
	t.Setenv(EnvPrefix+"API_TIMEOUT_MS", "")
	t.Setenv(LegacyEnvPrefix+"API_TIMEOUT_MS", "soon")
	if err := CheckEnvDurationMS("API_TIMEOUT_MS", false); err == nil || !strings.Contains(err.Error(), `AGENT_SLACK_API_TIMEOUT_MS="soon"`) {
		t.Errorf("legacy name: err = %v", err)
	}
}
