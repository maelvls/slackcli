package slackapi

import (
	"context"
	"io"
	"net/http"
	"testing"
	"time"
)

// An answer whose "ok" is missing or not a boolean fails the call, and is
// a failure in the audit log too.
func TestRejectedAnswerIsAuditedAsFailure(t *testing.T) {
	path := auditLog(t)
	bodies := []string{`{}`, `{"ok":"false"}`}
	calls := 0
	c := browserClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, bodies[calls])
		calls++
	})
	for range bodies {
		if _, err := c.API(context.Background(), "auth.test", nil); err == nil {
			t.Fatal("the call should fail")
		}
	}
	fake := NewFake(func(context.Context, string, map[string]any) (map[string]any, error) {
		return map[string]any{"ok": "false"}, nil
	})
	if _, err := fake.API(context.Background(), "auth.test", nil); err == nil {
		t.Fatal("the fake call should fail")
	}
	got := auditEntries(t, path)
	if len(got) != 3 {
		t.Fatalf("%d lines, want 3: %+v", len(got), got)
	}
	for i, e := range got {
		if e.Error != "not_ok" || !e.Failed() {
			t.Errorf("line %d = %+v, want the failure \"not_ok\"", i, e)
		}
	}
}

// A call whose context's deadline expires is a "timeout", not "cancelled".
func TestExpiredDeadlineIsAuditedAsTimeout(t *testing.T) {
	path := auditLog(t)
	release := make(chan struct{})
	c := browserClient(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	})
	// Runs before the server is closed, which waits for the handler.
	t.Cleanup(func() { close(release) })
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := c.API(ctx, "auth.test", nil); err == nil {
		t.Fatal("the call should fail")
	}
	got := auditEntries(t, path)
	if len(got) != 1 || got[0].Error != "timeout" {
		t.Fatalf("lines = %+v, want one \"timeout\"", got)
	}
}
