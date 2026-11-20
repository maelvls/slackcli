package channelcmd

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/maelvls/slackcli/internal/slackapi"
)

// infoHarness is a harness whose conversations.info answers with channel.
func infoHarness(t *testing.T, channel map[string]any) *harness {
	t.Helper()
	h := newHarness(t)
	h.rt.ClientOptions = []slackapi.Option{slackapi.WithHandler(func(_ context.Context, method string, params map[string]any) (map[string]any, error) {
		h.mu.Lock()
		h.calls = append(h.calls, call{method, params})
		h.mu.Unlock()
		if method == "conversations.info" && h.fail[method] == "" {
			return map[string]any{"channel": channel}, nil
		}
		return h.serve(method, params), nil
	})}
	return h
}

var generalChannel = map[string]any{
	"id": "C000000001", "name": "general", "is_channel": true, "is_private": false, "is_archived": false,
	"is_member": true, "num_members": 42, "created": 1490301622, "creator": "U000000123",
	"topic":      map[string]any{"value": "Company news", "creator": "U000000123", "last_set": 1490301700},
	"purpose":    map[string]any{"value": "Announcements"},
	"properties": map[string]any{"tabs": []any{}},
}

func TestInfo(t *testing.T) {
	h := infoHarness(t, generalChannel)
	want := map[string]any{
		"id": "C000000001", "name": "general", "is_member": true, "num_members": float64(42),
		"topic": "Company news", "purpose": "Announcements", "creator": "U000000123", "created": float64(1490301622),
	}
	wantInfo := call{"conversations.info", map[string]any{"channel": "C000000001", "include_num_members": true}}

	// By ID: one call.
	out, err := h.run("info", "C000000001")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(out, want) {
		t.Errorf("printed %s", h.stdout)
	}
	if !reflect.DeepEqual(h.calls, []call{wantInfo}) {
		t.Errorf("calls = %+v", h.calls)
	}

	// By name, the channel is resolved first; a message URL names it.
	for target, id := range map[string]string{"#general": "C000000001", "general": "C000000001", messageURL: "C12345678"} {
		out, err := h.run("info", target)
		if err != nil {
			t.Fatalf("info %s: %v", target, err)
		}
		if out["name"] != "general" || out["topic"] != "Company news" {
			t.Errorf("info %s printed %s", target, h.stdout)
		}
		if got := h.only("conversations.info"); len(got) != 1 || got[0].params["channel"] != id {
			t.Errorf("info %s: conversations.info calls = %+v", target, got)
		}
	}
}

func TestInfoRaw(t *testing.T) {
	h := infoHarness(t, generalChannel)
	out, err := h.run("info", "C000000001", "--raw")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(out["topic"], map[string]any{"value": "Company news", "creator": "U000000123", "last_set": float64(1490301700)}) ||
		out["is_channel"] != true || out["is_private"] != false {
		t.Errorf("printed %s", h.stdout)
	}
}

// A user ID designates the DM that exists with that user: none is opened.
func TestInfoOfDM(t *testing.T) {
	h := infoHarness(t, map[string]any{"id": "D000000001", "is_im": true, "user": "U000000123", "created": 1561496765})
	h.listing = func(map[string]any) map[string]any {
		return map[string]any{"channels": []any{map[string]any{"id": "D000000001", "user": "U000000123"}}}
	}
	out, err := h.run("info", "U000000123")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"id": "D000000001", "user": "U000000123", "is_im": true, "created": float64(1561496765)}
	if !reflect.DeepEqual(out, want) {
		t.Errorf("printed %s", h.stdout)
	}
	for _, m := range h.methods() {
		if m == "conversations.open" {
			t.Errorf("a DM was opened: calls = %v", h.methods())
		}
	}
}

// Without a DM with that user, the error is about describing, not reading.
func TestInfoOfMissingDM(t *testing.T) {
	h := infoHarness(t, generalChannel)
	h.listing = func(map[string]any) map[string]any { return map[string]any{"channels": []any{}} }
	_, err := h.run("info", "U000000123")
	if err == nil || !strings.Contains(err.Error(), "there is no direct message with user U000000123") ||
		!strings.Contains(err.Error(), "no conversation to describe") || strings.Contains(err.Error(), "to read") {
		t.Errorf("error = %v", err)
	}
	for _, m := range h.methods() {
		if m == "conversations.open" || m == "conversations.info" {
			t.Errorf("calls = %v", h.methods())
		}
	}
}

func TestInfoErrors(t *testing.T) {
	h := infoHarness(t, generalChannel)
	h.fail["conversations.info"] = "channel_not_found"
	if _, err := h.run("info", "C000000009"); err == nil || !strings.Contains(err.Error(), "looking up conversation C000000009") ||
		!strings.Contains(err.Error(), "channel_not_found") {
		t.Errorf("error = %v", err)
	}
	if _, err := h.run("info"); err == nil {
		t.Error("info without a target succeeded")
	}
	if _, err := h.run("info", "#"); err == nil || len(h.calls) != 0 {
		t.Errorf("info #: error = %v, calls = %+v", err, h.calls)
	}
}
