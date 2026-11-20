package clictx

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/maelvls/slackcli/internal/appinfo"
	"github.com/maelvls/slackcli/internal/slackapi"
)

// site is what one Slack host answers in a test: the session it accepts,
// who that session is (auth.test), and the methods it refuses.
type site struct {
	token string
	who   map[string]any
	// refuses gives the error code of the methods the host refuses.
	refuses map[string]string
}

// slack is a fake Slack, as an http.RoundTripper: nothing leaves the
// process, and each call is recorded as "<host> <method>". A host is given
// the session of another one answers invalid_auth, so that a test fails
// when a call is made with the wrong session.
type slack struct {
	mu    sync.Mutex
	sites map[string]*site
	calls []string
	// during, when set, runs while a call ("<host> <method>") is on its
	// way: a test cancels the context there.
	during func(call string)
}

func (s *slack) RoundTrip(req *http.Request) (*http.Response, error) {
	_ = req.ParseMultipartForm(1 << 20) // reads a URL-encoded form as well
	method := strings.TrimPrefix(req.URL.Path, "/api/")

	s.mu.Lock()
	s.calls = append(s.calls, req.URL.Host+" "+method)
	st := s.sites[req.URL.Host]
	during := s.during
	s.mu.Unlock()
	if during != nil {
		during(req.URL.Host + " " + method)
	}
	if err := req.Context().Err(); err != nil {
		return nil, err
	}

	var answer map[string]any
	switch {
	case st == nil || req.PostFormValue("token") != st.token:
		answer = map[string]any{"ok": false, "error": "invalid_auth"}
	case st.refuses[method] != "":
		answer = map[string]any{"ok": false, "error": st.refuses[method]}
	case method == "auth.test":
		answer = map[string]any{"ok": true}
		for k, v := range st.who {
			answer[k] = v
		}
	default:
		answer = map[string]any{"ok": true, "answered_by": req.URL.Host}
	}
	raw, _ := json.Marshal(answer)
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(string(raw))),
		Request:    req,
	}, nil
}

// took returns the calls made since the last time it was asked.
func (s *slack) took() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	calls := s.calls
	s.calls = nil
	return calls
}

const (
	engURL = "https://acme-eng.slack.com"
	orgURL = "https://acme.enterprise.slack.com"
)

// grid returns a context whose store holds the sessions of a workspace of
// an Enterprise Grid (the default) and of its organisation, and the fake
// Slack they talk to: the workspace is refused drafts.list, drafts.delete
// and client.counts, as on a real Grid.
func grid(t *testing.T) (*Context, *slack) {
	t.Helper()
	s := &slack{sites: map[string]*site{
		"acme-eng.slack.com": {
			token: "xoxc-eng",
			who:   map[string]any{"team_id": "T0ENG", "enterprise_id": "E0ACME", "user_id": "U0ME"},
			refuses: map[string]string{
				"drafts.list":   "team_is_restricted",
				"drafts.delete": "team_is_restricted",
				"client.counts": "team_is_restricted",
			},
		},
		"acme.enterprise.slack.com": {
			token: "xoxc-org",
			who:   map[string]any{"team_id": "E0ACME", "enterprise_id": "E0ACME", "user_id": "U0ME"},
		},
	}}
	c := newContext(t)
	c.ClientOptions = []slackapi.Option{slackapi.WithHTTPClient(&http.Client{Transport: s})}
	mustUpsert(t, c, browser(engURL, "xoxc-eng"), browser(orgURL, "xoxc-org"))
	if err := c.Store.SetDefault(engURL); err != nil {
		t.Fatal(err)
	}
	return c, s
}

func mustClient(t *testing.T, c *Context, selector string) *slackapi.Client {
	t.Helper()
	r, err := c.Client(context.Background(), selector)
	if err != nil {
		t.Fatal(err)
	}
	return r.Client
}

func TestIsOrgURL(t *testing.T) {
	for u, want := range map[string]bool{
		"https://acme.enterprise.slack.com":              true,
		"https://ACME.Enterprise.Slack.com/":             true,
		"https://acme-eng.slack.com":                     false,
		"https://enterprise.slack.com":                   false,
		"https://acme.enterprise.slack.com.evil.example": false,
		"": false,
	} {
		if got := IsOrgURL(u); got != want {
			t.Errorf("IsOrgURL(%q) = %v, want %v", u, got, want)
		}
	}
}

// drafts.list and drafts.delete, refused to the session of a workspace of
// a Grid, are answered by the stored session of its organisation, once
// auth.test has said that it is the same organisation and the same person.
func TestOrgFallbackAnswersRestrictedDraftCalls(t *testing.T) {
	c, s := grid(t)
	r, err := c.Client(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if r.WorkspaceURL != engURL || r.Auth.XoxcToken != "xoxc-eng" {
		t.Fatalf("resolved %s with %s: the command still runs in the workspace", r.WorkspaceURL, r.Auth.XoxcToken)
	}

	resp, err := r.Client.API(context.Background(), "drafts.list", nil)
	if err != nil || resp["answered_by"] != "acme.enterprise.slack.com" {
		t.Fatalf("drafts.list: resp = %v, err = %v", resp, err)
	}
	want := []string{
		"acme-eng.slack.com drafts.list",
		"acme-eng.slack.com auth.test",
		"acme.enterprise.slack.com auth.test",
		"acme.enterprise.slack.com drafts.list",
	}
	if got := s.took(); !reflect.DeepEqual(got, want) {
		t.Errorf("calls = %q, want %q", got, want)
	}

	// The organisation is checked once per client, not once per call.
	resp, err = r.Client.APIMultipart(context.Background(), "drafts.delete", map[string]any{"draft_id": "Dr1"})
	if err != nil || resp["answered_by"] != "acme.enterprise.slack.com" {
		t.Fatalf("drafts.delete: resp = %v, err = %v", resp, err)
	}
	want = []string{"acme-eng.slack.com drafts.delete", "acme.enterprise.slack.com drafts.delete"}
	if got := s.took(); !reflect.DeepEqual(got, want) {
		t.Errorf("calls = %q, want %q", got, want)
	}

	// What the workspace allows is not asked of the organisation.
	resp, err = r.Client.API(context.Background(), "drafts.create", nil)
	if err != nil || resp["answered_by"] != "acme-eng.slack.com" {
		t.Fatalf("drafts.create: resp = %v, err = %v", resp, err)
	}
	if got := s.took(); !reflect.DeepEqual(got, []string{"acme-eng.slack.com drafts.create"}) {
		t.Errorf("calls = %q", got)
	}
}

// Only drafts.* refused with team_is_restricted are retried: any other
// method, and any other refusal, fails as it did, in one call. The error
// of team_is_restricted names the workaround.
func TestOrgFallbackIsOnlyForRestrictedDraftCalls(t *testing.T) {
	c, s := grid(t)
	s.sites["acme-eng.slack.com"].refuses["drafts.update"] = "enterprise_is_restricted"
	s.sites["acme-eng.slack.com"].refuses["drafts.create"] = "user_is_restricted"
	s.sites["acme-eng.slack.com"].refuses["conversations.list"] = "team_is_restricted"
	client := mustClient(t, c, "")

	for method, code := range map[string]string{
		"client.counts":      "team_is_restricted",
		"conversations.list": "team_is_restricted",
		"drafts.update":      "enterprise_is_restricted",
		"drafts.create":      "user_is_restricted",
	} {
		_, err := client.API(context.Background(), method, nil)
		if !slackapi.IsErrorCode(err, code) {
			t.Errorf("%s: err = %v, want %s", method, err, code)
		}
		if got := s.took(); !reflect.DeepEqual(got, []string{"acme-eng.slack.com " + method}) {
			t.Errorf("%s: calls = %q, want the one refused call", method, got)
		}
		if named := strings.Contains(err.Error(), "--workspace <org>.enterprise.slack.com"); named != (code == "team_is_restricted") {
			t.Errorf("%s: err = %v: the organisation workaround is for team_is_restricted only", method, err)
		}
	}
}

// A stored organisation is used only when auth.test says that it is the
// organisation of the workspace, signed in as the same person. When it is
// not, the call fails as it did, and the error says what is stored.
func TestOrgFallbackNeverUsesAnotherOrganisationOrPerson(t *testing.T) {
	for name, tc := range map[string]struct {
		who  map[string]any
		says []string
		not  string
	}{
		"another organisation": {
			who:  map[string]any{"team_id": "E0OTHER", "enterprise_id": "E0OTHER", "user_id": "U0ME"},
			says: []string{"is another organisation (E0OTHER, when this workspace belongs to E0ACME)"},
			not:  "another person",
		},
		"another person": {
			who:  map[string]any{"team_id": "E0ACME", "enterprise_id": "E0ACME", "user_id": "U0SOMEONE"},
			says: []string{"is signed in as another person (U0SOMEONE, when this workspace is signed in as U0ME)"},
			not:  "another organisation",
		},
		"no identity": {
			who:  map[string]any{},
			says: []string{"is another organisation (none, ", " and is signed in as another person (none, "},
		},
	} {
		t.Run(name, func(t *testing.T) {
			who := tc.who
			c, s := grid(t)
			s.sites["acme.enterprise.slack.com"].who = who

			_, err := mustClient(t, c, "").API(context.Background(), "drafts.list", nil)
			if !slackapi.IsErrorCode(err, "team_is_restricted") {
				t.Fatalf("err = %v, want team_is_restricted", err)
			}
			for _, want := range append([]string{"Slack API error calling drafts.list: team_is_restricted: ", orgURL, "`" + appinfo.Name + " auth import-desktop`"}, tc.says...) {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("err = %v, does not say %q", err, want)
				}
			}
			if tc.not != "" && strings.Contains(err.Error(), tc.not) {
				t.Errorf("err = %v, says %q, which is not what differs", err, tc.not)
			}
			for _, call := range s.took() {
				if call == "acme.enterprise.slack.com drafts.list" {
					t.Error("drafts.list was sent to an organisation that is not the workspace's")
				}
			}
		})
	}

	t.Run("a workspace that is not part of a Grid", func(t *testing.T) {
		c, s := grid(t)
		s.sites["acme-eng.slack.com"].who = map[string]any{"team_id": "T0ENG", "user_id": "U0ME"}

		_, err := mustClient(t, c, "").API(context.Background(), "drafts.list", nil)
		if !slackapi.IsErrorCode(err, "team_is_restricted") {
			t.Fatalf("err = %v, want team_is_restricted", err)
		}
		if got := s.took(); !reflect.DeepEqual(got, []string{"acme-eng.slack.com drafts.list", "acme-eng.slack.com auth.test"}) {
			t.Errorf("calls = %q: nothing is asked of the organisation", got)
		}
	})
}

// When the session of the organisation cannot answer either, the error is
// still the refusal of the workspace, and says what would make it work.
func TestOrgFallbackWhenTheOrganisationCannotAnswer(t *testing.T) {
	t.Run("its session is rejected", func(t *testing.T) {
		c, s := grid(t)
		s.sites["acme.enterprise.slack.com"].token = "xoxc-newer"

		_, err := mustClient(t, c, "").API(context.Background(), "drafts.list", nil)
		if !slackapi.IsErrorCode(err, "team_is_restricted") || slackapi.IsAuthError(err) {
			t.Fatalf("err = %v, want team_is_restricted: the session of the workspace is fine", err)
		}
		for _, want := range []string{orgURL, "Slack rejected it (invalid_auth)", "`" + appinfo.Name + " auth import-desktop`"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("err = %v, does not say %q", err, want)
			}
		}
	})

	t.Run("it is refused the method as well", func(t *testing.T) {
		c, s := grid(t)
		s.sites["acme.enterprise.slack.com"].refuses = map[string]string{"drafts.list": "team_is_restricted"}

		_, err := mustClient(t, c, "").API(context.Background(), "drafts.list", nil)
		if !slackapi.IsErrorCode(err, "team_is_restricted") {
			t.Fatalf("err = %v, want team_is_restricted", err)
		}
		if !strings.Contains(err.Error(), "so are the ones stored for "+orgURL) {
			t.Errorf("err = %v, does not say that the organisation was tried", err)
		}
	})

	t.Run("another failure of the organisation is the answer", func(t *testing.T) {
		c, s := grid(t)
		s.sites["acme.enterprise.slack.com"].refuses = map[string]string{"drafts.delete": "draft_not_found"}

		_, err := mustClient(t, c, "").API(context.Background(), "drafts.delete", nil)
		if !slackapi.IsErrorCode(err, "draft_not_found") {
			t.Fatalf("err = %v, want draft_not_found", err)
		}
	})

	t.Run("no organisation is stored", func(t *testing.T) {
		c, s := grid(t)
		if _, err := c.Store.Remove(orgURL); err != nil {
			t.Fatal(err)
		}
		_, err := mustClient(t, c, "").API(context.Background(), "drafts.list", nil)
		if !slackapi.IsErrorCode(err, "team_is_restricted") || !strings.Contains(err.Error(), "--workspace <org>.enterprise.slack.com") {
			t.Fatalf("err = %v, want team_is_restricted and the workaround", err)
		}
		if got := s.took(); !reflect.DeepEqual(got, []string{"acme-eng.slack.com drafts.list"}) {
			t.Errorf("calls = %q, want the one refused call", got)
		}
	})

	// --workspace <org> would run the command with that token, which the
	// draft commands refuse: the generic advice is not given.
	t.Run("the organisation is stored with a standard token", func(t *testing.T) {
		c, s := grid(t)
		mustUpsert(t, c, standard(orgURL, "xoxp-org"))
		_, err := mustClient(t, c, "").API(context.Background(), "drafts.list", nil)
		if !slackapi.IsErrorCode(err, "team_is_restricted") {
			t.Fatalf("err = %v, want team_is_restricted", err)
		}
		for _, want := range []string{orgURL + " is stored with a standard token", "`" + appinfo.Name + " auth remove " + orgURL + "`"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("err = %v, does not say %q", err, want)
			}
		}
		if strings.Contains(err.Error(), "--workspace <org>") {
			t.Errorf("err = %v: suggests --workspace <org>, which would use the standard token", err)
		}
		if got := s.took(); !reflect.DeepEqual(got, []string{"acme-eng.slack.com drafts.list"}) {
			t.Errorf("calls = %q, want the one refused call", got)
		}
	})

	// An identity check that got no answer says nothing about whose
	// session is stored: the error says to try again, not to import.
	for name, host := range map[string]string{
		"the check of the workspace fails for a moment":    "acme-eng.slack.com",
		"the check of the organisation fails for a moment": "acme.enterprise.slack.com",
	} {
		t.Run(name, func(t *testing.T) {
			c, s := grid(t)
			if s.sites[host].refuses == nil {
				s.sites[host].refuses = map[string]string{}
			}
			s.sites[host].refuses["auth.test"] = "internal_error"

			_, err := mustClient(t, c, "").API(context.Background(), "drafts.list", nil)
			if !slackapi.IsErrorCode(err, "team_is_restricted") {
				t.Fatalf("err = %v, want team_is_restricted", err)
			}
			for _, want := range []string{"could not be checked just now (auth.test of https://" + host + ": internal_error)", "run the command again"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("err = %v, does not say %q", err, want)
				}
			}
			for _, not := range []string{"another organisation", "another person", "import-desktop"} {
				if strings.Contains(err.Error(), not) {
					t.Errorf("err = %v, says %q about a check that got no answer", err, not)
				}
			}
			for _, call := range s.took() {
				if call == "acme.enterprise.slack.com drafts.list" {
					t.Error("drafts.list was sent to an organisation that was not checked")
				}
			}
		})
	}
}

// SLACK_TOKEN is one identity: a stored session is never used in its
// place. And the organisation itself has nothing to fall back to.
func TestOrgFallbackIsOnlyForStoredWorkspaceSessions(t *testing.T) {
	t.Run("SLACK_TOKEN", func(t *testing.T) {
		c, s := grid(t)
		t.Setenv("SLACK_TOKEN", "xoxc-eng")
		t.Setenv("SLACK_COOKIE_D", "xoxd-stored")
		t.Setenv("SLACK_WORKSPACE_URL", engURL)

		_, err := mustClient(t, c, "").API(context.Background(), "drafts.list", nil)
		if !slackapi.IsErrorCode(err, "team_is_restricted") {
			t.Fatalf("err = %v, want team_is_restricted", err)
		}
		if got := s.took(); !reflect.DeepEqual(got, []string{"acme-eng.slack.com drafts.list"}) {
			t.Errorf("calls = %q, want the one refused call", got)
		}
	})

	t.Run("--workspace <org>", func(t *testing.T) {
		c, s := grid(t)
		s.sites["acme.enterprise.slack.com"].refuses = map[string]string{"drafts.list": "team_is_restricted"}

		_, err := mustClient(t, c, orgURL).API(context.Background(), "drafts.list", nil)
		if !slackapi.IsErrorCode(err, "team_is_restricted") {
			t.Fatalf("err = %v, want team_is_restricted", err)
		}
		if got := s.took(); !reflect.DeepEqual(got, []string{"acme.enterprise.slack.com drafts.list"}) {
			t.Errorf("calls = %q, want the one refused call", got)
		}
	})
}

// Ctrl-C (or the command timeout) during the identity check is an
// interruption, and is reported as one: not as the refusal the check was
// there to work around.
func TestOrgFallbackInterruptedDuringTheIdentityCheck(t *testing.T) {
	for _, during := range []string{"acme-eng.slack.com auth.test", "acme.enterprise.slack.com auth.test", "acme.enterprise.slack.com drafts.list"} {
		t.Run(during, func(t *testing.T) {
			c, s := grid(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			s.during = func(call string) {
				if call == during {
					cancel()
				}
			}
			r, err := c.Client(ctx, "")
			if err != nil {
				t.Fatal(err)
			}
			_, err = r.Client.API(ctx, "drafts.list", nil)
			if !errors.Is(err, context.Canceled) || slackapi.IsErrorCode(err, "team_is_restricted") {
				t.Errorf("err = %v, want context.Canceled", err)
			}
			calls := s.took()
			if last := calls[len(calls)-1]; last != during {
				t.Errorf("calls = %q: something was asked after the interruption", calls)
			}
		})
	}
}
