package slackapi

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/maelvls/slackcli/internal/appinfo"
)

// The well-known codes say what to do next, with commands that exist.
func TestAPIErrorExplainsWellKnownCodes(t *testing.T) {
	importCmd := "`" + appinfo.Name + " auth import-desktop`"
	for code, wants := range map[string][]string{
		"invalid_auth":      {"rejected the credentials", "SLACK_TOKEN", importCmd, "`" + appinfo.Name + " auth test`"},
		"not_authed":        {"no credentials", "SLACK_TOKEN", importCmd},
		"token_revoked":     {"was revoked", importCmd},
		"token_expired":     {"has expired", importCmd},
		"account_inactive":  {"deactivated", importCmd},
		"missing_scope":     {"OAuth scope", importCmd},
		"not_in_channel":    {"not a member", "join"},
		"channel_not_found": {"private", "`" + appinfo.Name + " channel list`", "--workspace"},
		"ratelimited":       {"again in a minute", appinfo.EnvName("RATE_LIMIT_MAX_WAIT_MS")},
		// The workaround an agent does not find on its own.
		"team_is_restricted": {"Enterprise Grid", "--workspace <org>.enterprise.slack.com", "`" + appinfo.Name + " auth whoami`", importCmd},
	} {
		err := error(&APIError{Method: "conversations.history", Code: code, Response: map[string]any{"ok": false, "error": code}})
		msg := err.Error()
		prefix := "Slack API error calling conversations.history: " + code + ": "
		if !strings.HasPrefix(msg, prefix) {
			t.Errorf("%s: message %q does not start with %q", code, msg, prefix)
		}
		for _, want := range wants {
			if !strings.Contains(msg, want) {
				t.Errorf("%s: message %q does not say %q", code, msg, want)
			}
		}

		// Wrapping neither hides the code nor repeats the explanation.
		wrapped := fmt.Errorf("fetching the history of C1: %w", fmt.Errorf("retrying: %w", err))
		var ae *APIError
		if !IsErrorCode(wrapped, code) || !errors.As(wrapped, &ae) || ae.Code != code || ErrorCode(wrapped) != code {
			t.Errorf("%s: the code is lost by wrapping: %v", code, wrapped)
		}
		if n := strings.Count(wrapped.Error(), wants[0]); n != 1 {
			t.Errorf("%s: explanation given %d times in %q", code, n, wrapped.Error())
		}
	}
}

func TestAPIErrorMessage(t *testing.T) {
	for _, tt := range []struct {
		name string
		resp map[string]any
		want string
	}{
		{"unknown code stays bare", map[string]any{"error": "thread_not_found"}, "Slack API error calling m: thread_not_found"},
		{"no code", map[string]any{}, "Slack API error calling m"},
		{
			"missing_scope names the scopes before the advice",
			map[string]any{"error": "missing_scope", "needed": "channels:read", "provided": "chat:write"},
			"Slack API error calling m: missing_scope (needed: channels:read; provided: chat:write): the token lacks the OAuth scope this call needs: " +
				"add the scope to the Slack app and reinstall it, or use the credentials of a user instead (`" + appinfo.Name + " auth import-desktop`)",
		},
		{
			"response metadata",
			map[string]any{"error": "invalid_arguments", "response_metadata": map[string]any{"messages": []any{"[ERROR] missing channel", 3, "second"}}},
			"Slack API error calling m: invalid_arguments [[ERROR] missing channel; second]",
		},
	} {
		if got := newAPIError("m", tt.resp).Error(); got != tt.want {
			t.Errorf("%s:\n got %q\nwant %q", tt.name, got, tt.want)
		}
	}
}

func TestRateLimitErrorSummary(t *testing.T) {
	err := &RateLimitError{Method: "users.info", RetryAfter: 30 * time.Second}
	env := appinfo.EnvName("RATE_LIMIT_MAX_WAIT_MS")
	if got := err.Summary(); got != "Slack API call users.info was rate limited; retry after 30s" {
		t.Errorf("Summary() = %q", got)
	}
	if msg := err.Error(); !strings.HasPrefix(msg, err.Summary()) || strings.Count(msg, env) != 1 {
		t.Errorf("Error() = %q, want the summary then the variable to set, once", msg)
	}
}
