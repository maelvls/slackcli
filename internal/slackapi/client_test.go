package slackapi

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func browserClient(t *testing.T, h http.HandlerFunc, opts ...Option) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	auth := Auth{Type: AuthBrowser, XoxcToken: "xoxc-test", XoxdCookie: "xoxd-a+b/c"}
	return New(auth, srv.URL+"/", append([]Option{WithLoopbackWorkspaceForTesting()}, opts...)...)
}

func TestBrowserFormCall(t *testing.T) {
	c := browserClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/conversations.history" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if got := r.Header.Get("Cookie"); got != "d=xoxd-a%2Bb%2Fc" {
			t.Errorf("cookie = %q", got)
		}
		if got := r.Header.Get("Origin"); got != "https://app.slack.com" {
			t.Errorf("origin = %q", got)
		}
		_ = r.ParseForm()
		want := map[string]string{"token": "xoxc-test", "channel": "C1", "limit": "20", "inclusive": "true", "blocks": `[{"type":"x&<y>"}]`}
		for k, v := range want {
			if r.PostForm.Get(k) != v {
				t.Errorf("%s = %q, want %q", k, r.PostForm.Get(k), v)
			}
		}
		if _, ok := r.PostForm["skipped"]; ok {
			t.Error("nil param was sent")
		}
		_, _ = io.WriteString(w, `{"ok":true,"messages":[{"ts":"1.2"}]}`)
	})
	resp, err := c.API(context.Background(), "conversations.history", map[string]any{
		"channel": "C1", "limit": 20, "inclusive": true, "skipped": nil,
		"blocks": []any{map[string]any{"type": "x&<y>"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp["messages"].([]any)) != 1 {
		t.Fatalf("resp = %v", resp)
	}
}

func TestStandardTokenUsesBearer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer xoxb-1" {
			t.Errorf("authorization = %q", got)
		}
		_ = r.ParseForm()
		if r.PostForm.Get("token") != "" {
			t.Error("token leaked into the form body")
		}
		_, _ = io.WriteString(w, `{"ok":false,"error":"missing_scope","needed":"channels:read","provided":"chat:write"}`)
	}))
	defer srv.Close()
	c := New(Auth{Type: AuthStandard, Token: "xoxb-1"}, "", WithBaseURL(srv.URL))
	_, err := c.API(context.Background(), "conversations.list", nil)
	if !IsErrorCode(err, "missing_scope") {
		t.Fatalf("err = %v", err)
	}
	want := "Slack API error calling conversations.list: missing_scope (needed: channels:read; provided: chat:write): "
	if !strings.HasPrefix(err.Error(), want) {
		t.Fatalf("message = %q", err.Error())
	}
	if IsAuthError(err) {
		t.Fatal("missing_scope is not an auth error")
	}
}

func TestMultipartRetriesOn429(t *testing.T) {
	t.Setenv("SLACKCLI_RATE_LIMIT_MAX_WAIT_MS", "30000")
	calls := 0
	var delays []time.Duration
	c := browserClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if !strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
			t.Errorf("content-type = %q", r.Header.Get("Content-Type"))
		}
		if calls == 1 {
			w.Header().Set("Retry-After", "2")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"ok":false,"error":"ratelimited"}`)
			return
		}
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Error(err)
		}
		if r.FormValue("title") != "Launch plan" || r.FormValue("token") != "xoxc-test" {
			t.Errorf("form = %v", r.MultipartForm.Value)
		}
		_, _ = io.WriteString(w, `{"ok":true,"file_id":"F12345678"}`)
	}, WithSleep(func(_ context.Context, d time.Duration) error { delays = append(delays, d); return nil }))

	resp, err := c.APIMultipart(context.Background(), "files.createCanvas", map[string]any{"title": "Launch plan"})
	if err != nil {
		t.Fatal(err)
	}
	if resp["file_id"] != "F12345678" || calls != 2 || len(delays) != 1 || delays[0] != 2*time.Second {
		t.Fatalf("resp=%v calls=%d delays=%v", resp, calls, delays)
	}
}

func TestRateLimitFailsFastByDefault(t *testing.T) {
	t.Setenv("SLACKCLI_RATE_LIMIT_MAX_WAIT_MS", "")
	t.Setenv("AGENT_SLACK_RATE_LIMIT_MAX_WAIT_MS", "")
	c := browserClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	_, err := c.API(context.Background(), "search.messages", nil)
	var rl *RateLimitError
	if !errors.As(err, &rl) || rl.RetryAfter != 7*time.Second {
		t.Fatalf("err = %v", err)
	}
}

// Some methods answer a rate limit with an HTTP 200 and the "ratelimited"
// code: it is the same *RateLimitError as an HTTP 429, which still carries
// the code, and it is waited for and retried the same way.
func TestRateLimitedCodeIsARateLimit(t *testing.T) {
	t.Setenv("SLACKCLI_RATE_LIMIT_MAX_WAIT_MS", "")
	t.Setenv("AGENT_SLACK_RATE_LIMIT_MAX_WAIT_MS", "")
	calls := 0
	var delays []time.Duration
	c := browserClient(t, func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls == 1 {
			w.Header().Set("Retry-After", "3")
		}
		if calls <= 2 {
			_, _ = io.WriteString(w, `{"ok":false,"error":"ratelimited"}`)
			return
		}
		_, _ = io.WriteString(w, `{"ok":true}`)
	}, WithSleep(func(_ context.Context, d time.Duration) error { delays = append(delays, d); return nil }))

	// Fails fast by default, with the wait Slack asked for.
	_, err := c.API(context.Background(), "users.list", nil)
	var rl *RateLimitError
	if !errors.As(err, &rl) || rl.Method != "users.list" || rl.RetryAfter != 3*time.Second || !IsErrorCode(err, RateLimitedCode) || !IsTransient(err) {
		t.Fatalf("err = %v", err)
	}
	if rl.API == nil || rl.API.Status != http.StatusOK {
		t.Errorf("API = %+v", rl.API)
	}
	if calls != 1 || len(delays) != 0 {
		t.Errorf("%d calls, waited %v", calls, delays)
	}

	// Retried when the caller allows the wait: without a Retry-After, as
	// for an HTTP 429, after 5s.
	t.Setenv("SLACKCLI_RATE_LIMIT_MAX_WAIT_MS", "30000")
	resp, err := c.API(context.Background(), "users.list", nil)
	if err != nil || resp["ok"] != true || calls != 3 || len(delays) != 1 || delays[0] != 5*time.Second {
		t.Fatalf("resp=%v err=%v calls=%d delays=%v", resp, err, calls, delays)
	}

	// The same from a handler (NewFake), which tests use.
	fake := NewFake(func(context.Context, string, map[string]any) (map[string]any, error) {
		return map[string]any{"ok": false, "error": "ratelimited"}, nil
	})
	if _, err := fake.API(context.Background(), "conversations.info", nil); !errors.As(err, &rl) || rl.Method != "conversations.info" || !IsErrorCode(err, RateLimitedCode) {
		t.Errorf("fake: err = %v", err)
	}
}

// chat.postMessage and chat.scheduleMessage answer "rate_limited" when the
// app posted too many messages (about one per second to a channel): the
// message was not posted. It is a rate limit like "ratelimited": a
// *RateLimitError that fails fast by default and says the message was not
// posted, and is retried only when the caller allows the wait.
func TestPostingRateLimitedCodeIsARateLimit(t *testing.T) {
	t.Setenv("SLACKCLI_RATE_LIMIT_MAX_WAIT_MS", "")
	t.Setenv("AGENT_SLACK_RATE_LIMIT_MAX_WAIT_MS", "")
	calls := 0
	var delays []time.Duration
	c := browserClient(t, func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls <= 2 {
			w.Header().Set("Retry-After", "2")
			_, _ = io.WriteString(w, `{"ok":false,"error":"rate_limited"}`)
			return
		}
		_, _ = io.WriteString(w, `{"ok":true,"ts":"1700000000.000100"}`)
	}, WithSleep(func(_ context.Context, d time.Duration) error { delays = append(delays, d); return nil }))

	_, err := c.API(context.Background(), "chat.postMessage", nil)
	var rl *RateLimitError
	if !errors.As(err, &rl) || rl.RetryAfter != 2*time.Second || !IsErrorCode(err, PostingRateLimitedCode) || !IsTransient(err) {
		t.Fatalf("err = %v", err)
	}
	for _, want := range []string{"chat.postMessage", "rate_limited", "too many messages", "was not posted", "send it again after 2s", "SLACKCLI_RATE_LIMIT_MAX_WAIT_MS"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not say %q: %v", want, err)
		}
	}
	if calls != 1 || len(delays) != 0 {
		t.Errorf("%d calls, waited %v", calls, delays)
	}

	t.Setenv("SLACKCLI_RATE_LIMIT_MAX_WAIT_MS", "30000")
	resp, err := c.API(context.Background(), "chat.postMessage", nil)
	if err != nil || resp["ts"] != "1700000000.000100" || calls != 3 || len(delays) != 1 || delays[0] != 2*time.Second {
		t.Fatalf("resp=%v err=%v calls=%d delays=%v", resp, err, calls, delays)
	}

	fake := NewFake(func(context.Context, string, map[string]any) (map[string]any, error) {
		return map[string]any{"ok": false, "error": "rate_limited"}, nil
	})
	if _, err := fake.API(context.Background(), "chat.scheduleMessage", nil); !errors.As(err, &rl) || !IsErrorCode(err, PostingRateLimitedCode) {
		t.Errorf("fake: err = %v", err)
	}
}

func TestTimeout(t *testing.T) {
	t.Setenv("SLACKCLI_API_TIMEOUT_MS", "30")
	// The handler hangs until the test is over. The request's context is
	// not enough to release it: the server only notices that the client
	// hung up once the handler has read the request body.
	release := make(chan struct{})
	c := browserClient(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	})
	// Registered after the server's own cleanup, so it runs before it:
	// closing the server waits for the handler to return.
	t.Cleanup(func() { close(release) })
	_, err := c.API(context.Background(), "auth.test", nil)
	if err == nil || !strings.Contains(err.Error(), "timed out after 30ms") {
		t.Fatalf("err = %v", err)
	}
}

func TestBrowserAuthNeedsWorkspace(t *testing.T) {
	c := New(Auth{Type: AuthBrowser, XoxcToken: "xoxc-1", XoxdCookie: "xoxd-1"}, "")
	if _, err := c.API(context.Background(), "auth.test", nil); err == nil || !strings.Contains(err.Error(), "--workspace") {
		t.Fatalf("err = %v", err)
	}
}

func TestFakeHandler(t *testing.T) {
	c := NewFake(func(_ context.Context, method string, params map[string]any) (map[string]any, error) {
		if method == "boom" {
			return map[string]any{"ok": false, "error": "invalid_auth"}, nil
		}
		return map[string]any{"echo": params["x"]}, nil
	})
	resp, err := c.API(context.Background(), "x", map[string]any{"x": 1})
	if err != nil || resp["echo"] != 1 {
		t.Fatalf("resp=%v err=%v", resp, err)
	}
	if _, err := c.API(context.Background(), "boom", nil); !IsAuthError(err) {
		t.Fatalf("err = %v", err)
	}
}

// chat.update keeps the blocks of a message unless it is given others: a
// caller that means "no blocks" passes an empty, non-nil array, which must
// reach Slack as "[]". A nil one is dropped like any nil parameter.
func TestEncodeParamsSendsEmptyArrays(t *testing.T) {
	got, err := EncodeParams(map[string]any{"blocks": []any{}, "attachments": []any(nil), "text": "", "unset": nil})
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := got["blocks"]; !ok || v != "[]" {
		t.Errorf("blocks = %q (sent = %v), want []", v, ok)
	}
	if v, ok := got["text"]; !ok || v != "" {
		t.Errorf("text = %q (sent = %v), want an empty string", v, ok)
	}
	for _, k := range []string{"attachments", "unset"} {
		if v, ok := got[k]; ok {
			t.Errorf("%s was sent as %q, want it dropped", k, v)
		}
	}
}

func TestDecodeCookieD(t *testing.T) {
	for in, want := range map[string]string{
		"xoxd-abc%2Bdef%2F": "xoxd-abc+def/",
		" xoxd-abc+def ":    "xoxd-abc+def",
		"xoxd-100%":         "xoxd-100%",
	} {
		if got := DecodeCookieD(in); got != want {
			t.Errorf("DecodeCookieD(%q) = %q, want %q", in, got, want)
		}
	}
	if got := EncodeCookieD("a b+c/d"); got != "a%20b%2Bc%2Fd" {
		t.Errorf("EncodeCookieD = %q", got)
	}
}
