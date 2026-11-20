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
	// Hint, when set, is what Error says after the code in place of the
	// general explanation of [explain]: what is known about this very
	// failure (see WithFallback).
	Hint string
}

// withHint returns err with hint as its explanation when err is an
// *APIError and there is a hint, and err itself otherwise. The error is
// copied: the same code, method and response, another explanation.
func withHint(err error, hint string) error {
	var ae *APIError
	if hint == "" || !errors.As(err, &ae) {
		return err
	}
	hinted := *ae
	hinted.Hint = hint
	return &hinted
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
	if e.Hint != "" {
		b.WriteString(": " + e.Hint)
	} else if hint := explain(e.Code); hint != "" {
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
	case "team_is_restricted":
		return fmt.Sprintf("this workspace belongs to an Enterprise Grid organisation, and Slack refuses this method to the session of a workspace. The session of the organisation usually works: run the command again with --workspace <org>.enterprise.slack.com (`%[1]s auth whoami` lists the configured workspaces; `%[1]s auth import-desktop` imports the organisation along with its workspaces)", appinfo.Name)
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

// IsRestricted reports whether err is one of Slack's "*_is_restricted"
// refusals (team_is_restricted, enterprise_is_restricted …): on Enterprise
// Grid, some methods refuse the session of a workspace.
func IsRestricted(err error) bool {
	return strings.HasSuffix(ErrorCode(err), "_is_restricted")
}

// TimeoutError is returned when Slack did not answer a call in time. A
// request that may have changed something says so: the caller must not
// blindly retry it.
type TimeoutError struct {
	Method  string
	Timeout time.Duration
}

func (e *TimeoutError) Error() string {
	note := ""
	if !isReadOnlyMethod(e.Method) {
		note = "; the request may still have been processed, so check before retrying"
	}
	return fmt.Sprintf("Slack API call %s timed out after %s%s. Set %s to adjust.",
		e.Method, e.Timeout, note, appinfo.EnvName("API_TIMEOUT_MS"))
}

// NetworkError is returned when a call did not reach Slack, or its answer
// did not come back: no connection, a reset, a DNS failure.
type NetworkError struct {
	Method string
	Err    error
}

func (e *NetworkError) Error() string {
	return fmt.Sprintf("calling Slack API %s: %v", e.Method, e.Err)
}
func (e *NetworkError) Unwrap() error { return e.Err }

// HTTPStatusError is returned when Slack answered a call with an HTTP
// error status and no error code of its own: a 5xx of its front ends,
// most of the time.
type HTTPStatusError struct {
	Method string
	Status int
}

func (e *HTTPStatusError) Error() string {
	return fmt.Sprintf("Slack HTTP %d calling %s", e.Status, e.Method)
}

// transientCodes are the error codes Slack answers when the failure is its
// own and momentary.
var transientCodes = []string{"ratelimited", "internal_error", "fatal_error", "service_unavailable", "request_timeout"}

// IsTransient reports whether err says nothing about what was asked: Slack
// did not answer (TimeoutError, NetworkError), is rate limiting the call
// (RateLimitError, "ratelimited"), or failed on its side (HTTP 5xx,
// internal_error …). The same call may well succeed a moment later, so
// such an error must not be taken for "not found" or "not allowed".
func IsTransient(err error) bool {
	var (
		timeout *TimeoutError
		network *NetworkError
		limited *RateLimitError
		status  *HTTPStatusError
	)
	switch {
	case err == nil:
		return false
	case errors.As(err, &timeout), errors.As(err, &network), errors.As(err, &limited):
		return true
	case errors.As(err, &status):
		return status.Status >= 500
	}
	return IsErrorCode(err, transientCodes...)
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
