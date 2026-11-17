package slackapi

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/maelvls/slackcli/internal/appinfo"
)

func getenv(k string) string { return os.Getenv(k) }

// APIError is a Slack Web API response with ok=false.
type APIError struct {
	// Method is the API method that failed, e.g. "conversations.history".
	Method string
	// Code is Slack's error code, e.g. "channel_not_found".
	Code string
	// Response is the full decoded response body.
	Response map[string]any
}

func newAPIError(method string, resp map[string]any) *APIError {
	code, _ := resp["error"].(string)
	return &APIError{Method: method, Code: code, Response: resp}
}

// Error is "Slack API error calling <method>: <code>", followed by the
// scopes involved when Slack names them, by the messages of
// response_metadata, and, for the codes of [explain], by what to do about
// it. The explanation is part of the message of the error itself, so that
// it appears exactly once however the error is wrapped: code that wraps an
// *APIError adds what it knows (the channel, the user), not a second
// explanation of the code.
func (e *APIError) Error() string {
	if e.Code == "" {
		return "Slack API error calling " + e.Method
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Slack API error calling %s: %s", e.Method, e.Code)
	// missing_scope & co. come with the scopes involved; they are what the
	// user needs to fix the problem.
	if needed, _ := e.Response["needed"].(string); needed != "" {
		fmt.Fprintf(&b, " (needed: %s", needed)
		if provided, _ := e.Response["provided"].(string); provided != "" {
			fmt.Fprintf(&b, "; provided: %s", provided)
		}
		b.WriteString(")")
	}
	if md, _ := e.Response["response_metadata"].(map[string]any); md != nil {
		if msgs, _ := md["messages"].([]any); len(msgs) > 0 {
			parts := make([]string, 0, len(msgs))
			for _, m := range msgs {
				if s, _ := m.(string); s != "" {
					parts = append(parts, s)
				}
			}
			if len(parts) > 0 {
				fmt.Fprintf(&b, " [%s]", strings.Join(parts, "; "))
			}
		}
	}
	if hint := explain(e.Code); hint != "" {
		b.WriteString(": " + hint)
	}
	return b.String()
}

// explain says, for the Slack error codes that any command can run into,
// what the code means and what to do next ("" for the others). The
// commands' users are mostly agents, which read the message verbatim and
// have nothing else to go by.
func explain(code string) string {
	reimport := fmt.Sprintf("if SLACK_TOKEN is set, replace it; otherwise import the credentials again with `%[1]s auth import-desktop` (or import-chrome, import-brave, import-firefox), then check them with `%[1]s auth test`", appinfo.Name)
	switch code {
	case "invalid_auth":
		return "Slack rejected the credentials (expired, signed out, or for another workspace): " + reimport
	case "not_authed":
		return "Slack received no credentials: " + reimport
	case "token_revoked":
		return "the token was revoked (signing out of Slack revokes a browser token): " + reimport
	case "token_expired":
		return "the token has expired: " + reimport
	case "account_inactive":
		return "the account behind these credentials was deactivated, or the app was uninstalled: credentials of an active account are needed; " + reimport
	case "missing_scope":
		return fmt.Sprintf("the token lacks the OAuth scope this call needs: add the scope to the Slack app and reinstall it, or use the credentials of a user instead (`%s auth import-desktop`)", appinfo.Name)
	case "not_in_channel":
		return "these credentials are not a member of that conversation: join it in Slack (a bot must be invited with /invite), or use credentials that are a member"
	case "channel_not_found":
		return fmt.Sprintf("no conversation has that ID in this workspace, or it is a private one these credentials are not a member of: check the ID (`%s channel list`), and the workspace (--workspace, or pass a Slack URL, which names its own)", appinfo.Name)
	case "ratelimited":
		return fmt.Sprintf("Slack is rate limiting this method: run the command again in a minute, or set %s to wait and retry automatically", appinfo.EnvName("RATE_LIMIT_MAX_WAIT_MS"))
	}
	return ""
}

// ErrorCode returns the Slack error code carried by err ("" if err is not,
// and does not wrap, an *APIError).
func ErrorCode(err error) string {
	var ae *APIError
	if errors.As(err, &ae) {
		return ae.Code
	}
	return ""
}

// IsErrorCode reports whether err carries one of the given Slack error
// codes.
func IsErrorCode(err error, codes ...string) bool {
	code := ErrorCode(err)
	if code == "" {
		return false
	}
	for _, c := range codes {
		if c == code {
			return true
		}
	}
	return false
}

// IsAuthError reports whether err means the credentials are no longer
// valid, i.e. re-importing them may help.
func IsAuthError(err error) bool {
	return IsErrorCode(err, "invalid_auth", "token_expired", "not_authed", "account_inactive", "token_revoked")
}

// RateLimitError is returned on HTTP 429 when the wait Slack asks for
// exceeds what the caller allows.
type RateLimitError struct {
	Method     string
	RetryAfter time.Duration
	MaxWait    time.Duration
}

// Error says what was limited and for how long (Summary), and how to have
// the client wait instead of failing.
func (e *RateLimitError) Error() string {
	return fmt.Sprintf("%s (set %s to wait and retry automatically; currently %dms).",
		e.Summary(), appinfo.EnvName("RATE_LIMIT_MAX_WAIT_MS"), e.MaxWait.Milliseconds())
}

// Summary is Error without the advice, for a message that gives its own
// (a warning about a partial result, a longer explanation): the advice
// must not be given twice.
func (e *RateLimitError) Summary() string {
	return fmt.Sprintf("Slack API call %s was rate limited; retry after %s", e.Method, e.RetryAfter)
}
