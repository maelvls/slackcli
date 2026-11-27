package slackapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/maelvls/slackcli/internal/audit"
	"github.com/maelvls/slackcli/internal/testenv"
)

func auditLog(t *testing.T) string {
	t.Helper()
	testenv.Isolate(t)
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	t.Setenv("SLACKCLI_AUDIT_LOG", path)
	return path
}

func auditEntries(t *testing.T, path string) []audit.Entry {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []audit.Entry
	for _, line := range bytes.Split(bytes.TrimSpace(data), []byte("\n")) {
		var e audit.Entry
		if err := json.Unmarshal(line, &e); err != nil {
			t.Fatalf("line %q: %v", line, err)
		}
		out = append(out, e)
	}
	return out
}

// A call served by a fake handler is a line of the audit log, like one
// sent to Slack, and neither the parameters nor the credentials are in it.
func TestFakeCallIsAudited(t *testing.T) {
	path := auditLog(t)
	c := NewFake(func(_ context.Context, method string, _ map[string]any) (map[string]any, error) {
		if method == "auth.test" {
			return map[string]any{"ok": false, "error": "invalid_auth"}, nil
		}
		return map[string]any{"ok": true}, nil
	})
	if _, err := c.API(context.Background(), "chat.postMessage", map[string]any{"channel": "C1", "text": "a secret message"}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.API(context.Background(), "auth.test", nil); !IsAuthError(err) {
		t.Fatalf("err = %v", err)
	}

	raw, _ := os.ReadFile(path)
	for _, s := range []string{"secret", "xoxc-test", "xoxd-test"} {
		if strings.Contains(string(raw), s) {
			t.Errorf("the audit log holds %q:\n%s", s, raw)
		}
	}
	got := auditEntries(t, path)
	if len(got) != 2 {
		t.Fatalf("%d lines, want 2:\n%s", len(got), raw)
	}
	if e := got[0]; e.Kind != audit.KindAPI || e.Method != "chat.postMessage" || e.Host != "test.slack.com" || e.OK == nil || !*e.OK || e.Attempt != 1 || e.Error != "" {
		t.Errorf("first line = %+v", e)
	}
	if e := got[1]; e.Method != "auth.test" || e.OK == nil || *e.OK || e.Error != "invalid_auth" {
		t.Errorf("second line = %+v", e)
	}
}

// Each attempt of a call that Slack rate limits is a line, the retry
// included, with the wait Slack asked for.
func TestEachAttemptIsAudited(t *testing.T) {
	path := auditLog(t)
	t.Setenv("SLACKCLI_RATE_LIMIT_MAX_WAIT_MS", "30000")
	calls := 0
	c := browserClient(t, func(w http.ResponseWriter, _ *http.Request) {
		calls++
		switch calls {
		case 1:
			w.Header().Set("Retry-After", "3")
			w.WriteHeader(http.StatusTooManyRequests)
		case 2:
			w.WriteHeader(http.StatusInternalServerError)
		default:
			_, _ = io.WriteString(w, `{"ok":false,"error":"channel_not_found"}`)
		}
	}, WithSleep(func(context.Context, time.Duration) error { return nil }))

	if _, err := c.API(context.Background(), "conversations.history", nil); err == nil {
		t.Fatal("the 500 should fail the call")
	}
	if _, err := c.API(context.Background(), "conversations.info", nil); !IsErrorCode(err, "channel_not_found") {
		t.Fatalf("err = %v", err)
	}
	got := auditEntries(t, path)
	if len(got) != 3 {
		t.Fatalf("%d lines, want 3: %+v", len(got), got)
	}
	host := strings.TrimPrefix(c.WorkspaceURL(), "http://")
	if e := got[0]; e.Method != "conversations.history" || e.Host != host || e.Status != 429 || !e.RateLimited || e.RetryAfterS != 3 || e.Attempt != 1 || e.Error != "ratelimited" {
		t.Errorf("first line = %+v", e)
	}
	if e := got[1]; e.Method != "conversations.history" || e.Status != 500 || e.Attempt != 2 || e.Error != "http_status" || e.RateLimited {
		t.Errorf("second line = %+v", e)
	}
	if e := got[2]; e.Method != "conversations.info" || e.Status != 200 || e.OK == nil || *e.OK || e.Error != "channel_not_found" || e.Attempt != 1 {
		t.Errorf("third line = %+v", e)
	}
}

// A rate limit that Slack answers with an HTTP 200 and the "ratelimited"
// code is logged like an HTTP 429: rate limited, with the wait it asked
// for. So is one that a fake handler answers in the body.
func TestRateLimitCodeIsAuditedAsRateLimited(t *testing.T) {
	path := auditLog(t)
	t.Setenv("SLACKCLI_RATE_LIMIT_MAX_WAIT_MS", "30000")
	calls := 0
	c := browserClient(t, func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls == 1 {
			w.Header().Set("Retry-After", "7")
			_, _ = io.WriteString(w, `{"ok":false,"error":"ratelimited"}`)
			return
		}
		_, _ = io.WriteString(w, `{"ok":true}`)
	}, WithSleep(func(context.Context, time.Duration) error { return nil }))
	if _, err := c.API(context.Background(), "conversations.history", nil); err != nil {
		t.Fatal(err)
	}
	fake := NewFake(func(context.Context, string, map[string]any) (map[string]any, error) {
		return map[string]any{"ok": false, "error": "ratelimited"}, nil
	})
	var limited *RateLimitError
	if _, err := fake.API(context.Background(), "auth.test", nil); !errors.As(err, &limited) {
		t.Fatalf("err = %v, want a rate limit", err)
	}

	got := auditEntries(t, path)
	if len(got) != 3 {
		t.Fatalf("%d lines, want 3: %+v", len(got), got)
	}
	if e := got[0]; e.Status != 200 || !e.RateLimited || e.RetryAfterS != 7 || e.Attempt != 1 || e.Error != "ratelimited" {
		t.Errorf("first line = %+v", e)
	}
	if e := got[1]; e.Status != 200 || e.RateLimited || e.RetryAfterS != 0 || e.Attempt != 2 || e.Error != "" {
		t.Errorf("second line = %+v", e)
	}
	if e := got[2]; e.Method != "auth.test" || !e.RateLimited || e.RetryAfterS != limited.RetryAfter.Seconds() || e.Error != "ratelimited" {
		t.Errorf("third line = %+v", e)
	}
}
