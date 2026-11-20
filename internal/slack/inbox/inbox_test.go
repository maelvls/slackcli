package inbox

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/maelvls/slackcli/internal/slackapi"
)

const (
	me   = "UMEMEMEME"
	base = "https://acme.slack.com"
)

// since is ts 1000.
var since = time.Unix(1000, 0)

type call struct {
	method string
	params map[string]any
}

func ts(n float64) string { return fmt.Sprintf("%.6f", n) }

func msg(user string, at float64, extra ...any) map[string]any {
	m := map[string]any{"type": "message", "user": user, "ts": ts(at), "text": fmt.Sprintf("from %s at %v", user, at)}
	for i := 0; i+1 < len(extra); i += 2 {
		m[extra[i].(string)] = extra[i+1]
	}
	return m
}

// match is a search.messages match; threadTS < 0 means "not in a thread".
func match(channel map[string]any, user string, at, threadTS float64) map[string]any {
	id := channel["id"].(string)
	link := base + "/archives/" + id + "/p" + strings.ReplaceAll(ts(at), ".", "")
	if threadTS >= 0 {
		link += "?thread_ts=" + ts(threadTS) + "&cid=" + id
	}
	return map[string]any{"channel": channel, "user": user, "ts": ts(at), "permalink": link}
}

func im(id, other string) map[string]any {
	return map[string]any{"id": id, "is_im": true, "name": other}
}

func channel(id, name string) map[string]any {
	return map[string]any{"id": id, "name": name, "is_channel": true}
}

// world is a fake workspace: what the three searches answer, and the
// messages of each conversation and thread, oldest first.
type world struct {
	to, mentions, mine []map[string]any
	history            map[string][]map[string]any
	replies            map[string][]map[string]any
	// pages is the number of pages each search claims to have (0: one).
	pages int
	// fail answers a method with an error instead.
	fail map[string]error

	mu       sync.Mutex
	calls    []call
	inFlight int
	peak     int
}

func (w *world) client() *slackapi.Client {
	return slackapi.NewFake(func(_ context.Context, method string, params map[string]any) (map[string]any, error) {
		// The names are looked up at the end, by package users, with a
		// bound of its own.
		counted := 0
		if method != "users.info" {
			counted = 1
		}
		w.mu.Lock()
		w.calls = append(w.calls, call{method, params})
		w.inFlight += counted
		w.peak = max(w.peak, w.inFlight)
		w.mu.Unlock()
		defer func() {
			w.mu.Lock()
			w.inFlight -= counted
			w.mu.Unlock()
		}()
		// Let the calls overlap, so that peak means something.
		time.Sleep(time.Millisecond)
		if err := w.fail[method]; err != nil {
			return nil, err
		}
		return w.serve(method, params), nil
	})
}

func apiError(code string) map[string]any { return map[string]any{"ok": false, "error": code} }

func (w *world) serve(method string, params map[string]any) map[string]any {
	id, _ := params["channel"].(string)
	switch method {
	case "auth.test":
		return map[string]any{"user_id": me, "url": base + "/"}
	case "search.messages":
		matches := w.mentions
		switch q := params["query"].(string); {
		case strings.HasPrefix(q, "to:"):
			matches = w.to
		case strings.HasPrefix(q, "is:thread"):
			matches = w.mine
		}
		return map[string]any{"messages": map[string]any{"matches": toAny(matches), "paging": map[string]any{"pages": float64(max(w.pages, 1))}}}
	case "conversations.history":
		oldest := tsNum(params["oldest"].(string))
		var out []map[string]any
		for _, m := range w.history[id] {
			at := tsNum(m["ts"].(string))
			if at > oldest || (params["inclusive"] == true && at == oldest) {
				out = append(out, m)
			}
		}
		// Slack answers newest first.
		slices.Reverse(out)
		return map[string]any{"messages": toAny(out[:min(len(out), params["limit"].(int))])}
	case "conversations.replies":
		msgs, ok := w.replies[id+":"+params["ts"].(string)]
		if !ok {
			return apiError("thread_not_found")
		}
		return map[string]any{"messages": toAny(msgs), "has_more": false}
	case "users.info":
		user := params["user"].(string)
		return map[string]any{"user": map[string]any{"id": user, "name": "name-" + user, "profile": map[string]any{}}}
	}
	return apiError("unknown_method")
}

func toAny(msgs []map[string]any) []any {
	out := make([]any, len(msgs))
	for i, m := range msgs {
		out[i] = m
	}
	return out
}

func (w *world) only(methods ...string) []call {
	var out []call
	for _, c := range w.calls {
		if slices.Contains(methods, c.method) {
			out = append(out, c)
		}
	}
	return out
}

func (w *world) fetch(t *testing.T, opts Options) Result {
	t.Helper()
	opts.Since, opts.CacheDir = since, t.TempDir()
	res, err := Fetch(context.Background(), w.client(), opts)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func newWorld() *world {
	const top = -1
	d1, d2, d3, d4 := im("D1", "U00000001"), im("D2", "U00000002"), im("D3", "U00000003"), im("D4", "U00000004")
	c1, c2, c3 := channel("C1", "answered-in-thread"), channel("C2", "answered-in-channel"), channel("C3", "unanswered")
	c4, c5 := channel("C4", "my-thread"), channel("C5", "dedupe")
	return &world{
		to: []map[string]any{
			match(d1, "U00000001", 1003, top),
			match(d2, "U00000002", 1001, top),
			match(d3, "U00000003", 1001, top),
			match(im("DSELF", me), me, 1001, top),
			match(im("DOLD", "U00000008"), "U00000008", 500, top), // before --since
		},
		mentions: []map[string]any{
			match(c1, "U00000004", 1001, 1000.5),
			match(c2, "U00000005", 1001, top),
			match(c3, "U00000005", 1010, top),
			match(c5, "U00000007", 1003, 1000),
			match(d4, "U00000004", 1004, top), // a mention in a DM: reported as dm
		},
		mine: []map[string]any{match(c4, me, 1001, 1000), match(c5, me, 1001, 1000)},
		history: map[string][]map[string]any{
			"D1": {msg("U00000001", 1001), msg(me, 1001.5), msg("U00000001", 1002), msg("U00000001", 1003)},
			"D2": {msg("U00000002", 1001), msg(me, 1002)},
			"D3": {msg("U00000003", 1001, "reactions", []any{map[string]any{"name": "eyes", "users": []any{me}, "count": 1.0}})},
			"D4": {msg("U00000004", 1004, "text", "hey <@UMEMEMEME>")},
			"C2": {msg("U00000005", 1001), msg(me, 1005)},
			"C3": {msg("U00000005", 1010), msg("U00000006", 1011)},
		},
		replies: map[string][]map[string]any{
			"C1:1000.500000": {msg("U00000004", 1000.5), msg("U00000004", 1001), msg(me, 1002)},
			"C2:1001.000000": {msg("U00000005", 1001)},
			"C3:1010.000000": {msg("U00000005", 1010, "text", strings.Repeat("a", 400))},
			"C4:1000.000000": {
				msg("U00000006", 999),
				msg(me, 1001),
				msg("U00000006", 1002),
				msg("UBOT", 1003, "bot_id", "B1"),
				msg("USLACKBOT", 1004),
				msg("U00000006", 1005, "subtype", "channel_join"),
			},
			"C5:1000.000000": {msg("U00000007", 999), msg(me, 1001), msg("U00000007", 1003)},
		},
	}
}

func byChannel(res Result) map[string]Item {
	out := map[string]Item{}
	for _, item := range res.Items {
		out[item.ChannelID] = item
	}
	return out
}

func kinds(res Result) []string {
	var out []string
	for _, item := range res.Items {
		out = append(out, item.Kind+" "+item.ChannelID)
	}
	return out
}

func TestFetch(t *testing.T) {
	w := newWorld()
	res := w.fetch(t, Options{})

	if res.UserID != me || res.Since != "1970-01-01T00:16:40.000Z" || res.Truncated {
		t.Errorf("user_id, since, truncated = %q, %q, %v", res.UserID, res.Since, res.Truncated)
	}
	// Newest first. Not there: D2 (I replied), D3 (I reacted), C1 (answered
	// in the thread), C2 (answered in the channel), the DM with myself, and
	// DOLD (before the window).
	want := []string{"mention C3", "dm D4", "dm D1", "mention C5", "thread C4"}
	if got := kinds(res); !reflect.DeepEqual(got, want) {
		t.Fatalf("items = %q, want %q", got, want)
	}
	items := byChannel(res)

	// A DM: no thread_ts, named after the other user, counting what came
	// after my last message.
	if got, want := items["D1"], (Item{
		Kind: KindDM, ChannelID: "D1", ChannelName: "name-U00000001",
		TS: ts(1003), FirstTS: ts(1002), Count: 2,
		Author: Author{UserID: "U00000001", Name: "name-U00000001"},
		Text:   "from U00000001 at 1003", Permalink: base + "/archives/D1/p1003000000",
	}); got != want {
		t.Errorf("D1 = %+v\nwant %+v", got, want)
	}

	// A top-level mention nobody answered: thread_ts is the message itself,
	// and the text is cut at 300 characters.
	if got, want := items["C3"], (Item{
		Kind: KindMention, ChannelID: "C3", ChannelName: "unanswered", ThreadTS: ts(1010),
		TS: ts(1010), FirstTS: ts(1010), Count: 1,
		Author: Author{UserID: "U00000005", Name: "name-U00000005"},
		Text:   strings.Repeat("a", 300) + "...", Permalink: base + "/archives/C3/p1010000000",
	}); got != want {
		t.Errorf("C3 = %+v\nwant %+v", got, want)
	}

	// Bots, Slackbot and joins are not "others", and the message before
	// the window does not count.
	if got, want := items["C4"], (Item{
		Kind: KindThread, ChannelID: "C4", ChannelName: "my-thread", ThreadTS: ts(1000),
		TS: ts(1002), FirstTS: ts(1002), Count: 1,
		Author: Author{UserID: "U00000006", Name: "name-U00000006"},
		Text:   "from U00000006 at 1002", Permalink: base + "/archives/C4/p1002000000?thread_ts=" + ts(1000) + "&cid=C4",
	}); got != want {
		t.Errorf("C4 = %+v\nwant %+v", got, want)
	}

	// A mention in a DM is a dm item, without thread_ts.
	if got := items["D4"]; got.Kind != KindDM || got.ThreadTS != "" || got.ChannelName != "name-U00000004" {
		t.Errorf("D4 = %+v", got)
	}
	// The thread found both as a mention and as one of mine is one mention
	// item (checked by the list of kinds above).
	if got := items["C5"]; got.ThreadTS != ts(1000) || got.TS != ts(1003) {
		t.Errorf("C5 = %+v", got)
	}

	// The DM with myself and the one before the window are never read.
	for _, c := range w.only("conversations.history") {
		if id := c.params["channel"]; id == "DSELF" || id == "DOLD" {
			t.Errorf("%v was read", id)
		}
	}

	// The searches start two days before --since, newest first.
	var queries []string
	for _, c := range w.only("search.messages") {
		queries = append(queries, c.params["query"].(string))
		if c.params["sort"] != "timestamp" || c.params["sort_dir"] != "desc" {
			t.Errorf("search params = %v", c.params)
		}
	}
	slices.Sort(queries)
	if want := []string{"<@UMEMEMEME> after:1969-12-30", "is:thread from:<@UMEMEMEME> after:1969-12-30", "to:<@UMEMEMEME> after:1969-12-30"}; !reflect.DeepEqual(queries, want) {
		t.Errorf("queries = %q, want %q", queries, want)
	}

	// Conversations are read from the start of the window, a few at a time.
	for _, c := range w.only("conversations.replies") {
		if c.params["oldest"] != "1000.000000" || c.params["inclusive"] != true {
			t.Errorf("conversations.replies params = %v", c.params)
		}
	}
	if w.peak > concurrency || w.peak < 2 {
		t.Errorf("%d calls in flight at most, want several and no more than %d", w.peak, concurrency)
	}
	// Reading is all it does.
	for _, c := range w.calls {
		if !slices.Contains([]string{"auth.test", "search.messages", "conversations.history", "conversations.replies", "users.info"}, c.method) {
			t.Errorf("unexpected call %s", c.method)
		}
	}
}

func TestFetchNoThreads(t *testing.T) {
	w := newWorld()
	res := w.fetch(t, Options{NoThreads: true})
	// C5 is still a mention; C4, which I only posted in, is gone.
	if got, want := kinds(res), []string{"mention C3", "dm D4", "dm D1", "mention C5"}; !reflect.DeepEqual(got, want) {
		t.Errorf("items = %q, want %q", got, want)
	}
	if n := len(w.only("search.messages")); n != 2 {
		t.Errorf("searches = %d, want 2", n)
	}
	for _, c := range w.only("search.messages") {
		if strings.HasPrefix(c.params["query"].(string), "is:thread") {
			t.Error("my thread posts were searched")
		}
	}
}

func TestFetchMaxConversations(t *testing.T) {
	w := newWorld()
	res := w.fetch(t, Options{MaxConversations: 1})
	if !res.Truncated {
		t.Error("not truncated")
	}
	// The newest candidate (C3), then the look for an answer in its channel.
	var methods []string
	for _, c := range w.only("conversations.history", "conversations.replies") {
		methods = append(methods, c.method)
	}
	if want := []string{"conversations.replies", "conversations.history"}; !reflect.DeepEqual(methods, want) {
		t.Errorf("inspections = %q, want %q", methods, want)
	}
	if got, want := kinds(res), []string{"mention C3"}; !reflect.DeepEqual(got, want) {
		t.Errorf("items = %q, want %q", got, want)
	}
}

func TestFetchCappedSearch(t *testing.T) {
	w := newWorld()
	w.pages = 5
	res := w.fetch(t, Options{})
	if !res.Truncated {
		t.Error("not truncated")
	}
	if n := len(w.only("search.messages")); n != 3*searchPages {
		t.Errorf("searches = %d, want %d", n, 3*searchPages)
	}
}

func TestFetchNothingWaiting(t *testing.T) {
	res := (&world{}).fetch(t, Options{})
	if res.Items == nil || len(res.Items) != 0 || res.Truncated {
		t.Errorf("result = %+v", res)
	}
}

func TestFetchMaxBodyChars(t *testing.T) {
	if got := byChannel(newWorld().fetch(t, Options{MaxBodyChars: -1}))["C3"].Text; got != strings.Repeat("a", 400) {
		t.Errorf("unlimited text has %d characters", len(got))
	}
	if got := byChannel(newWorld().fetch(t, Options{MaxBodyChars: 5}))["C3"].Text; got != "aaaaa..." {
		t.Errorf("text = %q", got)
	}
}

// A conversation that cannot be read is left out; what makes the listing
// unreliable fails it.
func TestFetchErrors(t *testing.T) {
	// Slack answers thread_not_found for C5.
	w := newWorld()
	delete(w.replies, "C5:1000.000000")
	if got, want := kinds(w.fetch(t, Options{})), []string{"mention C3", "dm D4", "dm D1", "thread C4"}; !reflect.DeepEqual(got, want) {
		t.Errorf("items = %q, want %q", got, want)
	}

	rateLimit := &slackapi.RateLimitError{Method: "conversations.replies", RetryAfter: time.Minute}
	for name, tc := range map[string]struct {
		method string
		err    error
	}{
		"rate limit":       {"conversations.replies", rateLimit},
		"dead credentials": {"conversations.history", &slackapi.APIError{Method: "conversations.history", Code: "invalid_auth"}},
		"network":          {"conversations.history", errors.New("connection reset")},
		"search":           {"search.messages", &slackapi.APIError{Method: "search.messages", Code: "not_allowed_token_type"}},
		"auth.test":        {"auth.test", &slackapi.APIError{Method: "auth.test", Code: "invalid_auth"}},
	} {
		t.Run(name, func(t *testing.T) {
			w := newWorld()
			w.fail = map[string]error{tc.method: tc.err}
			_, err := Fetch(context.Background(), w.client(), Options{Since: since, CacheDir: t.TempDir()})
			if !errors.Is(err, tc.err) {
				t.Errorf("err = %v, want %v", err, tc.err)
			}
		})
	}
}

func TestParseSince(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	for input, want := range map[string]string{
		"3d":         "2026-11-07T12:00:00Z",
		"6h":         "2026-11-10T06:00:00Z",
		" 12h ":      "2026-11-10T00:00:00Z",
		"2026-11-07": "2026-11-07T00:00:00Z",
	} {
		got, err := ParseSince(input, now)
		if err != nil || got.UTC().Format(time.RFC3339) != want {
			t.Errorf("ParseSince(%q) = %v, %v; want %s", input, got, err, want)
		}
	}
	for _, input := range []string{"", "3 days", "3w", "-3d", "2026-13-45", "2026-11-07T10:00", "99999999999999999999d", "9999999999h"} {
		if _, err := ParseSince(input, now); err == nil || !strings.Contains(err.Error(), "invalid --since value") {
			t.Errorf("ParseSince(%q) = %v, want an error", input, err)
		}
	}
}
