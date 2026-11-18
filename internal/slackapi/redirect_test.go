package slackapi

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/maelvls/slackcli/internal/appinfo"
)

// redirector answers every request for acme.slack.com (or slack.com) with a
// redirect to location, and records every request made to any other host:
// there must be none.
type redirector struct {
	status   int
	location string

	mu     sync.Mutex
	served int
	leaked []string
}

func (r *redirector) RoundTrip(req *http.Request) (*http.Response, error) {
	body := ""
	if req.Body != nil {
		b, _ := io.ReadAll(req.Body)
		body = string(b)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if host := req.URL.Hostname(); host != "acme.slack.com" && host != "slack.com" {
		r.leaked = append(r.leaked, fmt.Sprintf("%s %s cookie=%q authorization=%q body=%q",
			req.Method, req.URL, req.Header.Get("Cookie"), req.Header.Get("Authorization"), body))
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"ok":true}`)), Request: req}, nil
	}
	r.served++
	return &http.Response{
		StatusCode: r.status,
		Header:     http.Header{"Location": {r.location}},
		Body:       io.NopCloser(strings.NewReader("redirect body with xoxc-echoed-secret")),
		Request:    req,
	}, nil
}

// An API call carries the credentials in its body (xoxc token) and
// headers. Whatever the status and wherever it points, a redirect is not
// followed, and the error gives nothing away.
func TestAPIRefusesRedirects(t *testing.T) {
	clients := map[string]func(*http.Client) *Client{
		"browser": func(h *http.Client) *Client {
			return New(Auth{Type: AuthBrowser, XoxcToken: "xoxc-secret", XoxdCookie: "xoxd-secret"}, "https://acme.slack.com", WithHTTPClient(h))
		},
		"standard": func(h *http.Client) *Client {
			return New(Auth{Type: AuthStandard, Token: "xoxb-secret"}, "", WithHTTPClient(h))
		},
	}
	locations := map[string]string{
		"foreign host":       "https://evil.example/api/chat.postMessage?token=xoxc-in-location",
		"foreign host, http": "http://evil.example/collect",
		"other Slack host":   "https://other.slack.com/api/chat.postMessage",
		"relative":           "/elsewhere",
	}
	for auth, newClient := range clients {
		for where, location := range locations {
			for _, status := range []int{301, 302, 303, 307, 308} {
				for _, multipart := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s/%s/%d/multipart=%v", auth, where, status, multipart), func(t *testing.T) {
						rt := &redirector{status: status, location: location}
						c := newClient(&http.Client{Transport: rt})
						call := c.API
						if multipart {
							call = c.APIMultipart
						}
						_, err := call(context.Background(), "chat.postMessage", map[string]any{"channel": "C1", "text": "hello"})
						if err == nil {
							t.Fatal("the redirect was followed, or taken for an answer")
						}
						if rt.served != 1 || len(rt.leaked) != 0 {
							t.Fatalf("%d requests to Slack and %d elsewhere, want 1 and 0: %q", rt.served, len(rt.leaked), rt.leaked)
						}
						msg := err.Error()
						for _, want := range []string{"chat.postMessage", fmt.Sprintf("HTTP %d", status), "not followed", "auth import-desktop"} {
							if !strings.Contains(msg, want) {
								t.Errorf("error %q does not say %q", msg, want)
							}
						}
						for _, secret := range []string{"xoxc-", "xoxd-", "xoxb-", "redirect body", "token="} {
							if strings.Contains(msg, secret) {
								t.Errorf("error %q gives away %q", msg, secret)
							}
						}
					})
				}
			}
		}
	}
}

// The same over real HTTP: the second server must not receive anything.
func TestAPIRedirectNeverReachesTheSecondServer(t *testing.T) {
	for _, status := range []int{301, 302, 307, 308} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var mu sync.Mutex
			received := 0
			second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				mu.Lock()
				received++
				mu.Unlock()
				_, _ = io.WriteString(w, `{"ok":true}`)
			}))
			defer second.Close()
			c := browserClient(t, func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, second.URL+"/api/auth.test", status)
			})
			_, err := c.API(context.Background(), "auth.test", nil)
			if err == nil || !strings.Contains(err.Error(), "redirect") {
				t.Fatalf("err = %v, want a refused redirect", err)
			}
			mu.Lock()
			defer mu.Unlock()
			if received != 0 {
				t.Fatalf("the second server received %d requests", received)
			}
		})
	}
}

// The no-redirect policy is the API's: the client handed out for downloads
// and uploads is the caller's, untouched.
func TestHTTPClientIsNotGivenTheAPIRedirectPolicy(t *testing.T) {
	given := &http.Client{}
	c := New(Auth{Type: AuthStandard, Token: "xoxb-1"}, "", WithHTTPClient(given))
	if c.HTTPClient() != given || given.CheckRedirect != nil {
		t.Fatalf("HTTPClient() = %p with CheckRedirect set = %v, want the client given (%p), unchanged", c.HTTPClient(), given.CheckRedirect != nil, given)
	}
}

// What an out-of-range float converts to depends on the architecture: an
// absurd Retry-After must be a capped, positive delay everywhere.
func TestParseRetryAfter(t *testing.T) {
	for in, want := range map[string]time.Duration{
		"":       5 * time.Second,
		"soon":   5 * time.Second,
		"NaN":    5 * time.Second,
		"0":      time.Second,
		"-3":     time.Second,
		"2.5":    2500 * time.Millisecond,
		"7":      7 * time.Second,
		"1e300":  maxRateLimitDelay,
		"Inf":    maxRateLimitDelay,
		"+Inf":   maxRateLimitDelay,
		"-Inf":   time.Second,
		"1e999":  5 * time.Second, // out of range for strconv: not a number
		"999999": maxRateLimitDelay,
	} {
		if got := parseRetryAfter(in); got != want {
			t.Errorf("parseRetryAfter(%q) = %s, want %s", in, got, want)
		}
	}
}

// The legacy variable follows the rule of the tool-specific ones: a value
// that cannot be used gives the default and is reported, never silently
// ignored (nor silently capped).
func TestLegacyAPITimeout(t *testing.T) {
	for _, name := range []string{appinfo.EnvName("API_TIMEOUT_MS"), appinfo.LegacyEnvPrefix + "API_TIMEOUT_MS"} {
		t.Setenv(name, "")
	}
	for in, want := range map[string]time.Duration{
		"":       defaultAPITimeout,
		"abc":    defaultAPITimeout,
		"NaN":    defaultAPITimeout,
		"0":      defaultAPITimeout,
		"-5":     defaultAPITimeout,
		"1e300":  defaultAPITimeout,
		"Inf":    defaultAPITimeout,
		"1500":   1500 * time.Millisecond,
		" 2500 ": 2500 * time.Millisecond,
	} {
		t.Setenv(LegacyAPITimeoutEnv, in)
		if got := APITimeout(); got != want {
			t.Errorf("%s=%q: %s, want %s", LegacyAPITimeoutEnv, in, got, want)
		}
		err := CheckLegacyAPITimeout()
		invalid := strings.TrimSpace(in) != "" && want == defaultAPITimeout
		switch {
		case !invalid && err != nil:
			t.Errorf("%s=%q: unexpected complaint: %v", LegacyAPITimeoutEnv, in, err)
		case invalid && (err == nil || !strings.Contains(err.Error(), fmt.Sprintf(`%s=%q is not valid: expected a positive number of milliseconds`, LegacyAPITimeoutEnv, in))):
			t.Errorf("%s=%q: complaint = %v", LegacyAPITimeoutEnv, in, err)
		}
	}

	// The tool-specific variable wins. The legacy one is then not read,
	// so there is nothing to say about it; unless the tool-specific one
	// is itself unusable.
	t.Setenv(LegacyAPITimeoutEnv, "abc")
	t.Setenv(appinfo.EnvName("API_TIMEOUT_MS"), "700")
	if got, err := APITimeout(), CheckLegacyAPITimeout(); got != 700*time.Millisecond || err != nil {
		t.Errorf("both set: timeout = %s, complaint = %v", got, err)
	}
	t.Setenv(appinfo.EnvName("API_TIMEOUT_MS"), "soon")
	if got, err := APITimeout(), CheckLegacyAPITimeout(); got != defaultAPITimeout || err == nil {
		t.Errorf("both invalid: timeout = %s, complaint = %v", got, err)
	}
	t.Setenv(LegacyAPITimeoutEnv, "900")
	if got, err := APITimeout(), CheckLegacyAPITimeout(); got != 900*time.Millisecond || err != nil {
		t.Errorf("legacy as the fallback of an invalid value: timeout = %s, complaint = %v", got, err)
	}
}
