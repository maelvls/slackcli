package clictx

import (
	"context"
	"strings"
	"testing"

	"github.com/maelvls/slackcli/internal/slackapi"
)

func TestCheckToken(t *testing.T) {
	for token, want := range map[string]slackapi.AuthType{
		"xoxb-1-abc":      slackapi.AuthStandard,
		"xoxp-1-abc":      slackapi.AuthStandard,
		"xoxa-2-abc":      slackapi.AuthStandard,
		"xoxs-1-abc":      slackapi.AuthStandard,
		"xoxe.xoxp-1-abc": slackapi.AuthStandard,
		"xapp-1-abc":      slackapi.AuthStandard,
		"xoxc-1-abc":      slackapi.AuthBrowser,
	} {
		if got, err := CheckToken("SLACK_TOKEN", token); err != nil || got != want {
			t.Errorf("%s: got %q, %v; want %q", token, got, err, want)
		}
	}
	for _, token := range []string{"garbage-secret", "xoxd-secret", "XOXB-secret", "Bearer xoxb-secret", "xox-secret"} {
		_, err := CheckToken("--token", token)
		if err == nil || !strings.HasPrefix(err.Error(), "--token ") || strings.Contains(err.Error(), "secret") {
			t.Errorf("%s: err = %v, want an error naming --token and not the value", token, err)
		}
	}
}

func TestCheckCookieD(t *testing.T) {
	for _, cookie := range []string{"xoxd-a+b", "d=xoxd-a+b", " xoxd-a%2Bb ", "d=xoxd-a%2Bb"} {
		if got, err := CheckCookieD("SLACK_COOKIE_D", cookie); err != nil || got != "xoxd-a+b" {
			t.Errorf("%q: got %q, %v", cookie, got, err)
		}
	}
	for _, cookie := range []string{"", "notxoxd-secret", "xoxc-secret"} {
		_, err := CheckCookieD("SLACK_COOKIE_D", cookie)
		if err == nil || !strings.Contains(err.Error(), "SLACK_COOKIE_D must be") || strings.Contains(err.Error(), "secret") {
			t.Errorf("%q: err = %v", cookie, err)
		}
	}
}

// What SLACK_TOKEN and SLACK_COOKIE_D hold is checked before any client is
// made, so that nothing that is not a Slack token is ever sent to Slack.
func TestEnvAuthValidatesTokenShapes(t *testing.T) {
	c := newContext(t)
	if _, ok, err := EnvAuth(); ok || err != nil {
		t.Fatalf("nothing set: ok = %v, err = %v", ok, err)
	}
	for _, tc := range []struct{ token, cookie, want string }{
		{"garbage-secret", "", "SLACK_TOKEN does not look like a Slack token"},
		{"xoxd-secret", "", `SLACK_TOKEN holds a "d" cookie`},
		{"xoxc-secret", "", "SLACK_COOKIE_D is not set"},
		{"xoxc-secret", "notxoxd-secret", "SLACK_COOKIE_D must be the value of the browser's"},
	} {
		t.Setenv("SLACK_TOKEN", tc.token)
		t.Setenv("SLACK_COOKIE_D", tc.cookie)
		_, ok, err := EnvAuth()
		if !ok || err == nil || !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), "secret") {
			t.Errorf("SLACK_TOKEN=%s SLACK_COOKIE_D=%s: ok = %v, err = %v", tc.token, tc.cookie, ok, err)
		}
		if _, clientErr := c.Client(context.Background(), "https://acme.slack.com"); clientErr == nil || clientErr.Error() != err.Error() {
			t.Errorf("SLACK_TOKEN=%s: Client err = %v, want %v", tc.token, clientErr, err)
		}
	}

	t.Setenv("SLACK_TOKEN", " xoxc-1 ")
	t.Setenv("SLACK_COOKIE_D", "d=xoxd-a%2Bb")
	want := slackapi.Auth{Type: slackapi.AuthBrowser, XoxcToken: "xoxc-1", XoxdCookie: "xoxd-a+b"}
	if a, ok, err := EnvAuth(); !ok || err != nil || a != want {
		t.Errorf("browser: got %+v, %v, %v", a, ok, err)
	}
	t.Setenv("SLACK_TOKEN", "xoxb-1")
	if a, ok, err := EnvAuth(); !ok || err != nil || a != (slackapi.Auth{Type: slackapi.AuthStandard, Token: "xoxb-1"}) {
		t.Errorf("standard: got %+v, %v, %v", a, ok, err)
	}
}

// The refusal names what turned safe mode on, the legacy variable included.
func TestCheckSafeModeNamesItsSource(t *testing.T) {
	for _, tc := range []struct {
		flag            bool
		current, legacy string
		want            string
	}{
		{true, "", "", "(turned on by --safe-mode)"},
		{false, "1", "", "(turned on by SLACKCLI_SAFE_MODE)"},
		{false, "", "1", "(turned on by AGENT_SLACK_SAFE_MODE)"},
		{false, "false", "enabled", "(turned on by AGENT_SLACK_SAFE_MODE)"},
		{true, "1", "1", "(turned on by --safe-mode and SLACKCLI_SAFE_MODE and AGENT_SLACK_SAFE_MODE)"},
	} {
		c := newContext(t)
		c.SafeModeFlag = tc.flag
		t.Setenv("SLACKCLI_SAFE_MODE", tc.current)
		t.Setenv("AGENT_SLACK_SAFE_MODE", tc.legacy)
		err := c.CheckSafeMode("message delete")
		if err == nil || !strings.Contains(err.Error(), "safe mode is active "+tc.want) {
			t.Errorf("%+v: err = %v", tc, err)
		}
	}
	if got := newContext(t).SafeModeSource(); got != "" {
		t.Errorf("safe mode off: SafeModeSource() = %q", got)
	}
}

func TestChangesSlack(t *testing.T) {
	for command, want := range map[string]bool{
		"message send":             true,
		"message compose":          true,
		"message edit":             true,
		"message scheduled cancel": true,
		"message react add":        true,
		"message draft create":     true,
		"later remind":             true,
		"channel mark":             true,
		"user dm-open":             true,
		"workflow run":             true,
		"message get":              false,
		"message list":             false,
		"message scheduled list":   false,
		"message":                  false,
		"channel list":             false,
		"search all":               false,
		"auth test":                false,
		"latergator":               false,
	} {
		if got := ChangesSlack(command); got != want {
			t.Errorf("ChangesSlack(%q) = %v, want %v", command, got, want)
		}
	}
	for _, command := range SafeModeBlocked() {
		if !ChangesSlack(command) {
			t.Errorf("%q is blocked by safe mode but ChangesSlack says it does not write", command)
		}
	}
}
