package clictx

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/maelvls/slackcli/internal/slackapi"
)

const (
	browserTokenPrefix = "xoxc-"
	cookieDPrefix      = "xoxd-"
)

// standardTokenPrefixes are those of the tokens sent as a Bearer token to
// slack.com: bot (xoxb-), user (xoxp-), legacy workspace-app (xoxa-) and
// session (xoxs-) tokens, rotating access tokens (xoxe.xoxp-…, xoxe.xoxb-…)
// and app-level tokens (xapp-).
var standardTokenPrefixes = []string{"xoxb-", "xoxp-", "xoxa-", "xoxs-", "xoxe.", "xapp-"}

// CheckToken says what kind of credentials a token is: a browser token
// (xoxc-), which goes with the "d" cookie, or a standard one (see
// standardTokenPrefixes). Anything else is an error: sent to Slack, it
// would only come back as a puzzling invalid_auth. source names where the
// token comes from ("SLACK_TOKEN", "--token"); the token itself is never
// part of the error.
func CheckToken(source, token string) (slackapi.AuthType, error) {
	switch {
	case strings.HasPrefix(token, browserTokenPrefix):
		return slackapi.AuthBrowser, nil
	case strings.HasPrefix(token, cookieDPrefix):
		return "", fmt.Errorf(`%s holds a "d" cookie (xoxd-), not a token: a token starts with xoxb-, xoxp- or, for a browser token, xoxc-`, source)
	}
	for _, prefix := range standardTokenPrefixes {
		if strings.HasPrefix(token, prefix) {
			return slackapi.AuthStandard, nil
		}
	}
	return "", fmt.Errorf("%s does not look like a Slack token: expected a bot or user token (xoxb-…, xoxp-…; also %s) or a browser token (xoxc-…)",
		source, strings.Join(standardTokenPrefixes[2:], ", "))
}

// CheckCookieD validates the browser's "d" cookie and returns it decoded.
// It accepts "d=xoxd-…" as copied from a Cookie header, percent-encoded or
// not. source names where the cookie comes from; the cookie itself is
// never part of the error.
func CheckCookieD(source, cookie string) (string, error) {
	cookie = slackapi.DecodeCookieD(strings.TrimPrefix(strings.TrimSpace(cookie), "d="))
	if !strings.HasPrefix(cookie, cookieDPrefix) {
		return "", fmt.Errorf(`%s must be the value of the browser's "d" cookie, which starts with xoxd-`, source)
	}
	return cookie, nil
}

// EnvAuthConfigured reports whether credentials come from SLACK_TOKEN.
func EnvAuthConfigured() bool { return strings.TrimSpace(os.Getenv("SLACK_TOKEN")) != "" }

// EnvAuth builds credentials from SLACK_TOKEN (and SLACK_COOKIE_D for an
// xoxc- token). ok is false when SLACK_TOKEN is unset. When it is set but
// unusable (not a Slack token, or a browser token without its cookie), err
// says why.
func EnvAuth() (a slackapi.Auth, ok bool, err error) {
	token := strings.TrimSpace(os.Getenv("SLACK_TOKEN"))
	if token == "" {
		return a, false, nil
	}
	kind, err := CheckToken("SLACK_TOKEN", token)
	if err != nil {
		return a, true, err
	}
	if kind == slackapi.AuthStandard {
		return slackapi.Auth{Type: kind, Token: token}, true, nil
	}
	cookie := strings.TrimSpace(os.Getenv("SLACK_COOKIE_D"))
	if cookie == "" {
		cookie = strings.TrimSpace(os.Getenv("SLACK_COOKIE"))
	}
	if cookie == "" {
		return a, true, errors.New("SLACK_TOKEN is a browser token (xoxc-) but SLACK_COOKIE_D is not set")
	}
	if cookie, err = CheckCookieD("SLACK_COOKIE_D", cookie); err != nil {
		return a, true, err
	}
	return slackapi.Auth{Type: kind, XoxcToken: token, XoxdCookie: cookie}, true, nil
}
