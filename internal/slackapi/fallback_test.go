package slackapi

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/maelvls/slackcli/internal/appinfo"
)

// answering returns a fake client for workspaceURL that answers every call
// with the error code given for its method ("" or none: ok), and the
// methods it was asked.
func answering(workspaceURL string, codes map[string]string, opts ...Option) (*Client, *[]string) {
	calls := new([]string)
	h := func(_ context.Context, method string, _ map[string]any) (map[string]any, error) {
		*calls = append(*calls, method)
		if code := codes[method]; code != "" {
			return map[string]any{"ok": false, "error": code}, nil
		}
		return map[string]any{"ok": true, "from": workspaceURL}, nil
	}
	auth := Auth{Type: AuthBrowser, XoxcToken: "xoxc-test", XoxdCookie: "xoxd-test"}
	return New(auth, workspaceURL, append([]Option{WithHandler(h)}, opts...)...), calls
}

// restrictedDrafts is the fallback of the tests: drafts.* refused with
// team_is_restricted go to other.
func restrictedDrafts(other *Client, why string, asked *int) Option {
	return WithFallback(Fallback{
		Applies: func(method string, err error) bool {
			return strings.HasPrefix(method, "drafts.") && IsErrorCode(err, "team_is_restricted")
		},
		Client: func(context.Context) (*Client, string) {
			if asked != nil {
				*asked++
			}
			return other, why
		},
	})
}

func TestFallbackRetriesARefusedCallWithTheOtherClient(t *testing.T) {
	const ws, org = "https://acme-eng.slack.com", "https://acme.enterprise.slack.com"
	other, otherCalls := answering(org, nil)
	asked := 0
	c, calls := answering(ws, map[string]string{
		"drafts.list":   "team_is_restricted",
		"client.counts": "team_is_restricted",
		"drafts.update": "draft_not_found",
	}, restrictedDrafts(other, "", &asked))
	ctx := context.Background()

	// A call that works is not retried, and the other client is not even
	// looked for.
	if resp, err := c.API(ctx, "drafts.create", nil); err != nil || resp["from"] != ws {
		t.Fatalf("drafts.create: resp = %v, err = %v", resp, err)
	}
	// Neither is a call the fallback does not apply to: another method,
	// another error.
	if _, err := c.API(ctx, "client.counts", nil); !IsErrorCode(err, "team_is_restricted") {
		t.Fatalf("client.counts: err = %v", err)
	}
	if _, err := c.API(ctx, "drafts.update", nil); !IsErrorCode(err, "draft_not_found") {
		t.Fatalf("drafts.update: err = %v", err)
	}
	if asked != 0 || len(*otherCalls) != 0 {
		t.Fatalf("the other client was asked for %d times and called %q before any call was refused", asked, *otherCalls)
	}

	// The refused call is made once more, with the other client, in the
	// same form.
	for _, call := range []func(context.Context, string, map[string]any) (map[string]any, error){c.API, c.APIMultipart} {
		if resp, err := call(ctx, "drafts.list", nil); err != nil || resp["from"] != org {
			t.Fatalf("drafts.list: resp = %v, err = %v", resp, err)
		}
	}
	if want := []string{"drafts.create", "client.counts", "drafts.update", "drafts.list", "drafts.list"}; !reflect.DeepEqual(*calls, want) {
		t.Errorf("calls = %q, want %q", *calls, want)
	}
	if want := []string{"drafts.list", "drafts.list"}; !reflect.DeepEqual(*otherCalls, want) {
		t.Errorf("calls of the other client = %q, want %q", *otherCalls, want)
	}
}

// When the retry cannot work, the error is the refusal of the client that
// was asked (same code, same method), and says what was tried.
func TestFallbackThatCannotAnswer(t *testing.T) {
	const ws, org = "https://acme-eng.slack.com", "https://acme.enterprise.slack.com"
	ctx := context.Background()
	refused := map[string]string{"drafts.list": "team_is_restricted"}
	general := explain("team_is_restricted")

	t.Run("no other client, and nothing known", func(t *testing.T) {
		c, _ := answering(ws, refused, restrictedDrafts(nil, "", nil))
		_, err := c.API(ctx, "drafts.list", nil)
		if !IsErrorCode(err, "team_is_restricted") || !strings.HasSuffix(err.Error(), general) {
			t.Errorf("err = %v, want the general explanation", err)
		}
	})

	t.Run("no other client, and why", func(t *testing.T) {
		c, _ := answering(ws, refused, restrictedDrafts(nil, "the organisation is signed out", nil))
		_, err := c.API(ctx, "drafts.list", nil)
		var ae *APIError
		if !errors.As(err, &ae) || ae.Code != "team_is_restricted" || ae.Method != "drafts.list" || ae.Response["error"] != "team_is_restricted" {
			t.Fatalf("err = %#v, want the refusal itself", err)
		}
		if got, want := err.Error(), "Slack API error calling drafts.list: team_is_restricted: the organisation is signed out"; got != want {
			t.Errorf("err = %q, want %q", got, want)
		}
	})

	t.Run("the client is its own fallback", func(t *testing.T) {
		var c *Client
		c, calls := answering(ws, refused, WithFallback(Fallback{
			Applies: func(string, error) bool { return true },
			Client:  func(context.Context) (*Client, string) { return c, "" },
		}))
		if _, err := c.API(ctx, "drafts.list", nil); !IsErrorCode(err, "team_is_restricted") || len(*calls) != 1 {
			t.Errorf("err = %v after %d calls, want one refused call", err, len(*calls))
		}
	})

	t.Run("the other credentials are rejected", func(t *testing.T) {
		other, _ := answering(org, map[string]string{"drafts.list": "invalid_auth"})
		c, _ := answering(ws, refused, restrictedDrafts(other, "", nil))
		_, err := c.API(ctx, "drafts.list", nil)
		// Not an auth error: the credentials of the workspace that was
		// asked for are fine, and must not be refreshed or reported dead.
		if !IsErrorCode(err, "team_is_restricted") || IsAuthError(err) {
			t.Fatalf("err = %v, want team_is_restricted", err)
		}
		for _, want := range []string{org, "(invalid_auth)", "`" + appinfo.Name + " auth import-desktop`"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("err = %v, does not say %q", err, want)
			}
		}
		if strings.Contains(err.Error(), general) {
			t.Errorf("err = %v: the general explanation is given as well", err)
		}
	})

	t.Run("the other client is refused as well", func(t *testing.T) {
		other, _ := answering(org, map[string]string{"drafts.list": "enterprise_is_restricted"})
		c, _ := answering(ws, refused, restrictedDrafts(other, "", nil))
		_, err := c.API(ctx, "drafts.list", nil)
		if !IsErrorCode(err, "team_is_restricted") || !strings.Contains(err.Error(), org) || !strings.Contains(err.Error(), "(enterprise_is_restricted)") {
			t.Errorf("err = %v, want team_is_restricted naming what was tried", err)
		}
	})

	t.Run("another failure of the other client is the answer", func(t *testing.T) {
		other, _ := answering(org, map[string]string{"drafts.list": "internal_error"})
		c, _ := answering(ws, refused, restrictedDrafts(other, "", nil))
		if _, err := c.API(ctx, "drafts.list", nil); !IsErrorCode(err, "internal_error") {
			t.Errorf("err = %v, want internal_error", err)
		}
	})

	t.Run("the fallback of the other client is not followed", func(t *testing.T) {
		third, thirdCalls := answering("https://third.slack.com", nil)
		other, _ := answering(org, refused, restrictedDrafts(third, "", nil))
		c, _ := answering(ws, refused, restrictedDrafts(other, "", nil))
		if _, err := c.API(ctx, "drafts.list", nil); !IsErrorCode(err, "team_is_restricted") || len(*thirdCalls) != 0 {
			t.Errorf("err = %v, third client called %d times", err, len(*thirdCalls))
		}
	})
}

// Looking for the other client can take calls of its own. A command that
// is interrupted meanwhile was interrupted: it must not report the refusal
// it was working around (and exit as if Slack had answered).
func TestFallbackInterruptedWhileLookingForTheOtherClient(t *testing.T) {
	other, otherCalls := answering("https://acme.enterprise.slack.com", nil)
	for name, found := range map[string]*Client{"none found": nil, "one found": other} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			c, _ := answering("https://acme-eng.slack.com", map[string]string{"drafts.list": "team_is_restricted"}, WithFallback(Fallback{
				Applies: func(string, error) bool { return true },
				Client: func(context.Context) (*Client, string) {
					cancel()
					return found, "the organisation is signed out"
				},
			}))
			_, err := c.API(ctx, "drafts.list", nil)
			if !errors.Is(err, context.Canceled) || IsErrorCode(err, "team_is_restricted") {
				t.Errorf("err = %v, want context.Canceled", err)
			}
			if len(*otherCalls) != 0 {
				t.Errorf("the other client was called %d times after the interruption", len(*otherCalls))
			}
		})
	}
}
