package usercmd

import (
	"bytes"
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/maelvls/slackcli/internal/slackapi"
)

// searchHarness is a harness whose workspace also answers Slack's people
// search with people (as many as asked for), and whose stderr is kept.
func searchHarness(t *testing.T, people ...map[string]any) (*harness, *bytes.Buffer) {
	t.Helper()
	h := newHarness(t)
	stderr := &bytes.Buffer{}
	h.rt.Stderr = stderr
	h.rt.ClientOptions = []slackapi.Option{slackapi.WithHandler(func(_ context.Context, method string, params map[string]any) (map[string]any, error) {
		h.mu.Lock()
		h.calls = append(h.calls, call{method, params})
		h.mu.Unlock()
		if method != "search.modules.people" {
			return serve(method, params), nil
		}
		items := []any{}
		for _, p := range people[:min(len(people), params["count"].(int))] {
			items = append(items, p)
		}
		return map[string]any{"items": items, "pagination": map[string]any{"page": 1, "page_count": 1, "total_count": len(people)}}, nil
	})}
	return h, stderr
}

func TestSearch(t *testing.T) {
	jane := map[string]any{"id": "U000000007", "username": "jdoe", "profile": map[string]any{"real_name": "Jane Doe", "title": "Engineer"}}
	john := map[string]any{"id": "U000000008", "username": "john", "profile": map[string]any{"real_name": "John Doe"}}
	h, stderr := searchHarness(t, jane, john)

	out, err := h.run("search", "doe")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"total": float64(2), "users": []any{
		map[string]any{"id": "U000000007", "name": "jdoe", "real_name": "Jane Doe", "title": "Engineer"},
		map[string]any{"id": "U000000008", "name": "john", "real_name": "John Doe"},
	}}
	if !reflect.DeepEqual(out, want) || stderr.Len() != 0 {
		t.Errorf("printed %s, stderr %s", h.stdout, stderr)
	}
	wantCalls := []call{{"search.modules.people", map[string]any{"module": "people", "query": "doe", "count": 20, "page": 1}}}
	if !reflect.DeepEqual(h.calls, wantCalls) {
		t.Errorf("calls = %+v", h.calls)
	}

	// Unquoted words make one query; --limit bounds the list, and the
	// result says that more match, in its hints: nothing goes to stderr.
	out, err = h.run("search", "jane", "doe", "--limit", "1", "--workspace", "acme")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(userIDs(out), []string{"U000000007"}) || out["total"] != float64(2) || stderr.Len() != 0 ||
		!reflect.DeepEqual(out["hints"], []any{`2 users match "jane doe", the first 1 are listed: make the query more specific, or raise --limit.`}) {
		t.Errorf("--limit 1 printed %s, stderr %s", h.stdout, stderr)
	}
	if got := h.only("search.modules.people"); len(got) != 1 || got[0].params["query"] != "jane doe" || got[0].params["count"] != 1 {
		t.Errorf("calls = %+v", got)
	}
}

// At the largest --limit, the hint does not say to raise it.
func TestSearchHintAtTheLargestLimit(t *testing.T) {
	var people []map[string]any
	for i := range 101 {
		people = append(people, map[string]any{"id": fmt.Sprintf("U%09d", i), "username": "doe"})
	}
	h, stderr := searchHarness(t, people...)
	out, err := h.run("search", "doe", "--limit", "100")
	if err != nil {
		t.Fatal(err)
	}
	want := []any{`101 users match "doe", the first 100 are listed: make the query more specific.`}
	if len(userIDs(out)) != 100 || !reflect.DeepEqual(out["hints"], want) || stderr.Len() != 0 {
		t.Errorf("printed %d users, hints %v, stderr %s", len(userIDs(out)), out["hints"], stderr)
	}
}

// "users" is printed even when nobody matches.
func TestSearchWithNoMatch(t *testing.T) {
	h, _ := searchHarness(t)
	if _, err := h.run("search", "nobody"); err != nil {
		t.Fatal(err)
	}
	if got, want := h.stdout.String(), "{\"users\":[]}\n"; got != want {
		t.Errorf("printed %s, want %s", got, want)
	}
}

// Where Slack refuses the people search, the directory is scanned.
func TestSearchScansTheDirectory(t *testing.T) {
	h := newHarness(t)
	out, err := h.run("search", "ALI")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"total": float64(1), "users": []any{map[string]any{
		"id": "U000000001", "name": "alice", "real_name": "Alice A", "display_name": "ali", "email": "alice@example.com",
		"title": "Engineer",
	}}}
	if !reflect.DeepEqual(out, want) {
		t.Errorf("printed %s", h.stdout)
	}
	if got := h.only("users.list"); len(got) != 1 {
		t.Errorf("users.list calls = %+v", got)
	}
}

func TestSearchUsageErrors(t *testing.T) {
	h, _ := searchHarness(t)
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"search"}, "arg"},
		{[]string{"search", " "}, "the query is empty"},
		{[]string{"search", "doe", "--limit", "0"}, "invalid --limit value 0: must be between 1 and 100"},
		{[]string{"search", "doe", "--limit", "101"}, "invalid --limit value 101"},
	} {
		if _, err := h.run(tc.args...); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("user %v: error = %v, want %q", tc.args, err, tc.want)
		}
		if len(h.calls) != 0 {
			t.Errorf("user %v: calls = %+v", tc.args, h.calls)
		}
	}
}
