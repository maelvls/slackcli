package messagecmd

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/maelvls/slackcli/internal/clictx"
	"github.com/maelvls/slackcli/internal/slackapi"
)

// fastClock makes every reading of the clock 10s later than the one
// before, and the sleeps between polls instant: a wait of minutes runs in
// a few milliseconds.
func fastClock(t *testing.T, h *harness) {
	t.Helper()
	var mu sync.Mutex
	now := time.Date(2026, 2, 4, 10, 0, 0, 0, time.UTC)
	h.c.now = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		now = now.Add(10 * time.Second)
		return now
	}
	old := wakeStep
	wakeStep = time.Millisecond
	t.Cleanup(func() { wakeStep = old })
}

func msg(ts, user, text string) map[string]any {
	return map[string]any{"ts": ts, "user": user, "text": text}
}

// pages answers conversations.history and conversations.replies from
// funcs of the poll number (1 for the first poll).
func (h *harness) pollFixtures(replies, history func(poll int) ([]map[string]any, error)) {
	var mu sync.Mutex
	poll := 0
	answer := func(msgs []map[string]any, err error) (map[string]any, error) {
		if err != nil {
			return nil, err
		}
		out := make([]any, len(msgs))
		for i, m := range msgs {
			out[i] = m
		}
		return map[string]any{"ok": true, "messages": out}, nil
	}
	h.fixtures["conversations.replies"] = func(map[string]any) (map[string]any, error) {
		mu.Lock()
		poll++
		n := poll
		mu.Unlock()
		return answer(replies(n))
	}
	h.fixtures["conversations.history"] = func(map[string]any) (map[string]any, error) {
		mu.Lock()
		n := poll
		mu.Unlock()
		return answer(history(n))
	}
}

func TestWaitPrintsWhatIsAlreadyThere(t *testing.T) {
	h := newHarness(t)
	h.replies(
		map[string]any{"ts": testTS, "thread_ts": testTS, "user": "U1", "text": "question?", "reply_count": 1},
		map[string]any{"ts": "1770165200.000001", "thread_ts": testTS, "user": "U2", "text": "in the thread"},
	)
	h.history(
		msg("1770165300.000001", "U2", "at the top level"),
		msg("1770165200.000001", "U2", "in the thread"), // a broadcast reply, seen twice
	)

	got := h.mustRun("wait", testURL, "--no-download")
	wantJSON(t, got, `{"channel_id": "C12345678", "messages": [
		{"channel_id": "C12345678", "ts": "1770165200.000001", "thread_ts": "1770165109.628379", "time": "2026-02-04T00:33:20Z", "author": {"user_id": "U2"}, "content": "in the thread"},
		{"channel_id": "C12345678", "ts": "1770165300.000001", "time": "2026-02-04T00:35:00Z", "author": {"user_id": "U2"}, "content": "at the top level"}
	]}`)
	hist := h.callsTo("conversations.history")
	if len(hist) != 1 || hist[0].params["oldest"] != testTS {
		t.Errorf("history calls = %+v, want one with oldest %s", hist, testTS)
	}
}

func TestWaitPollsUntilSomethingIsPosted(t *testing.T) {
	h := newHarness(t)
	fastClock(t, h)
	root := msg(testTS, "U1", "question?")
	h.pollFixtures(
		func(poll int) ([]map[string]any, error) {
			if poll < 3 {
				return []map[string]any{root}, nil
			}
			return []map[string]any{root, msg("1770165200.000001", "U2", "answer")}, nil
		},
		func(int) ([]map[string]any, error) { return nil, nil },
	)

	got := h.mustRun("wait", testURL, "--no-download", "--interval", "1m")
	if msgs := got["messages"].([]any); len(msgs) != 1 || msgs[0].(map[string]any)["content"] != "answer" {
		t.Errorf("messages = %v, want the answer only", got["messages"])
	}
	if n := len(h.callsTo("conversations.replies")); n != 3 {
		t.Errorf("polled %d times, want 3", n)
	}
}

func TestWaitAfter(t *testing.T) {
	h := newHarness(t)
	h.replies(msg(testTS, "U1", "question?"), msg("1770165200.000001", "U2", "seen already"), msg("1770165300.000001", "U2", "new"))
	h.history()
	got := h.mustRun("wait", testURL, "--no-download", "--after", "1770165200.000001")
	if msgs := got["messages"].([]any); len(msgs) != 1 || msgs[0].(map[string]any)["content"] != "new" {
		t.Errorf("messages = %v, want only the one after --after", got["messages"])
	}
}

func TestWaitTimesOut(t *testing.T) {
	h := newHarness(t)
	fastClock(t, h)
	h.replies(msg(testTS, "U1", "question?"))
	h.history()

	got := h.mustRun("wait", testURL, "--timeout", "2m", "--interval", "1m")
	wantJSON(t, got, `{"channel_id": "C12345678", "messages": [], "timed_out": true, "hints": [
		"Nothing was posted after 1770165109.628379 (2026-02-04T00:31:49Z) within 2m0s: to wait again, run the same command with --after 1770165109.628379."
	]}`)
}

func TestWaitKeepsTryingWhenSlackCannotBeReached(t *testing.T) {
	h := newHarness(t)
	fastClock(t, h)
	root := msg(testTS, "U1", "question?")
	h.pollFixtures(
		func(poll int) ([]map[string]any, error) {
			switch {
			case poll <= 3:
				return nil, &slackapi.NetworkError{Method: "conversations.replies", Err: errors.New("no route to host")}
			case poll == 4:
				return []map[string]any{root}, nil
			}
			return []map[string]any{root, msg("1770165200.000001", "U2", "answer")}, nil
		},
		func(int) ([]map[string]any, error) { return nil, nil },
	)

	got := h.mustRun("wait", testURL, "--no-download")
	if msgs := got["messages"].([]any); len(msgs) != 1 {
		t.Errorf("messages = %v, want the answer", got["messages"])
	}
	stderr := h.stderr.String()
	if n := strings.Count(stderr, "Cannot reach Slack"); n != 1 {
		t.Errorf("stderr says %d times that Slack cannot be reached, want once:\n%s", n, stderr)
	}
	if !strings.Contains(stderr, "Slack answers again.") {
		t.Errorf("stderr does not say Slack answers again:\n%s", stderr)
	}
}

func TestWaitStopsOnOtherErrors(t *testing.T) {
	h := newHarness(t)
	fastClock(t, h)
	h.fixtures["conversations.replies"] = func(map[string]any) (map[string]any, error) {
		return nil, apiError("conversations.replies", "channel_not_found")
	}
	h.mustFail("channel_not_found", "wait", testURL)
	if n := len(h.callsTo("conversations.replies")); n != 1 {
		t.Errorf("polled %d times, want 1", n)
	}
}

// A page of history holds the newest messages: when it is full, the
// older ones are fetched too, so that the first answer is not lost.
func TestWaitPagesBackThroughABusyChannel(t *testing.T) {
	h := newHarness(t)
	h.replies(msg(testTS, "U1", "question?"))
	const full = 1000 // messages.MaxHistoryLimit
	newest := make([]any, full)
	for i := range newest {
		// conversations.history answers newest first.
		newest[i] = msg(fmt.Sprintf("1770170000.%06d", full-i), "U3", "chatter")
	}
	h.fixtures["conversations.history"] = func(p map[string]any) (map[string]any, error) {
		if p["latest"] == nil {
			return map[string]any{"ok": true, "messages": newest, "has_more": true}, nil
		}
		return map[string]any{"ok": true, "messages": []any{msg("1770165200.000001", "U2", "the answer")}}, nil
	}

	got := h.mustRun("wait", testURL, "--no-download")
	msgs := got["messages"].([]any)
	if len(msgs) != full+1 || msgs[0].(map[string]any)["content"] != "the answer" {
		t.Fatalf("got %d messages, first %v; want %d, the answer first", len(msgs), msgs[0], full+1)
	}
	hist := h.callsTo("conversations.history")
	if last := hist[len(hist)-1]; last.params["latest"] != "1770170000.000001" {
		t.Errorf("last history call = %+v, want latest 1770170000.000001 (the oldest of the full page)", last.params)
	}
}

func TestWaitHasNoCommandTimeout(t *testing.T) {
	h := newHarness(t)
	if !clictx.HasNoTimeout(h.c.newWait()) {
		t.Error("wait can wait for hours: it must not be cut by the command timeout")
	}
}

func TestWaitRejectsBadFlags(t *testing.T) {
	h := newHarness(t)
	h.mustFail("--interval must be at least 1s", "wait", testURL, "--interval", "10ms")
	h.mustFail("--timeout must be a positive duration", "wait", testURL, "--timeout", "-1m")
	h.mustFail("--ts", "wait", "#general")
}
