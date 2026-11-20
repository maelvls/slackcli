package users

import (
	"context"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/maelvls/slackcli/internal/slackapi"
)

// person is an item of search.modules.people.
func person(id, handle, realName, title string) map[string]any {
	return map[string]any{"id": id, "username": handle, "profile": map[string]any{
		"real_name": realName, "display_name": handle, "title": title, "status_expiration": 0,
	}}
}

// peopleSearch serves search.modules.people over items, paged as Slack
// does, and nothing else.
func peopleSearch(items ...map[string]any) *recorder {
	return &recorder{serve: func(method string, params map[string]any) (map[string]any, error) {
		if method != "search.modules.people" {
			return nil, &slackapi.APIError{Method: method, Code: "unknown_method"}
		}
		count, page := params["count"].(int), params["page"].(int)
		start := min((page-1)*count, len(items))
		end := min(start+count, len(items))
		resp := map[string]any{"ok": true, "pagination": map[string]any{
			"page": page, "page_count": (len(items) + count - 1) / count, "per_page": count, "total_count": len(items),
		}}
		// Slack leaves "items" out when nothing matches.
		if end > start {
			list := []any{}
			for _, item := range items[start:end] {
				list = append(list, item)
			}
			resp["items"] = list
		}
		return resp, nil
	}}
}

func searchIDs(r SearchResult) []string {
	ids := []string{}
	for _, u := range r.Users {
		ids = append(ids, u.ID)
	}
	return ids
}

func TestSearchUsersAsksPeopleSearch(t *testing.T) {
	ctx := context.Background()
	rec := peopleSearch(
		person("U00000001A", "jdoe", "Jane Doe", "Engineer"),
		person("U00000002B", "john", "John Doeson", ""),
	)
	got, err := SearchUsers(ctx, rec.client(), "  doe ", 0)
	if err != nil {
		t.Fatal(err)
	}
	want := SearchResult{Total: 2, Users: []CompactUser{
		{ID: "U00000001A", Name: "jdoe", RealName: "Jane Doe", DisplayName: "jdoe", Title: "Engineer"},
		{ID: "U00000002B", Name: "john", RealName: "John Doeson", DisplayName: "john"},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("SearchUsers = %+v", got)
	}
	wantCalls := []call{{"search.modules.people", map[string]any{"module": "people", "query": "doe", "count": 20, "page": 1}}}
	if !reflect.DeepEqual(rec.calls, wantCalls) {
		t.Errorf("calls = %v", rec.calls)
	}
}

func TestSearchUsersLimit(t *testing.T) {
	ctx := context.Background()
	var people []map[string]any
	for i := range 5 {
		people = append(people, person("U0000000"+strconv.Itoa(i)+"A", "doe"+strconv.Itoa(i), "Doe", ""))
	}

	// The limit cuts the list, and Total says how many there are.
	rec := peopleSearch(people...)
	got, err := SearchUsers(ctx, rec.client(), "doe", 2)
	if err != nil || len(got.Users) != 2 || got.Total != 5 || len(rec.calls) != 1 {
		t.Errorf("SearchUsers(limit 2) = %+v, %v after %d calls", got, err, len(rec.calls))
	}

	// Slack may serve smaller pages than asked for: the next ones are read.
	rec = peopleSearch(people...)
	serve := rec.serve
	rec.serve = func(method string, params map[string]any) (map[string]any, error) {
		return serve(method, map[string]any{"count": 2, "page": params["page"]})
	}
	got, err = SearchUsers(ctx, rec.client(), "doe", 100)
	if err != nil || len(got.Users) != 5 || got.Total != 5 || len(rec.calls) != 3 {
		t.Errorf("SearchUsers(small pages) = %+v, %v after %d calls", got, err, len(rec.calls))
	}

	// Nobody: an empty list, not nil, and no scan of the directory.
	rec = peopleSearch()
	got, err = SearchUsers(ctx, rec.client(), "nobody", 0)
	if err != nil || got.Users == nil || len(got.Users) != 0 || got.Total != 0 || len(rec.calls) != 1 {
		t.Errorf("SearchUsers(nobody) = %+v, %v after %d calls", got, err, len(rec.calls))
	}
}

func TestSearchUsersEmptyQuery(t *testing.T) {
	rec := peopleSearch()
	for _, q := range []string{"", "  ", "@"} {
		if _, err := SearchUsers(context.Background(), rec.client(), q, 0); err == nil || !strings.Contains(err.Error(), "the query is empty") {
			t.Errorf("SearchUsers(%q) error = %v", q, err)
		}
	}
	if len(rec.calls) != 0 {
		t.Errorf("calls = %v", rec.calls)
	}
}

func TestSearchUsersByEmail(t *testing.T) {
	ctx := context.Background()
	rec := &recorder{serve: func(method string, params map[string]any) (map[string]any, error) {
		if method == "users.lookupByEmail" && params["email"] == "jane@example.com" {
			return map[string]any{"user": member("U00000001A", "jdoe")}, nil
		}
		if method == "users.lookupByEmail" {
			return nil, &slackapi.APIError{Method: method, Code: "users_not_found"}
		}
		return map[string]any{"ok": true}, nil
	}}
	got, err := SearchUsers(ctx, rec.client(), "jane@example.com", 0)
	if err != nil || !reflect.DeepEqual(searchIDs(got), []string{"U00000001A"}) || got.Total != 1 {
		t.Errorf("SearchUsers(email) = %+v, %v", got, err)
	}
	if !reflect.DeepEqual(rec.methods(), []string{"users.lookupByEmail"}) {
		t.Errorf("calls = %v", rec.methods())
	}

	// An address nobody has is then searched as text.
	rec.calls = nil
	got, err = SearchUsers(ctx, rec.client(), "nobody@example.com", 0)
	if err != nil || len(got.Users) != 0 {
		t.Errorf("SearchUsers(unknown email) = %+v, %v", got, err)
	}
	if !reflect.DeepEqual(rec.methods(), []string{"users.lookupByEmail", "search.modules.people"}) {
		t.Errorf("calls = %v", rec.methods())
	}
}

// searchDirectoryPages is a directory of two pages for the scan.
var searchDirectoryPages = [][]map[string]any{{
	member("U00000001A", "jdoe", "real_name", "Jane Doe", "profile", map[string]any{"title": "Engineer", "email": "jane@example.com"}),
	member("U00000002B", "doe", "real_name", "Somebody Else"),
	member("U00000003C", "gone", "real_name", "Jane Doe", "deleted", true),
	member("U00000004D", "doebot", "real_name", "Doe Bot", "is_bot", true),
}, {
	member("U00000005E", "mael", "real_name", "Maël Doe", "profile", map[string]any{"display_name": "Maël"}),
	member("U00000006F", "x", "real_name", "Xavier", "profile", map[string]any{"title": "Works with Doe"}),
	member("U00000007G", "macdoe", "real_name", "Mac Adoe"),
}}

func TestSearchUsersScansTheDirectory(t *testing.T) {
	ctx := context.Background()
	for query, want := range map[string][]string{
		// The exact handle first, then the names with a word that starts
		// with the term, then the rest; deactivated members and bots are
		// left out.
		"doe":           {"U00000002B", "U00000001A", "U00000005E", "U00000006F", "U00000007G"},
		"@DOE":          {"U00000002B", "U00000001A", "U00000005E", "U00000006F", "U00000007G"},
		"jane doe":      {"U00000001A"},
		"doe jane":      {"U00000001A"},
		"doe engineer":  {"U00000001A"},
		"mael":          {"U00000005E"},
		"maël":          {"U00000005E"},
		"example.com":   {"U00000001A"},
		"nobody at all": {},
	} {
		// A bot token cannot use the people search.
		rec := directory{pages: searchDirectoryPages}.recorder()
		bot := slackapi.New(slackapi.Auth{Type: slackapi.AuthStandard, Token: "xoxb-test"}, "https://test.slack.com",
			slackapi.WithHandler(func(_ context.Context, method string, params map[string]any) (map[string]any, error) {
				rec.calls = append(rec.calls, call{method, params})
				return rec.serve(method, params)
			}))
		got, err := SearchUsers(ctx, bot, query, 0)
		if err != nil {
			t.Fatalf("SearchUsers(%q): %v", query, err)
		}
		if !reflect.DeepEqual(searchIDs(got), want) || got.Total != len(want) {
			t.Errorf("SearchUsers(%q) = %v (total %d), want %v", query, searchIDs(got), got.Total, want)
		}
		if !reflect.DeepEqual(rec.methods(), []string{"users.list", "users.list"}) {
			t.Errorf("SearchUsers(%q): calls = %v", query, rec.methods())
		}
	}
}

// The people search is internal to Slack: where it is refused, the
// directory is scanned.
func TestSearchUsersFallsBackToTheDirectory(t *testing.T) {
	rec := directory{pages: searchDirectoryPages}.recorder()
	serve := rec.serve
	rec.serve = func(method string, params map[string]any) (map[string]any, error) {
		if method == "search.modules.people" {
			return nil, &slackapi.APIError{Method: method, Code: "not_allowed_token_type"}
		}
		return serve(method, params)
	}
	got, err := SearchUsers(context.Background(), rec.client(), "doe", 2)
	if err != nil || !reflect.DeepEqual(searchIDs(got), []string{"U00000002B", "U00000001A"}) || got.Total != 5 {
		t.Errorf("SearchUsers = %+v, %v", got, err)
	}
	if !reflect.DeepEqual(rec.methods(), []string{"search.modules.people", "users.list", "users.list"}) {
		t.Errorf("calls = %v", rec.methods())
	}
}

func TestSearchUsersFailures(t *testing.T) {
	ctx := context.Background()
	limited := func(methods ...string) *recorder {
		rec := directory{pages: searchDirectoryPages}.recorder()
		serve := rec.serve
		seen := map[string]int{}
		rec.serve = func(method string, params map[string]any) (map[string]any, error) {
			seen[method]++
			for _, m := range methods {
				name, nth, _ := strings.Cut(m, "#")
				if n, _ := strconv.Atoi(nth); name == method && seen[method] > n {
					return nil, &slackapi.RateLimitError{Method: method, RetryAfter: time.Minute}
				}
			}
			if method == "search.modules.people" {
				return nil, &slackapi.APIError{Method: method, Code: "not_allowed_token_type"}
			}
			return serve(method, params)
		}
		return rec
	}

	// A rate-limited people search is not a reason to scan the directory.
	rec := limited("search.modules.people")
	if _, err := SearchUsers(ctx, rec.client(), "doe", 0); err == nil || !strings.Contains(err.Error(), "searching users") {
		t.Errorf("rate-limited people search: error = %v", err)
	}
	if !reflect.DeepEqual(rec.methods(), []string{"search.modules.people"}) {
		t.Errorf("calls = %v", rec.methods())
	}

	// Dead credentials fail the command.
	rec = &recorder{serve: func(method string, _ map[string]any) (map[string]any, error) {
		return nil, &slackapi.APIError{Method: method, Code: "invalid_auth"}
	}}
	if _, err := SearchUsers(ctx, rec.client(), "doe", 0); err == nil || len(rec.calls) != 1 {
		t.Errorf("invalid_auth: error = %v after %d calls", err, len(rec.calls))
	}

	// A scan that cannot start fails.
	rec = limited("users.list")
	if _, err := SearchUsers(ctx, rec.client(), "doe", 0); err == nil || !strings.Contains(err.Error(), "searching the user directory") {
		t.Errorf("rate-limited scan: error = %v", err)
	}

	// A scan cut short gives what it found, without a total, and says so.
	rec = limited("users.list#1")
	got, err := SearchUsers(ctx, rec.client(), "doe", 0)
	if err != nil || !reflect.DeepEqual(searchIDs(got), []string{"U00000002B", "U00000001A"}) || got.Total != 0 ||
		len(got.Warnings) != 1 || !strings.Contains(got.Warnings[0], "stopped early") {
		t.Errorf("scan cut short = %+v, %v", got, err)
	}
}
