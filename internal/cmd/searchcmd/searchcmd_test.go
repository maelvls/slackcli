package searchcmd

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/maelvls/slackcli/internal/appinfo"
	"github.com/maelvls/slackcli/internal/auth"
	"github.com/maelvls/slackcli/internal/authextract"
	"github.com/maelvls/slackcli/internal/clictx"
	"github.com/maelvls/slackcli/internal/slackapi"
	"github.com/maelvls/slackcli/internal/testenv"
)

const workspaceURL = "https://workspace.slack.com"

type call struct {
	method string
	params map[string]any
}

// harness runs the command against a fake workspace: API methods are
// served by handlers and file downloads by the files map.
type harness struct {
	t        *testing.T
	rt       *clictx.Context
	stdout   bytes.Buffer
	stderr   bytes.Buffer
	mu       sync.Mutex
	calls    []call
	handlers map[string]func(params map[string]any) (map[string]any, error)
	files    map[string]string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	for _, env := range []string{"SLACK_TOKEN", "SLACK_COOKIE_D", "SLACK_COOKIE", "SLACK_WORKSPACE_URL"} {
		t.Setenv(env, "")
	}
	// The user cache and the downloads live under the user cache directory.
	testenv.Isolate(t)

	h := &harness{t: t, handlers: map[string]func(map[string]any) (map[string]any, error){}, files: map[string]string{}}
	h.rt = &clictx.Context{
		Store:  &auth.Store{Path: filepath.Join(t.TempDir(), "credentials.json")},
		Stdout: &h.stdout,
		Stderr: &h.stderr,
		ClientOptions: []slackapi.Option{
			slackapi.WithHandler(h.handle),
			slackapi.WithHTTPClient(&http.Client{Transport: h}),
		},
	}
	h.addWorkspace(workspaceURL)
	return h
}

func (h *harness) addWorkspace(url string) {
	h.t.Helper()
	err := h.rt.Store.Upsert(auth.Workspace{URL: url, Auth: slackapi.Auth{Type: slackapi.AuthBrowser, XoxcToken: "xoxc-1", XoxdCookie: "xoxd-1"}})
	if err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) on(method string, fn func(params map[string]any) (map[string]any, error)) {
	h.handlers[method] = fn
}

func (h *harness) handle(_ context.Context, method string, params map[string]any) (map[string]any, error) {
	h.mu.Lock()
	h.calls = append(h.calls, call{method, params})
	fn := h.handlers[method]
	h.mu.Unlock()
	if fn == nil {
		h.t.Errorf("unexpected API call %s %v", method, params)
		return map[string]any{"ok": false, "error": "unexpected_call"}, nil
	}
	return fn(params)
}

func (h *harness) RoundTrip(req *http.Request) (*http.Response, error) {
	body, ok := h.files[req.URL.String()]
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/plain"}}, Body: io.NopCloser(strings.NewReader(body)), Request: req}
	if !ok {
		resp.StatusCode = http.StatusNotFound
	}
	return resp, nil
}

func (h *harness) callsTo(method string) []map[string]any {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []map[string]any
	for _, c := range h.calls {
		if c.method == method {
			out = append(out, c.params)
		}
	}
	return out
}

// run executes "search <args>" and returns what it printed.
func (h *harness) run(args ...string) (string, error) {
	h.t.Helper()
	h.stdout.Reset()
	h.stderr.Reset()
	h.mu.Lock()
	h.calls = nil
	h.mu.Unlock()
	cmd := New(h.rt)
	cmd.SetArgs(args)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SilenceUsage, cmd.SilenceErrors = true, true // as the root command does
	err := cmd.ExecuteContext(h.t.Context())
	return h.stdout.String(), err
}

func (h *harness) mustRun(args ...string) map[string]any {
	h.t.Helper()
	out, err := h.run(args...)
	if err != nil {
		h.t.Fatalf("search %v: %v", args, err)
	}
	var v map[string]any
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		h.t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	return v
}

// general makes the workspace hold one channel, #general, with one
// message, as in the TypeScript tests.
func (h *harness) general() {
	h.on("search.messages", func(map[string]any) (map[string]any, error) {
		return map[string]any{"messages": map[string]any{"matches": []any{
			map[string]any{"channel": map[string]any{"id": "C12345678", "name": "general"}},
		}}}, nil
	})
	h.on("conversations.history", func(map[string]any) (map[string]any, error) {
		return map[string]any{"messages": []any{map[string]any{
			"ts":        "1.000001",
			"text":      "hello <@U22222222>",
			"user":      "U11111111",
			"reactions": []any{map[string]any{"name": "eyes", "users": []any{"U11111111", "U44444444"}}},
		}}}, nil
	})
	h.on("users.info", func(p map[string]any) (map[string]any, error) {
		id := p["user"].(string)
		return map[string]any{"user": map[string]any{"id": id, "name": strings.ToLower(id), "profile": map[string]any{"display_name": id}}}, nil
	})
}

func TestSearchMessagesEndToEnd(t *testing.T) {
	h := newHarness(t)
	h.files["https://files.slack.com/files/F1"] = "log line"
	h.on("search.messages", func(map[string]any) (map[string]any, error) {
		return map[string]any{"messages": map[string]any{
			"matches": []any{
				map[string]any{
					"ts":        "1767268800.000100",
					"text":      "smoke tests failed",
					"username":  "ci",
					"channel":   map[string]any{"id": "C12345678", "name": "alerts"},
					"permalink": "https://workspace.slack.com/archives/C12345678/p1767268800000100?thread_ts=1767268700.000100&cid=C12345678",
				},
				map[string]any{"ts": "1767182400.000100", "channel": map[string]any{"id": "C12345678"}},
			},
			"paging": map[string]any{"pages": 7, "total": 7},
		}}, nil
	})
	h.on("conversations.history", func(map[string]any) (map[string]any, error) {
		return map[string]any{"messages": []any{}}, nil // a thread reply is not in the channel history
	})
	h.on("conversations.replies", func(p map[string]any) (map[string]any, error) {
		return map[string]any{"messages": []any{map[string]any{
			"ts":        p["latest"],
			"thread_ts": "1767268700.000100",
			"text":      "smoke tests *failed* on <#C12345678|alerts>",
			"user":      "U11111111",
			"files": []any{map[string]any{
				"id": "F1", "name": "run.log", "mimetype": "text/plain", "url_private_download": "https://files.slack.com/files/F1",
			}},
		}}}, nil
	})

	out, err := h.run("messages", "smoke tests failed", "--user", "@ci", "--after", "2025-12-01", "--limit", "1", "--max-content-chars", "18")
	if err != nil {
		t.Fatal(err)
	}

	want := map[string]any{"query": "smoke tests failed after:2025-12-01 from:@ci", "count": 1, "page": 1, "highlight": false, "sort": "timestamp", "sort_dir": "desc"}
	if calls := h.callsTo("search.messages"); len(calls) != 1 || !reflect.DeepEqual(calls[0], want) {
		t.Errorf("search.messages calls = %v, want one %v", calls, want)
	}
	// The thread named by the permalink is where the reply is looked for.
	if calls := h.callsTo("conversations.replies"); len(calls) != 1 || calls[0]["ts"] != "1767268700.000100" {
		t.Errorf("conversations.replies calls = %v", calls)
	}

	downloads := filepath.Join(h.downloadsDir(), "F1.txt")
	wantOut := `{
  "messages": [
    {
      "channel_id": "C12345678",
      "channel_name": "alerts",
      "ts": "1767268800.000100",
      "time": "` + time.Unix(1767268800, 0).Format(time.RFC3339) + `",
      "author": {
        "user_id": "U11111111",
        "name": "ci"
      },
      "content": "smoke tests *faile\n…",
      "files": [
        {
          "name": "run.log",
          "mimetype": "text/plain",
          "path": ` + jsonString(downloads) + `
        }
      ],
      "permalink": "https://workspace.slack.com/archives/C12345678/p1767268800000100?thread_ts=1767268700.000100&cid=C12345678"
    }
  ],
  "more_messages": {
    "total": 7,
    "hint": "Every result returned is dated ` + localDay(1767268800) + `, so --before cannot page further: narrow the query to see the rest. Raising --limit (now 1, max 200) returns more at once."
  }
}
`
	if wantOut := testenv.CompactJSON(t, wantOut); out != wantOut {
		t.Errorf("output:\n%s\nwant:\n%s", out, wantOut)
	}
	if h.stderr.Len() != 0 {
		t.Errorf("stderr = %q", h.stderr.String())
	}
}

// A channel scanned by name is named on its results; --resolve-users puts
// the author's display name on them.
func TestScanNamesTheChannelAndTheAuthor(t *testing.T) {
	h := newHarness(t)
	h.general()
	out := h.mustRun("messages", "hello", "--channel", "general", "--resolve-users")
	msgs, _ := out["messages"].([]any)
	if len(msgs) == 0 {
		t.Fatalf("no messages: %v", out)
	}
	for _, m := range msgs {
		m := m.(map[string]any)
		author, _ := m["author"].(map[string]any)
		users, _ := out["referenced_users"].(map[string]any)
		u, _ := users[author["user_id"].(string)].(map[string]any)
		if m["channel_name"] != "general" || m["time"] == nil || author["name"] == nil || author["name"] != u["display_name"] {
			t.Errorf("message = %v, user = %v", m, u)
		}
	}

	// Given by ID, the channel is not looked up for its name.
	out = h.mustRun("messages", "hello", "--channel", "C12345678")
	if m := out["messages"].([]any)[0].(map[string]any); m["channel_name"] != nil {
		t.Errorf("channel_name = %v for a channel given by ID", m["channel_name"])
	}
}

// An empty result says what the search did not look at.
func TestNoMatchHint(t *testing.T) {
	h := newHarness(t)
	h.on("search.messages", func(map[string]any) (map[string]any, error) {
		return map[string]any{"messages": map[string]any{"matches": []any{}}}, nil
	})
	if _, err := h.run("messages", "in:@alice"); err != nil {
		t.Fatal(err)
	}
	if want := "Nothing matched. Slack's search requires every word of the query"; !strings.Contains(h.stderr.String(), want) {
		t.Errorf("stderr = %q, want %q", h.stderr.String(), want)
	}

	h.general()
	if _, err := h.run("messages", "no such words anywhere", "--channel", "general"); err != nil {
		t.Fatal(err)
	}
	if want := "only top-level messages are looked at, not thread replies"; !strings.Contains(h.stderr.String(), want) {
		t.Errorf("--channel: stderr = %q, want %q", h.stderr.String(), want)
	}

	// Something matched: nothing to say.
	if _, err := h.run("messages", "hello", "--channel", "general"); err != nil || h.stderr.Len() != 0 {
		t.Errorf("a match: err = %v, stderr = %q", err, h.stderr.String())
	}
}

// Port of "search command forwards --refresh-users and bypasses cached
// user lookups".
func TestRefreshUsersBypassesTheUserCache(t *testing.T) {
	h := newHarness(t)
	h.general()

	out := h.mustRun("messages", "hello", "--channel", "general")
	if n := len(h.callsTo("users.info")); n != 0 {
		t.Errorf("users.info calls without --resolve-users = %d", n)
	}
	if _, ok := out["referenced_users"]; ok {
		t.Errorf("referenced_users without --resolve-users: %v", out)
	}
	if n := len(h.callsTo("conversations.info")); n != 0 {
		t.Errorf("conversations.info calls = %d, want none", n)
	}

	out = h.mustRun("messages", "hello", "--channel", "general", "--resolve-users")
	if n := len(h.callsTo("users.info")); n != 2 {
		t.Errorf("users.info calls with --resolve-users = %d, want 2", n)
	}
	want := map[string]any{
		"U11111111": map[string]any{"id": "U11111111", "name": "u11111111", "display_name": "U11111111"},
		"U22222222": map[string]any{"id": "U22222222", "name": "u22222222", "display_name": "U22222222"},
	}
	if !reflect.DeepEqual(out["referenced_users"], want) {
		t.Errorf("referenced_users = %v, want %v", out["referenced_users"], want)
	}

	h.mustRun("messages", "hello", "--channel", "general", "--resolve-users")
	if n := len(h.callsTo("users.info")); n != 0 {
		t.Errorf("users.info calls with a warm cache = %d, want 0", n)
	}

	out = h.mustRun("messages", "hello", "--channel", "general", "--refresh-users")
	if n := len(h.callsTo("users.info")); n != 2 {
		t.Errorf("users.info calls with --refresh-users = %d, want 2", n)
	}
	if !reflect.DeepEqual(out["referenced_users"], want) {
		t.Errorf("--refresh-users implies --resolve-users: got %v", out["referenced_users"])
	}
}

func TestChannelFlagRepeatsAndSplitsOnCommas(t *testing.T) {
	h := newHarness(t)
	h.general()
	h.on("files.list", func(map[string]any) (map[string]any, error) {
		return map[string]any{"files": []any{map[string]any{"id": "F1", "title": "hello.txt", "permalink": "https://workspace.slack.com/files/U1/F1"}}}, nil
	})

	out := h.mustRun("all", "hello", "--channel", "C12345678,C22222222", "--channel", "C33333333", "--no-download")
	var scanned []string
	for _, c := range h.callsTo("conversations.history") {
		scanned = append(scanned, c["channel"].(string))
	}
	if len(scanned) != 3 || len(h.callsTo("files.list")) != 3 { // one page each: a short page is the last
		t.Errorf("channels scanned = %v, files.list calls = %d", scanned, len(h.callsTo("files.list")))
	}
	if msgs := out["messages"].([]any); len(msgs) != 3 {
		t.Errorf("messages = %v", msgs)
	}
	wantFiles := []any{map[string]any{"title": "hello.txt", "permalink": "https://workspace.slack.com/files/U1/F1"}}
	if !reflect.DeepEqual(out["files"], wantFiles) {
		t.Errorf("files = %v, want %v (one file, no path with --no-download)", out["files"], wantFiles)
	}
}

func TestFailedDownloadIsReportedOnStdoutAndStderr(t *testing.T) {
	h := newHarness(t)
	h.on("search.files", func(map[string]any) (map[string]any, error) {
		return map[string]any{"files": map[string]any{"matches": []any{
			map[string]any{"id": "F1", "title": "Report", "mimetype": "application/pdf", "filetype": "pdf", "url_private": "https://files.slack.com/files/F1"},
		}}}, nil
	})
	out := h.mustRun("files", "report")
	want := []any{map[string]any{
		"title":    "Report",
		"mimetype": "application/pdf",
		"path":     filepath.Join(h.downloadsDir(), "F1.download-error.txt"),
		"error":    "Failed to download file (404)",
	}}
	if !reflect.DeepEqual(out["files"], want) {
		t.Errorf("files = %v, want %v", out["files"], want)
	}
	if got := h.stderr.String(); got != "Warning: file F1 could not be downloaded: Failed to download file (404)\n" {
		t.Errorf("stderr = %q", got)
	}
}

func TestInvalidFlagsFailBeforeAnyCall(t *testing.T) {
	h := newHarness(t)
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"messages", "x", "--limit", "abc"}, `invalid argument "abc" for "--limit"`},
		{[]string{"messages", "x", "--limit", "0"}, "invalid --limit value 0: must be between 1 and 200"},
		{[]string{"messages", "x", "--limit", "-3"}, "invalid --limit value -3"},
		{[]string{"files", "x", "--limit", "201"}, "invalid --limit value 201"},
		{[]string{"messages", "x", "--limit", "20.5"}, `invalid argument "20.5" for "--limit"`},
		{[]string{"messages", "x", "--max-content-chars", "lots"}, `invalid argument "lots" for "--max-content-chars"`},
		{[]string{"messages", "x", "--max-content-chars", "0"}, "invalid --max-content-chars value 0"},
		{[]string{"messages", "x", "--max-body-chars", "0"}, "invalid --max-body-chars value 0"},
		{[]string{"all", "x", "--content-type", "video"}, `--content-type: invalid content type "video": expected any, text, image, snippet or file`},
		{[]string{"all", "x", "--after", "yesterday"}, `invalid --after date "yesterday": expected a calendar day as YYYY-MM-DD`},
		{[]string{"all", "x", "--before", "2026-02-30"}, `invalid --before date "2026-02-30"`},
		{[]string{"all", "x", "--after", "2026-01-05", "--before", "2026-01-05"}, "no day in between"},
		{[]string{"all", ""}, "the search query is empty"},
		{[]string{"all"}, "accepts 1 arg(s), received 0"},
		{[]string{"all", "x", "y"}, "accepts 1 arg(s), received 2"},
		{[]string{"messages", "has:link x", "--channel", "C12345678"}, `the search operator "has:link" cannot be combined with --channel`},
	} {
		_, err := h.run(tc.args...)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("search %v: err = %v, want %q", tc.args, err, tc.want)
		}
		if len(h.calls) != 0 {
			t.Errorf("search %v: Slack was called: %v", tc.args, h.calls)
		}
		if h.stdout.Len() != 0 {
			t.Errorf("search %v: printed %q", tc.args, h.stdout.String())
		}
	}
}

func TestBotTokenGetsAnActionableError(t *testing.T) {
	h := newHarness(t)
	t.Setenv("SLACK_TOKEN", "xoxb-bot")
	_, err := h.run("messages", "hello")
	if err == nil || !strings.Contains(err.Error(), "bot token (xoxb-)") || !strings.Contains(err.Error(), "--channel") {
		t.Fatalf("err = %v", err)
	}
	if len(h.calls) != 0 {
		t.Errorf("Slack was called: %v", h.calls)
	}
}

func TestWorkspaceFlagSelectsTheWorkspace(t *testing.T) {
	h := newHarness(t)
	h.general()
	h.addWorkspace("https://other.slack.com")

	for args, want := range map[string]string{
		"":                  "https://workspace.slack.com/archives/C12345678/p1000001", // the default workspace
		"--workspace=other": "https://other.slack.com/archives/C12345678/p1000001",
	} {
		argv := []string{"messages", "hello", "--channel", "#general"}
		if args != "" {
			argv = append(argv, args)
		}
		out := h.mustRun(argv...)
		if got := out["messages"].([]any)[0].(map[string]any)["permalink"]; got != want {
			t.Errorf("search %v: permalink = %v, want %v", argv, got, want)
		}
	}

	t.Setenv("SLACK_WORKSPACE_URL", "https://other.slack.com")
	out := h.mustRun("messages", "hello", "--channel", "#general")
	if got := out["messages"].([]any)[0].(map[string]any)["permalink"]; !strings.HasPrefix(got.(string), "https://other.slack.com/") {
		t.Errorf("SLACK_WORKSPACE_URL: permalink = %v", got)
	}
	if _, err := h.run("messages", "hello", "--workspace", "nope"); err == nil {
		t.Error("an unknown workspace must fail")
	}
}

func TestExpiredCredentialsAreRefreshedOnce(t *testing.T) {
	h := newHarness(t)
	h.general()
	attempts := 0
	h.on("conversations.history", func(map[string]any) (map[string]any, error) {
		attempts++
		if attempts == 1 {
			return map[string]any{"ok": false, "error": "invalid_auth"}, nil
		}
		return map[string]any{"messages": []any{map[string]any{"ts": "1.000001", "text": "hello", "user": "U11111111"}}}, nil
	})
	h.rt.Desktop = func(context.Context) (*authextract.Extracted, error) {
		return &authextract.Extracted{CookieD: "xoxd-2", Teams: []authextract.Team{{URL: workspaceURL, Name: "workspace", Token: "xoxc-2"}}}, nil
	}

	out := h.mustRun("messages", "hello", "--channel", "C12345678")
	if attempts != 2 || len(out["messages"].([]any)) != 1 {
		t.Errorf("attempts = %d, output = %v", attempts, out)
	}
}

// The search parts of the agent-facing help contracts: --help is the
// documentation agents read.
func TestHelpContracts(t *testing.T) {
	root := New(&clictx.Context{})
	if got := root.Short; got != "Search Slack messages and files (token-efficient JSON)" {
		t.Errorf("search description = %q", got)
	}
	for _, want := range []string{"newest first", "bot tokens (xoxb-) cannot", "top-level messages", "more_messages", "--before"} {
		if !strings.Contains(root.Long, want) {
			t.Errorf("search help lacks %q", want)
		}
	}

	var names []string
	for _, sub := range root.Commands() {
		names = append(names, sub.Name())
		if sub.Use != sub.Name()+" <query>" {
			t.Errorf("%s: use = %q", sub.Name(), sub.Use)
		}
		for flag, wants := range map[string][]string{
			"workspace":      {"required if you have multiple workspaces"},
			"channel":        {"#name, name or ID", "repeat the flag", "commas", "scanned", "search operators are then rejected"},
			"user":           {"U…/W…", "@handle", "email", "display name"},
			"after":          {"YYYY-MM-DD", "Exclusive", "starts on 2026-01-02", "time zone"},
			"before":         {"YYYY-MM-DD", "Exclusive", "ends on 2026-01-31"},
			"content-type":   {"any", "text", "image", "snippet", "file"},
			"limit":          {"1 to 200", "more_messages / more_files"},
			"max-body-chars": {"-1 for unlimited"},
			"no-download":    {"without a local path"},
			"resolve-users":  {"referenced_users"},
			"refresh-users":  {"implies --resolve-users"},
		} {
			f := sub.Flags().Lookup(flag)
			if f == nil {
				t.Errorf("%s: no --%s flag", sub.Name(), flag)
				continue
			}
			for _, want := range wants {
				if !strings.Contains(f.Usage, want) {
					t.Errorf("%s --%s: help %q lacks %q", sub.Name(), flag, f.Usage, want)
				}
			}
		}
		for flag, def := range map[string]string{"limit": "20", "max-body-chars": "8000", "max-content-chars": "8000", "content-type": "any"} {
			if got := sub.Flags().Lookup(flag).DefValue; got != def {
				t.Errorf("%s --%s default = %q, want %q", sub.Name(), flag, got, def)
			}
		}
		// --max-content-chars, the flag's former name, is still accepted
		// but no longer advertised.
		if f := sub.Flags().Lookup("max-content-chars"); f == nil || !f.Hidden || strings.Contains(sub.UsageString(), "max-content-chars") {
			t.Errorf("%s: --max-content-chars is not a hidden alias", sub.Name())
		}
	}
	if want := []string{"all", "files", "messages"}; !reflect.DeepEqual(names, want) {
		t.Errorf("subcommands = %v, want %v", names, want)
	}
}

func (h *harness) downloadsDir() string { return appinfo.DownloadsDir() }

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// localDay is the day of a Unix time in the local time zone, as hints
// print it.
func localDay(sec int64) string { return time.Unix(sec, 0).Format("2006-01-02") }

// The cap on message content has the same name as on "message get/list",
// --max-body-chars; its former name is an alias.
func TestMaxBodyCharsAndItsAlias(t *testing.T) {
	for _, flag := range []string{"--max-body-chars", "--max-content-chars"} {
		h := newHarness(t)
		h.general()
		h.on("conversations.history", func(map[string]any) (map[string]any, error) {
			return map[string]any{"messages": []any{map[string]any{"ts": "1767225600.000100", "user": "U1", "text": "hello world"}}}, nil
		})
		out := h.mustRun("messages", "hello", "--channel", "C12345678", flag, "5")
		msgs, _ := out["messages"].([]any)
		if len(msgs) != 1 || msgs[0].(map[string]any)["content"] != "hello\n…" {
			t.Errorf("%s 5: messages = %v", flag, out["messages"])
		}
	}
}
