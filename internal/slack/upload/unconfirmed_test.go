package upload

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/maelvls/slackcli/internal/slackapi"
)

// A redirect of the byte upload is never followed: the contents of the
// file must not go to a host Slack did not reserve, and a redirected POST
// that net/http turns into a GET would pass for a successful upload.
func TestBytePostRedirectIsNotFollowed(t *testing.T) {
	for _, status := range []int{301, 302, 303, 307, 308} {
		for _, where := range []string{"foreign host", "same host"} {
			t.Run(fmt.Sprintf("%d to %s", status, where), func(t *testing.T) {
				var mu sync.Mutex
				elsewhere := 0
				count := func(http.ResponseWriter, *http.Request) {
					mu.Lock()
					elsewhere++
					mu.Unlock()
				}
				second := httptest.NewTLSServer(http.HandlerFunc(count))
				defer second.Close()
				var first *httptest.Server
				first = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path != "/f" {
						count(w, r)
						return
					}
					target := second.URL + "/stolen?sig=secret-signature"
					if where == "same host" {
						target = first.URL + "/moved?sig=secret-signature"
					}
					http.Redirect(w, r, target, status)
				}))
				defer first.Close()

				// A client that would happily follow to either server.
				hc := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
				defer hc.CloseIdleConnections()
				completions := 0
				client := slackapi.NewFake(func(_ context.Context, method string, _ map[string]any) (map[string]any, error) {
					switch method {
					case "files.getUploadURLExternal":
						return map[string]any{"ok": true, "upload_url": first.URL + "/f", "file_id": "F1"}, nil
					case "files.completeUploadExternal":
						completions++
					}
					return map[string]any{"ok": true}, nil
				}, slackapi.WithHTTPClient(hc))

				_, err := ToChannel(context.Background(), client, Destination{ChannelID: "C1"}, []string{writeFile(t, "x.txt", "hi")})
				if err == nil || !strings.Contains(err.Error(), "redirect") || !strings.Contains(err.Error(), fmt.Sprintf("HTTP %d", status)) {
					t.Fatalf("err = %v, want a refused redirect", err)
				}
				if strings.Contains(err.Error(), "secret-signature") {
					t.Errorf("err = %v, gives away the query of the target", err)
				}
				mu.Lock()
				defer mu.Unlock()
				if elsewhere != 0 || completions != 0 {
					t.Errorf("%d requests followed the redirect and %d completions, want none", elsewhere, completions)
				}
				if hc.CheckRedirect != nil {
					t.Error("the redirect policy leaked into the client's HTTP client")
				}
			})
		}
	}
}

// When the answer of files.completeUploadExternal is lost, Slack may have
// shared the files all the same: the error must say so, since retrying
// posts them a second time. When Slack refuses, nothing happened.
func TestCompletionWithoutAnAnswerMayHaveGoneThrough(t *testing.T) {
	failing := func(t *testing.T, fail func() (map[string]any, error)) *slackapi.Client {
		h := newHarness(t)
		reserve := h.fixtures["files.getUploadURLExternal"]
		return slackapi.NewFake(func(_ context.Context, method string, p map[string]any) (map[string]any, error) {
			if method == "files.getUploadURLExternal" {
				return reserve(p), nil
			}
			return fail()
		}, slackapi.WithHTTPClient(h.client.HTTPClient()))
	}
	lost := func() (map[string]any, error) {
		return nil, errors.New("Slack API call files.completeUploadExternal timed out after 20s")
	}
	paths := []string{writeFile(t, "x.txt", "hi")}

	_, err := ToChannel(context.Background(), failing(t, lost), Destination{ChannelID: "C1"}, paths)
	if err == nil {
		t.Fatal("want an error")
	}
	for _, want := range []string{"finalising upload of x.txt", "may have gone through", "message list C1", "before retrying", "a second time", "timed out after 20s"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %q, does not say %q", err, want)
		}
	}

	_, err = ForDraft(context.Background(), failing(t, lost), paths)
	if err == nil || !strings.Contains(err.Error(), "may have gone through") || strings.Contains(err.Error(), "message list") {
		t.Errorf("err = %v, want a warning without a channel to check", err)
	}

	for name, refused := range map[string]func() (map[string]any, error){
		"API error": func() (map[string]any, error) { return map[string]any{"ok": false, "error": "channel_not_found"}, nil },
		"rate limit": func() (map[string]any, error) {
			return nil, &slackapi.RateLimitError{Method: "files.completeUploadExternal"}
		},
	} {
		_, err := ToChannel(context.Background(), failing(t, refused), Destination{ChannelID: "C1"}, paths)
		if err == nil || strings.Contains(err.Error(), "may have gone through") {
			t.Errorf("%s: err = %v, want a plain failure: Slack answered, nothing was posted", name, err)
		}
	}
}
