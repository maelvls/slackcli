package messagecmd

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// history makes conversations.history answer with msgs, whatever is asked.
func (h *harness) history(msgs ...map[string]any) {
	h.fixtures["conversations.history"] = func(map[string]any) (map[string]any, error) {
		out := make([]any, len(msgs))
		for i, m := range msgs {
			out[i] = m
		}
		return map[string]any{"ok": true, "messages": out}, nil
	}
}

// replies makes conversations.replies answer with msgs.
func (h *harness) replies(msgs ...map[string]any) {
	h.fixtures["conversations.replies"] = func(map[string]any) (map[string]any, error) {
		out := make([]any, len(msgs))
		for i, m := range msgs {
			out[i] = m
		}
		return map[string]any{"ok": true, "messages": out}, nil
	}
}

func TestGetByURL(t *testing.T) {
	h := newHarness(t)
	h.history(map[string]any{"ts": testTS, "user": "U05BRPTKL6A", "text": "hello *world*"})
	got := h.mustRun("get", testURL)
	wantJSON(t, got, `{"message": {
		"channel_id": "C12345678",
		"ts": "1770165109.628379",
		"time": "2026-02-04T00:31:49Z",
		"author": {"user_id": "U05BRPTKL6A"},
		"content": "hello *world*"
	}}`)
	h.wantCalls(call{"conversations.history", map[string]any{"channel": testChannel, "latest": testTS, "inclusive": true, "limit": 1}})
}

func TestDefaultSubcommandIsGet(t *testing.T) {
	h := newHarness(t)
	h.history(map[string]any{"ts": testTS, "user": "U05BRPTKL6A", "text": "0123456789"})

	want := h.mustRun("get", testURL, "--max-body-chars", "4")
	for _, args := range [][]string{
		{testURL, "--max-body-chars", "4"},
		{"--max-body-chars", "4", testURL},
		{testChannel, "--ts", testTS, "--max-body-chars=4"},
		{"general", "--ts", testTS, "--max-body-chars", "4"},
	} {
		if got := h.mustRun(args...); !reflect.DeepEqual(got, want) {
			t.Errorf("message %v = %v, want the output of get: %v", args, got, want)
		}
	}
	content, _ := want["message"].(map[string]any)["content"].(string)
	if !strings.HasPrefix(content, "0123") || strings.Contains(content, "456789") {
		t.Errorf("content = %q, want it cut after 4 characters", content)
	}
}

func TestUnknownSubcommand(t *testing.T) {
	tests := []struct {
		args []string
		want string
	}{
		{[]string{"gett", testURL}, `unknown command "gett" for "message"`},
		{[]string{"sned", testChannel, "hello"}, `did you mean send?`},
		{[]string{"lisst"}, `did you mean list?`},
		{[]string{"sennd"}, `did you mean send?`},
		{[]string{"frobnicate", "x", "y"}, `unknown command "frobnicate"`},
		{[]string{testURL, "extra"}, "accepts exactly one <target>"},
		// A single unknown word is a channel name, which needs --ts.
		{[]string{"announcements"}, "--ts is required"},
		{[]string{"react", "ad", testURL, "eyes"}, `unknown command "ad" for "message react"`},
		{[]string{"scheduled", "cancle", "Q1"}, `unknown command "cancle" for "message scheduled"`},
	}
	for _, tt := range tests {
		h := newHarness(t)
		h.mustFail(tt.want, tt.args...)
		h.wantCalls()
	}
}

func TestMessageWithoutArgsPrintsHelp(t *testing.T) {
	h := newHarness(t)
	if out, err := h.run(); err != nil || out != "" {
		t.Errorf("out = %q, err = %v", out, err)
	}
	h.wantCalls()
}

func TestGetValidation(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"channel without ts", []string{"get", testChannel}, `--ts is required when the target is a channel: pass --ts "<seconds>.<micros>"`},
		{"blank ts", []string{"get", testChannel, "--ts", "   "}, "--ts is required"},
		{"garbage ts", []string{"get", testChannel, "--ts", "abc"}, "invalid --ts"},
		{"truncated ts", []string{"get", testChannel, "--ts", "1770165109"}, "invalid --ts"},
		{"ts contradicting URL", []string{"get", testURL, "--ts", "1770160000.000001"}, "contradicts the message URL"},
		{"garbage thread ts", []string{"get", testURL, "--thread-ts", "x"}, "invalid --thread-ts"},
		{"user target without ts", []string{"get", "U05BRPTKL6A"}, `--ts is required when the target is a user ID: pass --ts "<seconds>.<micros>"`},
		{"user handle", []string{"get", "@alice", "--ts", testTS}, `"@alice" is a user handle, not a target`},
		{"non-numeric max-body-chars", []string{"get", testURL, "--max-body-chars", "abc"}, `invalid argument "abc" for "--max-body-chars"`},
		{"fractional max-body-chars", []string{"get", testURL, "--max-body-chars", "3.5"}, "--max-body-chars"},
		{"zero max-body-chars", []string{"get", testURL, "--max-body-chars", "0"}, "must be a positive integer, or -1 for unlimited"},
		{"negative max-body-chars", []string{"get", testURL, "--max-body-chars", "-2"}, "must be a positive integer, or -1 for unlimited"},
		{"non-Slack URL", []string{"get", "https://example.com/archives/C12345678/p1770165109628379"}, "unsupported target URL"},
		{"no target", []string{"get"}, "accepts 1 arg"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			h.mustFail(tt.want, tt.args...)
			h.wantCalls()
		})
	}
}

func TestMessageTS(t *testing.T) {
	h := newHarness(t)
	h.history(map[string]any{"ts": testTS, "text": "x"})
	// Surrounding blanks are trimmed; a --ts equal to the URL's is fine.
	h.mustRun("get", testChannel, "--ts", " "+testTS+" ")
	if ts := h.callsTo("conversations.history")[0].params["latest"]; ts != testTS {
		t.Errorf("latest = %q", ts)
	}
	h.mustRun("get", testURL, "--ts", testTS)
}

func TestGetUnlimitedBody(t *testing.T) {
	h := newHarness(t)
	long := strings.Repeat("x", 9000)
	h.history(map[string]any{"ts": testTS, "text": long})

	content := func(args ...string) string {
		s, _ := h.mustRun(args...)["message"].(map[string]any)["content"].(string)
		return s
	}
	if got := content("get", testURL); len(got) >= 9000 || !strings.HasPrefix(got, strings.Repeat("x", 8000)) {
		t.Errorf("default: content has %d characters, want it cut at 8000", len(got))
	}
	if got := content("get", testURL, "--max-body-chars", "-1"); got != long {
		t.Errorf("-1: content has %d characters, want all 9000", len(got))
	}
}

func TestGetMessageNotFound(t *testing.T) {
	h := newHarness(t)
	h.mustFail("no message with ts "+testTS, "get", testURL)
}

func TestGetThreadSummary(t *testing.T) {
	root := map[string]any{"ts": "1770160000.000001", "thread_ts": "1770160000.000001", "reply_count": 3, "text": "root"}
	reply := map[string]any{"ts": "1770160500.000100", "thread_ts": "1770160000.000001", "text": "reply"}

	t.Run("root: no extra call", func(t *testing.T) {
		h := newHarness(t)
		h.history(root)
		got := h.mustRun("get", testChannel, "--ts", "1770160000.000001")
		wantJSON(t, map[string]any{"thread": got["thread"]}, `{"thread": {"ts": "1770160000.000001", "length": 4}}`)
		if got := h.methods(); !reflect.DeepEqual(got, []string{"conversations.history"}) {
			t.Errorf("calls = %v", got)
		}
	})

	t.Run("reply: length comes from the root", func(t *testing.T) {
		h := newHarness(t)
		h.fixtures["conversations.replies"] = func(p map[string]any) (map[string]any, error) {
			if p["ts"] == "1770160000.000001" && p["limit"] == 1 {
				return map[string]any{"ok": true, "messages": []any{root}}, nil
			}
			return map[string]any{"ok": true, "messages": []any{root, reply}}, nil
		}
		// --thread-ts tells where to look for the reply (the TypeScript
		// ignored it for URL targets).
		got := h.mustRun("get", testWorkspace+"/archives/C12345678/p1770160500000100", "--thread-ts", "1770160000.000001")
		wantJSON(t, map[string]any{"thread": got["thread"]}, `{"thread": {"ts": "1770160000.000001", "length": 4}}`)
		if got["message"].(map[string]any)["ts"] != "1770160500.000100" {
			t.Errorf("message = %v", got["message"])
		}
		first := h.callsTo("conversations.replies")[0].params
		if first["ts"] != "1770160000.000001" || first["oldest"] != "1770160500.000100" {
			t.Errorf("first conversations.replies call = %#v, want the hinted thread", first)
		}
	})

	t.Run("no thread", func(t *testing.T) {
		h := newHarness(t)
		h.history(map[string]any{"ts": testTS, "text": "alone"})
		if got := h.mustRun("get", testURL); got["thread"] != nil {
			t.Errorf("thread = %v", got["thread"])
		}
	})
}

func TestGetIncludeReactionsAndResolveUsers(t *testing.T) {
	h := newHarness(t)
	h.history(map[string]any{
		"ts": testTS, "user": "U05BRPTKL6A", "text": "ping <@U0AAAAAAAAA>",
		"reactions": []any{map[string]any{"name": "eyes", "users": []any{"U0BBBBBBBBB"}, "count": 1}},
	})
	h.fixtures["users.info"] = func(p map[string]any) (map[string]any, error) {
		id := p["user"].(string)
		return map[string]any{"ok": true, "user": map[string]any{"id": id, "name": "name-" + id}}, nil
	}

	got := h.mustRun("get", testURL)
	if got["referenced_users"] != nil || got["message"].(map[string]any)["reactions"] != nil {
		t.Errorf("without flags: %v", got)
	}
	if len(h.callsTo("users.info")) != 0 {
		t.Error("users.info called without --resolve-users")
	}

	got = h.mustRun("get", testURL, "--include-reactions", "--resolve-users")
	users, _ := got["referenced_users"].(map[string]any)
	for _, id := range []string{"U05BRPTKL6A", "U0AAAAAAAAA", "U0BBBBBBBBB"} {
		if u, _ := users[id].(map[string]any); u["name"] != "name-"+id {
			t.Errorf("referenced_users[%s] = %v", id, users[id])
		}
	}
	reactions, _ := got["message"].(map[string]any)["reactions"].([]any)
	if len(reactions) != 1 || reactions[0].(map[string]any)["name"] != "eyes" {
		t.Errorf("reactions = %v", reactions)
	}
	if h.callsTo("conversations.history")[0].params["include_all_metadata"] != true {
		t.Error("include_all_metadata not requested")
	}

	// Cached now; --refresh-users implies --resolve-users and refetches.
	h.mustRun("get", testURL, "--resolve-users")
	if n := len(h.callsTo("users.info")); n != 0 {
		t.Errorf("%d users.info calls with a warm cache", n)
	}
	got = h.mustRun("get", testURL, "--refresh-users")
	if n := len(h.callsTo("users.info")); n != 2 {
		t.Errorf("%d users.info calls with --refresh-users, want 2", n)
	}
	if got["referenced_users"] == nil {
		t.Error("--refresh-users did not print referenced_users")
	}
}

// A message URL with thread_ts and no cid is complete: no warning.
func TestGetURLWithoutCID(t *testing.T) {
	h := newHarness(t)
	h.history(map[string]any{"ts": testTS, "text": "x"})
	for _, url := range []string{testURL + "?thread_ts=1770160000.000001", testURL} {
		h.mustRun("get", url)
		if h.stderr.Len() != 0 {
			t.Errorf("%s: stderr = %q", url, h.stderr.String())
		}
	}
}

func TestGetDownloadsFiles(t *testing.T) {
	h := newHarness(t)
	h.history(map[string]any{"ts": testTS, "text": "see attached", "files": []any{
		map[string]any{"id": "F1", "name": "notes.txt", "mimetype": "text/plain", "url_private_download": "https://files.slack.com/files-pri/T1-F1/download/notes.txt"},
		map[string]any{"id": "F2", "name": "gone.png", "mimetype": "image/png", "url_private_download": "https://files.slack.com/files-pri/T1-F2/download/gone.png"},
	}})
	h.serve = func(req *http.Request) *http.Response {
		if strings.Contains(req.URL.Path, "F2") {
			return httpResponse(http.StatusNotFound, "", "")
		}
		return httpResponse(http.StatusOK, "text/plain", "my notes")
	}

	// One failed download does not hide the message or the other file.
	got := h.mustRun("get", testURL)
	wantJSON(t, got, fmt.Sprintf(`{"message": {
		"channel_id": "C12345678",
		"ts": "1770165109.628379",
		"time": "2026-02-04T00:31:49Z",
		"content": "see attached",
		"files": [
			{"name": "notes.txt", "mimetype": "text/plain", "path": %q},
			{"name": "gone.png", "mimetype": "image/png", "path": %q, "error": "Failed to download file (404)"}
		]
	}}`, filepath.Join(h.c.downloadsDir, "F1.txt"), filepath.Join(h.c.downloadsDir, "F2.download-error.txt")))
	if b, _ := os.ReadFile(filepath.Join(h.c.downloadsDir, "F1.txt")); string(b) != "my notes" {
		t.Errorf("downloaded content = %q", b)
	}
	if !strings.Contains(h.stderr.String(), "Warning: could not download file F2: Failed to download file (404)") {
		t.Errorf("stderr = %q", h.stderr.String())
	}
}

// --no-download, as on "search": the files are listed without a path and
// nothing is fetched, on "get" (also the implicit one) and on "list".
func TestNoDownload(t *testing.T) {
	file := map[string]any{"id": "F1", "name": "notes.txt", "mimetype": "text/plain", "url_private_download": "https://files.slack.com/files-pri/T1-F1/download/notes.txt"}
	for _, args := range [][]string{
		{"get", testURL, "--no-download"},
		{testURL, "--no-download"},
		{"list", testChannel, "--no-download"},
		{"list", testURL, "--no-download"},
	} {
		h := newHarness(t)
		h.history(map[string]any{"ts": testTS, "text": "see attached", "files": []any{file}})
		h.replies(map[string]any{"ts": testTS, "text": "see attached", "files": []any{file}})
		h.serve = func(req *http.Request) *http.Response {
			t.Errorf("%v: downloaded %s", args, req.URL)
			return httpResponse(http.StatusNotFound, "", "")
		}
		got := h.mustRun(args...)
		msg, _ := got["message"].(map[string]any)
		if list, ok := got["messages"].([]any); ok && len(list) == 1 {
			msg, _ = list[0].(map[string]any)
		}
		wantJSON(t, map[string]any{"files": msg["files"]}, `{"files": [{"name": "notes.txt", "mimetype": "text/plain"}]}`)
		if entries, _ := os.ReadDir(h.c.downloadsDir); len(entries) != 0 {
			t.Errorf("%v: %d files written to the downloads directory", args, len(entries))
		}
		if h.stderr.Len() != 0 {
			t.Errorf("%v: stderr = %q", args, h.stderr.String())
		}
	}
}

// --max-content-chars, the name of the flag on "search", is --max-body-chars.
func TestMaxContentCharsAlias(t *testing.T) {
	for _, args := range [][]string{{"get", testURL}, {"list", testChannel}} {
		h := newHarness(t)
		h.history(map[string]any{"ts": testTS, "text": "hello world"})
		want := h.mustRun(append(args, "--max-body-chars", "5")...)
		got := h.mustRun(append(args, "--max-content-chars", "5")...)
		if !reflect.DeepEqual(got, want) || !strings.Contains(h.stdout.String(), `hello\n…`) {
			t.Errorf("%v: --max-content-chars 5 printed %s, want %v", args, h.stdout.String(), want)
		}
		h.mustFail("must be a positive integer, or -1 for unlimited", append(args, "--max-content-chars", "0")...)
	}
}

func threadFixture(h *harness) {
	h.replies(
		map[string]any{"ts": "1770160000.000001", "thread_ts": "1770160000.000001", "reply_count": 2, "user": "U05BRPTKL6A", "text": "root"},
		map[string]any{"ts": "1770160500.000100", "thread_ts": "1770160000.000001", "user": "U0AAAAAAAAA", "text": "first"},
		map[string]any{"ts": "1770160600.000200", "thread_ts": "1770160000.000001", "user": "U05BRPTKL6A", "text": "second"},
	)
}

// A thread is printed in the shape of channel history: channel_id at the
// top, and on every message its channel_id and thread_ts; the root says
// how many replies it has.
const wantThread = `{"channel_id": "C12345678", "messages": [
	{"channel_id": "C12345678", "ts": "1770160000.000001", "time": "2026-02-03T23:06:40Z", "thread_ts": "1770160000.000001", "reply_count": 2, "author": {"user_id": "U05BRPTKL6A"}, "content": "root"},
	{"channel_id": "C12345678", "ts": "1770160500.000100", "time": "2026-02-03T23:15:00Z", "thread_ts": "1770160000.000001", "author": {"user_id": "U0AAAAAAAAA"}, "content": "first"},
	{"channel_id": "C12345678", "ts": "1770160600.000200", "time": "2026-02-03T23:16:40Z", "thread_ts": "1770160000.000001", "author": {"user_id": "U05BRPTKL6A"}, "content": "second"}
]}`

func TestListThread(t *testing.T) {
	tests := []struct {
		name   string
		args   []string
		wantTS string
	}{
		{"message URL", []string{testWorkspace + "/archives/C12345678/p1770160000000001"}, "1770160000.000001"},
		{"reply URL with thread_ts", []string{testWorkspace + "/archives/C12345678/p1770160500000100?thread_ts=1770160000.000001&cid=C12345678"}, "1770160000.000001"},
		{"channel and --thread-ts", []string{testChannel, "--thread-ts", "1770160000.000001"}, "1770160000.000001"},
		{"channel and --ts", []string{testChannel, "--ts", "1770160500.000100"}, "1770160500.000100"},
		{"channel name and --ts", []string{"#general", "--ts", "1770160000.000001"}, "1770160000.000001"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			threadFixture(h)
			wantJSON(t, h.mustRun(append([]string{"list"}, tt.args...)...), wantThread)
			calls := h.callsTo("conversations.replies")
			// Asked about a reply, FetchThread asks again about the root
			// that the answer names.
			wantCalls := 1
			if tt.wantTS != "1770160000.000001" {
				wantCalls = 2
			}
			if len(calls) != wantCalls || calls[0].params["ts"] != tt.wantTS || calls[0].params["channel"] != testChannel ||
				calls[len(calls)-1].params["ts"] != "1770160000.000001" {
				t.Errorf("conversations.replies calls = %v", calls)
			}
			if len(h.callsTo("conversations.history")) != 0 {
				t.Error("the message was fetched before its thread: FetchThread alone is enough")
			}
		})
	}
}

func TestListThreadNotFound(t *testing.T) {
	h := newHarness(t)
	h.fixtures["conversations.replies"] = func(map[string]any) (map[string]any, error) {
		return nil, apiError("conversations.replies", "thread_not_found")
	}
	h.mustFail("no message with ts "+testTS+" in channel "+testChannel, "list", testURL)
}

func TestListThreadRejectsHistoryFlags(t *testing.T) {
	for _, flag := range [][]string{
		{"--limit", "5"}, {"--oldest", "1770160000.000001"}, {"--latest", "1770160000.000001"},
		{"--with-reaction", "eyes"}, {"--without-reaction", "eyes"},
	} {
		for _, target := range [][]string{{testURL}, {testChannel, "--ts", testTS}, {testChannel, "--thread-ts", testTS}} {
			h := newHarness(t)
			args := append(append([]string{"list"}, target...), flag...)
			h.mustFail(flag[0]+" only applies to channel history", args...)
			h.wantCalls()
		}
	}
}

func TestListHistory(t *testing.T) {
	h := newHarness(t)
	// Slack answers newest first.
	h.history(
		map[string]any{"ts": "1770160600.000200", "user": "U05BRPTKL6A", "text": "newer"},
		map[string]any{"ts": "1770160000.000001", "thread_ts": "1770160000.000001", "reply_count": 2, "user": "U0AAAAAAAAA", "text": "older"},
	)
	got := h.mustRun("list", "general", "--limit", "50", "--oldest", " 1770100000 ", "--latest", "1770170000.000000")
	wantJSON(t, got, `{
		"channel_id": "C12345678",
		"messages": [
			{"channel_id": "C12345678", "ts": "1770160000.000001", "time": "2026-02-03T23:06:40Z", "thread_ts": "1770160000.000001", "reply_count": 2, "author": {"user_id": "U0AAAAAAAAA"}, "content": "older"},
			{"channel_id": "C12345678", "ts": "1770160600.000200", "time": "2026-02-03T23:16:40Z", "author": {"user_id": "U05BRPTKL6A"}, "content": "newer"}
		]
	}`)
	want := map[string]any{"channel": testChannel, "limit": 50, "oldest": "1770100000", "latest": "1770170000.000000"}
	if got := h.callsTo("conversations.history")[0].params; !reflect.DeepEqual(got, want) {
		t.Errorf("conversations.history params = %#v\nwant %#v", got, want)
	}
}

// --oldest and --latest take a day or a date and time, in the local time
// zone (UTC in these tests), as well as a ts.
func TestListHistoryDateBounds(t *testing.T) {
	for _, tc := range []struct {
		oldest, latest         string
		wantOldest, wantLatest string
	}{
		{"2026-02-03", "2026-02-04", "1770076800", "1770163200"},
		{"2026-02-04T12:30", "2026-02-04T12:30:15", "1770208200", "1770208215"},
		{"2026-02-04 12:30", "2026-02-04 12:30:15", "1770208200", "1770208215"},
		{"2026-02-04T01:00:00+01:00", "2026-02-04T00:31:49Z", "1770163200", "1770165109"},
		{"1770100000", "1770170000.000000", "1770100000", "1770170000.000000"},
	} {
		h := newHarness(t)
		h.history(map[string]any{"ts": "1770165109.628379", "user": "U05BRPTKL6A", "text": "hello"})
		h.mustRun("list", testChannel, "--oldest", tc.oldest, "--latest", tc.latest)
		if p := h.callsTo("conversations.history")[0].params; p["oldest"] != tc.wantOldest || p["latest"] != tc.wantLatest {
			t.Errorf("--oldest %s --latest %s: conversations.history params = %v", tc.oldest, tc.latest, p)
		}
	}

	h := newHarness(t)
	h.mustFail("--oldest 1770163200 is not before --latest 1770076800", "list", testChannel, "--oldest", "2026-02-04", "--latest", "2026-02-03")
	err := h.mustFail(`invalid --oldest value "last week"`, "list", testChannel, "--oldest", "last week")
	for _, want := range []string{"a day (2026-01-31)", "2026-01-31T14:30", "a Slack ts"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
	h.wantCalls()

	// The hint about an empty window says when its bounds are.
	h.history()
	h.mustRun("list", testChannel, "--oldest", "2026-02-03")
	if want := "No messages matched in the channel after 1770076800 (2026-02-03T00:00:00Z)."; !strings.Contains(h.stderr.String(), want) {
		t.Errorf("stderr = %q, want %q", h.stderr, want)
	}
}

// Joins and the like are left out of a channel's history, and counted.
func TestListHistoryHidesSystemMessages(t *testing.T) {
	h := newHarness(t)
	h.history(
		map[string]any{"ts": "1770160600.000200", "user": "U05BRPTKL6A", "text": "<@U05BRPTKL6A> has joined the channel", "subtype": "channel_join"},
		map[string]any{"ts": "1770160500.000100", "user": "U0AAAAAAAAA", "text": "set the channel topic: x", "subtype": "channel_topic"},
		map[string]any{"ts": "1770160000.000001", "user": "U0AAAAAAAAA", "text": "hello"},
	)
	got := h.mustRun("list", testChannel)
	wantJSON(t, got, `{
		"channel_id": "C12345678",
		"messages": [
			{"channel_id": "C12345678", "ts": "1770160000.000001", "time": "2026-02-03T23:06:40Z", "author": {"user_id": "U0AAAAAAAAA"}, "content": "hello"}
		],
		"system_messages_hidden": 2
	}`)
	if h.stderr.Len() != 0 {
		t.Errorf("stderr = %q", h.stderr)
	}

	got = h.mustRun("list", testChannel, "--include-system")
	if msgs := got["messages"].([]any); len(msgs) != 3 || got["system_messages_hidden"] != nil {
		t.Errorf("--include-system printed %s", h.stdout)
	}

	// Nothing but system messages: stderr says why "messages" is empty.
	h.history(map[string]any{"ts": "1770160600.000200", "user": "U05BRPTKL6A", "text": "<@U05BRPTKL6A> has joined the channel", "subtype": "channel_join"})
	got = h.mustRun("list", testChannel)
	wantJSON(t, got, `{"channel_id": "C12345678", "messages": [], "system_messages_hidden": 1}`)
	if want := "other than 1 system messages (joins, leaves, topic changes …): pass --include-system to see them."; !strings.Contains(h.stderr.String(), want) {
		t.Errorf("stderr = %q, want %q", h.stderr, want)
	}
}

// --expand-threads lists the replies of each thread after its root: one
// call for what would otherwise take one per thread.
func TestListHistoryExpandThreads(t *testing.T) {
	h := newHarness(t)
	h.history(
		map[string]any{"ts": "1770160700.000300", "user": "U05BRPTKL6A", "text": "newer"},
		map[string]any{"ts": "1770160000.000001", "thread_ts": "1770160000.000001", "reply_count": 2, "user": "U05BRPTKL6A", "text": "root"},
	)
	threadFixture(h)
	got := h.mustRun("list", testChannel, "--expand-threads")
	wantJSON(t, got, `{"channel_id": "C12345678", "messages": [
		{"channel_id": "C12345678", "ts": "1770160000.000001", "time": "2026-02-03T23:06:40Z", "thread_ts": "1770160000.000001", "reply_count": 2, "author": {"user_id": "U05BRPTKL6A"}, "content": "root"},
		{"channel_id": "C12345678", "ts": "1770160500.000100", "time": "2026-02-03T23:15:00Z", "thread_ts": "1770160000.000001", "author": {"user_id": "U0AAAAAAAAA"}, "content": "first"},
		{"channel_id": "C12345678", "ts": "1770160600.000200", "time": "2026-02-03T23:16:40Z", "thread_ts": "1770160000.000001", "author": {"user_id": "U05BRPTKL6A"}, "content": "second"},
		{"channel_id": "C12345678", "ts": "1770160700.000300", "time": "2026-02-03T23:18:20Z", "author": {"user_id": "U05BRPTKL6A"}, "content": "newer"}
	]}`)
	if calls := h.callsTo("conversations.replies"); len(calls) != 1 || calls[0].params["ts"] != "1770160000.000001" {
		t.Errorf("conversations.replies calls = %v", calls)
	}

	// Without the flag, the root alone, with its reply_count.
	got = h.mustRun("list", testChannel)
	if msgs := got["messages"].([]any); len(msgs) != 2 || msgs[0].(map[string]any)["reply_count"] != float64(2) || len(h.callsTo("conversations.replies")) != 0 {
		t.Errorf("without --expand-threads: %s", h.stdout)
	}

	// A thread listing has its replies already.
	h.mustFail("--expand-threads only applies to channel history", "list", testURL, "--expand-threads")
	h.mustFail("--include-system only applies to channel history", "list", testChannel, "--thread-ts", "1770160000.000001", "--include-system")
}

// A user ID is the DM with that user. Reading it opens no DM: the one
// that exists is looked for among the DMs, and when there is none, there
// is nothing to read.
func TestReadTheDMWithAUser(t *testing.T) {
	h := newHarness(t)
	h.history(map[string]any{"ts": testTS, "user": "U05BRPTKL6A", "text": "hello"})
	h.fixtures["users.conversations"] = func(p map[string]any) (map[string]any, error) {
		if p["cursor"] == nil {
			return map[string]any{
				"ok":                true,
				"channels":          []any{map[string]any{"id": "D00000001", "is_im": true, "user": "U0AAAAAAAAA"}},
				"response_metadata": map[string]any{"next_cursor": "page-2"},
			}, nil
		}
		return map[string]any{"ok": true, "channels": []any{map[string]any{"id": "D12345678", "is_im": true, "user": "U05BRPTKL6A"}}}, nil
	}
	got := h.mustRun("list", "U05BRPTKL6A", "--limit", "5")
	wantJSON(t, got, `{"channel_id": "D12345678", "messages": [
		{"channel_id": "D12345678", "ts": "1770165109.628379", "time": "2026-02-04T00:31:49Z", "author": {"user_id": "U05BRPTKL6A"}, "content": "hello"}
	]}`)
	h.wantCalls(
		call{"users.conversations", map[string]any{"types": "im", "limit": 1000, "exclude_archived": false}},
		call{"users.conversations", map[string]any{"types": "im", "limit": 1000, "exclude_archived": false, "cursor": "page-2"}},
		call{"conversations.history", map[string]any{"channel": "D12345678", "limit": 5}},
	)

	got = h.mustRun("get", "U05BRPTKL6A", "--ts", testTS)
	if got["message"].(map[string]any)["channel_id"] != "D12345678" {
		t.Errorf("get printed %s", h.stdout)
	}

	// No DM with that user: an error, and none is opened.
	h.mustFail("there is no direct message with user U0BBBBBBBBB", "list", "U0BBBBBBBBB")
	if n := len(h.callsTo("conversations.history")); n != 0 {
		t.Errorf("%d conversations.history calls for a DM that does not exist", n)
	}
	h.mustFail("there is no direct message with user U0BBBBBBBBB", "get", "U0BBBBBBBBB", "--ts", testTS)

	// Credentials that cannot list DMs: the error says what to pass.
	h.fixtures["users.conversations"] = func(map[string]any) (map[string]any, error) {
		return map[string]any{"ok": false, "error": "enterprise_is_restricted"}, nil
	}
	h.mustFail("pass the DM's channel ID (D…) or a message URL instead", "list", "U05BRPTKL6A")

	if n := len(h.callsTo("conversations.open")); n != 0 {
		t.Errorf("%d conversations.open calls: reading must not open a DM", n)
	}
}

// --resolve-users names the author on the message itself.
func TestResolveUsersNamesTheAuthor(t *testing.T) {
	h := newHarness(t)
	h.history(
		map[string]any{"ts": "1770160600.000200", "user": "U05BRPTKL6A", "text": "newer"},
		map[string]any{"ts": "1770160000.000001", "bot_id": "B0AAAAAAAAA", "text": "from a bot"},
	)
	h.fixtures["users.info"] = func(p map[string]any) (map[string]any, error) {
		return map[string]any{"ok": true, "user": map[string]any{
			"id": p["user"], "name": "alice", "real_name": "Alice Martin", "profile": map[string]any{"display_name": "ali"},
		}}, nil
	}
	got := h.mustRun("list", testChannel, "--resolve-users")
	msgs := got["messages"].([]any)
	if author := msgs[1].(map[string]any)["author"]; !reflect.DeepEqual(author, map[string]any{"user_id": "U05BRPTKL6A", "name": "ali"}) {
		t.Errorf("author = %v", author)
	}
	if author := msgs[0].(map[string]any)["author"]; !reflect.DeepEqual(author, map[string]any{"bot_id": "B0AAAAAAAAA"}) {
		t.Errorf("bot author = %v", author)
	}
	if got["referenced_users"] == nil {
		t.Error("referenced_users is gone")
	}

	got = h.mustRun("list", testChannel)
	if author := got["messages"].([]any)[1].(map[string]any)["author"]; !reflect.DeepEqual(author, map[string]any{"user_id": "U05BRPTKL6A"}) {
		t.Errorf("without --resolve-users: author = %v", author)
	}
}

func TestListHistoryDefaultLimitAndEmptyResult(t *testing.T) {
	h := newHarness(t)
	got := h.mustRun("list", testChannel)
	wantJSON(t, got, `{"channel_id": "C12345678", "messages": []}`)
	// "messages" is there although empty: jq '.messages[]' must not fail.
	if want := "{\"channel_id\":\"C12345678\",\"messages\":[]}\n"; h.stdout.String() != want {
		t.Errorf("printed %s, want %s", h.stdout, want)
	}
	if limit := h.callsTo("conversations.history")[0].params["limit"]; limit != 25 {
		t.Errorf("limit = %v, want 25", limit)
	}
	if !strings.Contains(h.stderr.String(), "No messages matched") {
		t.Errorf("stderr = %q, want a hint that nothing matched", h.stderr.String())
	}
}

func TestListHistoryByChannelURL(t *testing.T) {
	h := newHarness(t)
	h.history(map[string]any{"ts": testTS, "text": "x"})
	got := h.mustRun("list", testWorkspace+"/archives/"+testChannel)
	if got["channel_id"] != testChannel || len(h.callsTo("conversations.history")) != 1 {
		t.Errorf("got %v, calls %v", got, h.methods())
	}
}

func TestListHistoryReactionFilters(t *testing.T) {
	h := newHarness(t)
	h.history(
		map[string]any{"ts": "1770160600.000200", "text": "done", "reactions": []any{map[string]any{"name": "eyes", "users": []any{"U05BRPTKL6A"}, "count": 1}, map[string]any{"name": "white_check_mark", "users": []any{"U05BRPTKL6A"}, "count": 1}}},
		map[string]any{"ts": "1770160500.000100", "text": "todo", "reactions": []any{map[string]any{"name": "eyes", "users": []any{"U05BRPTKL6A"}, "count": 1}}},
		map[string]any{"ts": "1770160000.000001", "text": "unseen"},
	)
	got := h.mustRun("list", testChannel, "--oldest", "1770100000.000000",
		"--with-reaction", ":eyes:", "--with-reaction", "👀", "--without-reaction", "white_check_mark")
	msgs, _ := got["messages"].([]any)
	if len(msgs) != 1 || msgs[0].(map[string]any)["content"] != "todo" {
		t.Fatalf("messages = %v", msgs)
	}
	// Reactions are fetched for filtering but only printed on request.
	if msgs[0].(map[string]any)["reactions"] != nil {
		t.Errorf("reactions printed without --include-reactions: %v", msgs[0])
	}
}

func TestListValidation(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"limit not a number", []string{testChannel, "--limit", "abc"}, `invalid argument "abc" for "--limit"`},
		{"limit zero", []string{testChannel, "--limit", "0"}, "must be between 1 and 1000"},
		{"limit negative", []string{testChannel, "--limit", "-5"}, "must be between 1 and 1000"},
		{"limit fractional", []string{testChannel, "--limit", "3.5"}, `invalid argument "3.5" for "--limit"`},
		{"limit above the maximum", []string{testChannel, "--limit", "1001"}, "must be between 1 and 1000"},
		{"oldest garbage", []string{testChannel, "--oldest", "yesterday"}, "invalid --oldest"},
		{"latest garbage", []string{testChannel, "--latest", "now"}, "invalid --latest"},
		{"reaction filter without oldest", []string{testChannel, "--with-reaction", "dart"}, "reaction filters require --oldest (a day such as 2026-01-31, or a ts such as 1770165109.628379) to bound scan size"},
		{"negative filter without oldest", []string{testChannel, "--without-reaction", "dart"}, "reaction filters require --oldest"},
		{"reaction filter with blank oldest", []string{testChannel, "--with-reaction", "dart", "--oldest", " "}, "reaction filters require --oldest"},
		{"unknown emoji", []string{testChannel, "--oldest", "1770100000", "--with-reaction", "not an emoji"}, "--with-reaction: unsupported emoji"},
		{"contradictory filters", []string{testChannel, "--oldest", "1770100000", "--with-reaction", "dart", "--without-reaction", "🎯"}, "both --with-reaction and --without-reaction"},
		{"user handle", []string{"@alice"}, `"@alice" is a user handle, not a target: pass the user's ID (U…), which "user get @alice" prints`},
		{"bad thread ts", []string{testChannel, "--thread-ts", "abc"}, "invalid --thread-ts"},
		{"bad ts", []string{testChannel, "--ts", "abc"}, "invalid --ts"},
		{"zero max-body-chars", []string{testChannel, "--max-body-chars", "0"}, "--max-body-chars"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			h.mustFail(tt.want, append([]string{"list"}, tt.args...)...)
			h.wantCalls()
		})
	}
}

func TestParseReactionFilters(t *testing.T) {
	got, err := parseReactionFilters("--with-reaction", []string{":dart:", "dart", "🎯"})
	if err != nil || !reflect.DeepEqual(got, []string{"dart"}) {
		t.Errorf("got %v, %v; want [dart]", got, err)
	}
	if got, err := parseReactionFilters("--with-reaction", nil); err != nil || len(got) != 0 {
		t.Errorf("unset: got %v, %v", got, err)
	}
	if _, err := parseReactionFilters("--with-reaction", []string{""}); err == nil {
		t.Error("an empty emoji was accepted")
	}
}

func TestRequireOldestForReactionFilters(t *testing.T) {
	if err := requireOldestForReactionFilters("", false); err != nil {
		t.Errorf("no filters: %v", err)
	}
	if err := requireOldestForReactionFilters("1770165109.628379", true); err != nil {
		t.Errorf("filters with oldest: %v", err)
	}
	if err := requireOldestForReactionFilters("", true); err == nil {
		t.Error("filters without oldest accepted")
	}
}

func TestWorkspaceSelection(t *testing.T) {
	h := newHarness(t)
	h.addWorkspace("https://other.slack.com")
	permalink := func(args ...string) string {
		p, _ := h.mustRun(args...)["permalink"].(string)
		return p
	}
	// The default workspace, then --workspace, then SLACK_WORKSPACE_URL.
	if p := permalink("send", testChannel, "hi"); !strings.HasPrefix(p, testWorkspace+"/") {
		t.Errorf("default: permalink = %s", p)
	}
	if p := permalink("send", testChannel, "hi", "--workspace", "other"); !strings.HasPrefix(p, "https://other.slack.com/") {
		t.Errorf("--workspace: permalink = %s", p)
	}
	t.Setenv("SLACK_WORKSPACE_URL", "https://other.slack.com")
	if p := permalink("send", "#general", "hi"); !strings.HasPrefix(p, "https://other.slack.com/") {
		t.Errorf("SLACK_WORKSPACE_URL: permalink = %s", p)
	}
	// A URL names its workspace, whatever the default.
	h.history(map[string]any{"ts": testTS, "text": "x"})
	if p := permalink("send", testURL, "hi"); !strings.HasPrefix(p, testWorkspace+"/") {
		t.Errorf("URL target: permalink = %s", p)
	}
	h.mustFail("no-such-workspace", "send", testChannel, "hi", "--workspace", "no-such-workspace")
}
