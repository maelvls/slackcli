package users

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/maelvls/slackcli/internal/slackapi"
)

// peopleWorkspace is a workspace for browser credentials: its people
// search finds items for the query (items given without email unless the
// test puts one in), users.info knows the members of info, users.list
// serves pages, and users.lookupByEmail is refused as Slack refuses it to
// xoxc tokens.
type peopleWorkspace struct {
	items     []map[string]any
	info      []map[string]any
	pages     [][]map[string]any
	peopleErr error
}

func (p peopleWorkspace) recorder() *recorder {
	return &recorder{serve: func(method string, params map[string]any) (map[string]any, error) {
		switch method {
		case "search.modules.people":
			if p.peopleErr != nil {
				return nil, p.peopleErr
			}
			items := []any{}
			for _, item := range p.items {
				items = append(items, item)
			}
			return map[string]any{"items": items, "pagination": map[string]any{"total_count": len(items), "page": 1, "page_count": 1}}, nil
		case "users.info":
			for _, u := range p.info {
				if u["id"] == params["user"] {
					return map[string]any{"user": u}, nil
				}
			}
			return apiError("user_not_found"), nil
		case "users.lookupByEmail":
			return apiError("not_allowed_token_type"), nil
		case "users.list":
			return directory{pages: p.pages}.recorder().serve(method, params)
		}
		return nil, fmt.Errorf("unexpected method %s", method)
	}}
}

// withEmail is a people search item that, unusually, gives an email.
func withEmail(item map[string]any, email string) map[string]any {
	item["profile"].(map[string]any)["email"] = email
	return item
}

func TestEmailWithBrowserCredentialsUsesThePeopleSearch(t *testing.T) {
	ctx := context.Background()
	alice := member("U00000001A", "alice", "tz", "Europe/Paris", "profile", profile("email", "Alice@Example.com", "real_name", "Alice A"))
	alicia := member("U00000002A", "alicia", "profile", profile("email", "alicia@example.com"))
	hidden := member("U00000003A", "hidden", "profile", profile("real_name", "Hidden"))
	bigDirectory := func(n int) [][]map[string]any {
		var pages [][]map[string]any
		for i := range n {
			pages = append(pages, []map[string]any{member(fmt.Sprintf("U%09dA", i+10), fmt.Sprintf("user%d", i))})
		}
		return pages
	}

	// What was seen on a large workspace: one match in one call, with no
	// email and no tz. users.info has them, and confirms the address.
	t.Run("one match without email, confirmed by users.info", func(t *testing.T) {
		rec := peopleWorkspace{items: []map[string]any{person("U00000001A", "alice", "Alice A", "")}, info: []map[string]any{alice}}.recorder()
		got, err := GetUser(ctx, rec.client(), "alice@example.com")
		if err != nil {
			t.Fatal(err)
		}
		if got.ID != "U00000001A" || got.Email != "Alice@Example.com" || got.TZ != "Europe/Paris" {
			t.Errorf("GetUser = %+v, want the users.info shape (email, tz)", got)
		}
		want := []call{
			{"search.modules.people", map[string]any{"module": "people", "query": "alice@example.com", "count": peopleByEmailCount, "page": 1}},
			{"users.info", map[string]any{"user": "U00000001A"}},
		}
		if !reflect.DeepEqual(rec.calls, want) {
			t.Errorf("calls = %v, want %v (no users.lookupByEmail, no users.list)", rec.calls, want)
		}
	})

	t.Run("one match that users.info shows with another email is not the user", func(t *testing.T) {
		rec := peopleWorkspace{
			items: []map[string]any{person("U00000002A", "alicia", "Alicia", "")},
			info:  []map[string]any{alicia},
			pages: [][]map[string]any{{alicia}},
		}.recorder()
		_, err := ResolveUserID(ctx, rec.client(), "alice@example.com")
		var nf *NotFoundError
		if !errors.As(err, &nf) {
			t.Errorf("err = %v, want a NotFoundError", err)
		}
		// A small directory is still looked through for a deactivated
		// member with that address.
		if want := []string{"search.modules.people", "users.info", "users.list"}; !reflect.DeepEqual(rec.methods(), want) {
			t.Errorf("methods = %v, want %v", rec.methods(), want)
		}
	})

	t.Run("several matches: the one users.info confirms is picked", func(t *testing.T) {
		rec := peopleWorkspace{
			items: []map[string]any{person("U00000002A", "alicia", "Alicia", ""), person("U00000001A", "alice", "Alice A", "")},
			info:  []map[string]any{alice, alicia},
		}.recorder()
		got, err := ResolveUserID(ctx, rec.client(), "ALICE@example.com")
		if err != nil || got != "U00000001A" {
			t.Fatalf("got %q, %v", got, err)
		}
		if n := len(rec.only("users.info")); n != 2 || len(rec.only("users.list")) != 0 {
			t.Errorf("methods = %v, want each candidate checked once and no scan", rec.methods())
		}
	})

	t.Run("several matches: an email in the search result is matched exactly", func(t *testing.T) {
		rec := peopleWorkspace{
			items: []map[string]any{
				withEmail(person("U00000002A", "alicia", "Alicia", ""), "alicia@example.com"),
				withEmail(person("U00000001A", "alice", "Alice A", ""), "alice@example.com"),
			},
			info: []map[string]any{alice, alicia},
		}.recorder()
		got, err := ResolveUserID(ctx, rec.client(), "alice@example.com")
		if err != nil || got != "U00000001A" {
			t.Fatalf("got %q, %v", got, err)
		}
		// The other one has another address: not even checked.
		if want := []call{{"users.info", map[string]any{"user": "U00000001A"}}}; !reflect.DeepEqual(rec.only("users.info"), want) {
			t.Errorf("users.info calls = %v, want %v", rec.only("users.info"), want)
		}
	})

	t.Run("several members with the address: none is picked", func(t *testing.T) {
		twin := member("U00000004A", "alice2", "profile", profile("email", "alice@example.com"))
		rec := peopleWorkspace{
			items: []map[string]any{person("U00000001A", "alice", "Alice A", ""), person("U00000004A", "alice2", "Alice B", "")},
			info:  []map[string]any{alice, twin},
		}.recorder()
		_, err := ResolveUserID(ctx, rec.client(), "alice@example.com")
		var amb *AmbiguousError
		if !errors.As(err, &amb) || len(amb.Matches) != 2 {
			t.Fatalf("err = %v, want an AmbiguousError with 2 matches", err)
		}
		for _, want := range []string{"2 members have that email address", "U00000001A (@alice)", "U00000004A (@alice2)", "Pass one of these user IDs"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error does not say %q: %v", want, err)
			}
		}
	})

	t.Run("a match whose email users.info hides is not picked", func(t *testing.T) {
		rec := peopleWorkspace{items: []map[string]any{person("U00000003A", "hidden", "Hidden", "")}, info: []map[string]any{hidden}}.recorder()
		_, err := ResolveUserID(ctx, rec.client(), "hidden@example.com")
		var amb *AmbiguousError
		if !errors.As(err, &amb) || len(amb.Matches) != 1 || !strings.Contains(err.Error(), "cannot be checked") || !strings.Contains(err.Error(), "U00000003A (@hidden)") {
			t.Errorf("err = %v", err)
		}
	})

	t.Run("too many matches are not checked one by one", func(t *testing.T) {
		var items []map[string]any
		for i := range maxPeopleByEmailChecks + 1 {
			items = append(items, person(fmt.Sprintf("U%09dA", i), fmt.Sprintf("a%d", i), "", ""))
		}
		rec := peopleWorkspace{items: items}.recorder()
		_, err := ResolveUserID(ctx, rec.client(), "a@example.com")
		var amb *AmbiguousError
		if !errors.As(err, &amb) || len(amb.Matches) != maxPeopleByEmailChecks+1 || !strings.Contains(err.Error(), "too many to check") {
			t.Errorf("err = %v", err)
		}
		if want := []string{"search.modules.people"}; !reflect.DeepEqual(rec.methods(), want) {
			t.Errorf("methods = %v", rec.methods())
		}
	})

	t.Run("no match: a deactivated member is found in a small directory", func(t *testing.T) {
		gone := member("U00000005A", "gone", "deleted", true, "profile", profile("email", "gone@example.com"))
		rec := peopleWorkspace{pages: [][]map[string]any{{alice}, {gone}, {alicia}}}.recorder()
		got, err := ResolveUserID(ctx, rec.client(), "gone@example.com")
		if err != nil || got != "U00000005A" {
			t.Fatalf("got %q, %v", got, err)
		}
		if want := []string{"search.modules.people", "users.list", "users.list"}; !reflect.DeepEqual(rec.methods(), want) {
			t.Errorf("methods = %v, want %v", rec.methods(), want)
		}
	})

	t.Run("no match: a directory too large to scan is given up on early", func(t *testing.T) {
		rec := peopleWorkspace{pages: bigDirectory(deactivatedScanPages + 3)}.recorder()
		_, err := ResolveUserID(ctx, rec.client(), "nobody@example.com")
		if err == nil {
			t.Fatal("no error")
		}
		if n := len(rec.only("users.list")); n != deactivatedScanPages {
			t.Errorf("%d users.list calls, want %d", n, deactivatedScanPages)
		}
		for _, want := range []string{`"nobody@example.com"`, "no active member", "people search", "deactivated", "more than 5000 members", "user ID (U…)"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error does not say %q: %v", want, err)
			}
		}
		var nf *NotFoundError
		if errors.As(err, &nf) {
			t.Errorf("a scan given up on is not a NotFoundError")
		}
	})

	t.Run("no match: the scan stops when the command has no time for another page", func(t *testing.T) {
		deadline := time.Now().Add(500 * time.Millisecond)
		ctx := slackapi.WithCommandDeadline(ctx, func() (time.Time, bool) { return deadline, true })
		rec := peopleWorkspace{pages: bigDirectory(3)}.recorder()
		if _, err := ResolveUserID(ctx, rec.client(), "nobody@example.com"); err == nil || !strings.Contains(err.Error(), "more than 1000 members") {
			t.Errorf("err = %v", err)
		}
		if n := len(rec.only("users.list")); n != 1 {
			t.Errorf("%d users.list calls, want 1", n)
		}
	})

	t.Run("no match: a directory whose first page is rate limited", func(t *testing.T) {
		rec := peopleWorkspace{}.recorder()
		serve := rec.serve
		rec.serve = func(method string, params map[string]any) (map[string]any, error) {
			if method == "users.list" {
				return nil, &slackapi.RateLimitError{Method: method, RetryAfter: 30 * time.Second}
			}
			return serve(method, params)
		}
		_, err := ResolveUserID(ctx, rec.client(), "nobody@example.com")
		if err == nil || !strings.Contains(err.Error(), "no active member of this workspace has that email address (Slack's people search") || strings.Contains(err.Error(), "users.lookupByEmail") {
			t.Errorf("err = %v", err)
		}
	})

	t.Run("a people search that Slack does not serve falls back to users.lookupByEmail", func(t *testing.T) {
		rec := peopleWorkspace{peopleErr: &slackapi.APIError{Method: "search.modules.people", Code: "unknown_method"}, pages: [][]map[string]any{{alice}}}.recorder()
		got, err := ResolveUserID(ctx, rec.client(), "alice@example.com")
		if err != nil || got != "U00000001A" {
			t.Fatalf("got %q, %v", got, err)
		}
		if want := []string{"search.modules.people", "users.lookupByEmail", "users.list"}; !reflect.DeepEqual(rec.methods(), want) {
			t.Errorf("methods = %v, want %v", rec.methods(), want)
		}
	})

	t.Run("a people search that Slack did not answer fails without a scan", func(t *testing.T) {
		rec := peopleWorkspace{peopleErr: &slackapi.RateLimitError{Method: "search.modules.people", RetryAfter: time.Minute}}.recorder()
		_, err := ResolveUserID(ctx, rec.client(), "alice@example.com")
		var rl *slackapi.RateLimitError
		if !errors.As(err, &rl) || !strings.Contains(err.Error(), "search.modules.people gave no answer") || !strings.Contains(err.Error(), "temporary") {
			t.Errorf("err = %v", err)
		}
		if want := []string{"search.modules.people"}; !reflect.DeepEqual(rec.methods(), want) {
			t.Errorf("methods = %v", rec.methods())
		}
	})

	t.Run("an ambiguous email does not fail the other inputs", func(t *testing.T) {
		twin := member("U00000004A", "alice2", "profile", profile("email", "alice@example.com"))
		rec := peopleWorkspace{
			items: []map[string]any{person("U00000001A", "alice", "", ""), person("U00000004A", "alice2", "", "")},
			info:  []map[string]any{alice, twin},
		}.recorder()
		got, err := ResolveUserIDs(ctx, rec.client(), []string{"alice@example.com", "U00000009A"})
		if err != nil {
			t.Fatal(err)
		}
		var amb *AmbiguousError
		if !errors.As(got[0].Err, &amb) || got[1].ID != "U00000009A" {
			t.Errorf("got %+v", got)
		}
	})

	// The tokens that may call users.lookupByEmail keep using it.
	t.Run("a user token still uses users.lookupByEmail", func(t *testing.T) {
		rec := directory{byEmail: map[string]map[string]any{"alice@example.com": alice}}.recorder()
		got, err := GetUser(ctx, rec.userTokenClient(), "alice@example.com")
		if err != nil || got.ID != "U00000001A" {
			t.Fatalf("got %+v, %v", got, err)
		}
		if want := []string{"users.lookupByEmail"}; !reflect.DeepEqual(rec.methods(), want) {
			t.Errorf("methods = %v, want %v", rec.methods(), want)
		}
	})
}
