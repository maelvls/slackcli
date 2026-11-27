package inboxcmd

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/maelvls/slackcli/internal/clictx"
	"github.com/maelvls/slackcli/internal/cmd/cmdutil"
	"github.com/maelvls/slackcli/internal/slack/channels"
	"github.com/maelvls/slackcli/internal/slack/inbox"
	"github.com/maelvls/slackcli/internal/slack/messages"
	"github.com/maelvls/slackcli/internal/slack/rtm"
	"github.com/maelvls/slackcli/internal/slackapi"
)

// Tests shorten these.
var (
	// streamOptions tunes the real-time connection.
	streamOptions rtm.Options
	// settle is how long, after the first item, the command keeps
	// listening, so that a burst of messages comes out as one result.
	settle = 2 * time.Second
	// clockStep is how often the wall clock is read for --timeout: timers
	// stand still while a Mac sleeps.
	clockStep = 5 * time.Second
	// now is the clock.
	now = time.Now
	// indexLag is how long Slack's search may take to index a message:
	// before printing, a catch-up runs at least that long after the
	// connection opened.
	indexLag = 15 * time.Second
	// retryDelay is the first pause before looking again for what could
	// not be judged or searched because Slack failed or rate limited.
	retryDelay = 30 * time.Second
)

const maxRetryDelay = 5 * time.Minute

// errTimedOut is the cause of the cancellation when --timeout runs out.
var errTimedOut = errors.New("timed out")

// waitResult is what "inbox wait" prints.
type waitResult struct {
	UserID string `json:"user_id"`
	// After is the cursor to pass as --after to wait for what comes next:
	// the newest item's ts, or the --after waited from.
	After     string       `json:"after"`
	Truncated bool         `json:"truncated,omitempty"`
	Items     []inbox.Item `json:"items"`
	TimedOut  bool         `json:"timed_out,omitempty"`
}

// found collects the items of the stream and of the catch-ups.
type found struct {
	mu        sync.Mutex
	items     map[string]inbox.Item
	truncated bool
	err       error
	// onFirst is called, once, when the first item or error comes.
	onFirst func()
	fired   bool
}

func (f *found) add(items []inbox.Item, truncated bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.truncated = f.truncated || truncated
	for _, it := range items {
		key := it.ChannelID + ":" + it.ThreadTS
		if old, ok := f.items[key]; !ok || messages.CompareTS(it.TS, old.TS) > 0 {
			f.items[key] = it
		}
	}
	if len(f.items) > 0 {
		f.fire()
	}
}

func (f *found) fail(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err == nil {
		f.err = err
	}
	f.fire()
}

func (f *found) fire() {
	if !f.fired && f.onFirst != nil {
		f.fired = true
		f.onFirst()
	}
}

func (f *found) list() []inbox.Item {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]inbox.Item, 0, len(f.items))
	for _, it := range f.items {
		out = append(out, it)
	}
	slices.SortFunc(out, func(a, b inbox.Item) int { return messages.CompareTS(b.TS, a.TS) })
	return out
}

func newWait(rt *clictx.Context) *cobra.Command {
	var (
		workspace, after               string
		maxBodyChars, maxConversations int
		noThreads                      bool
		timeout                        time.Duration
	)
	cmd := &cobra.Command{
		Use:   "wait",
		Short: "Wait until something new waits on you in the inbox, then print it",
		Long: `Wait until a new message puts something in the inbox (a DM or group DM, an
@mention, a reply in a thread you posted in: see "inbox"), print it and
exit: the way to be woken up by Slack instead of running "inbox" in a loop.

It stays connected to Slack's real-time API, like "watch", and judges each
message as it arrives, by the rules of "inbox": a channel message that
neither mentions you nor replies in a thread you posted in costs no call;
the others cost a read of their conversation or thread (once per thread
for the threads you did not post in). Your own messages, bots and system
messages are ignored.

Only messages strictly after --after count; by default, those from now on.
With --after, what came between --after and now is looked for first,
through Slack's search ("inbox" with that start), and so is what came
while the connection was down, after each reconnection: pass the "after"
of the previous result to miss nothing between two runs. A message that
could not be judged because Slack failed or rate limited is looked for
the same way, later.

Once there is a first item, it listens for 2s more, so that messages sent
together come out together. Then, as Slack's search index lags behind,
it searches once more, at least 15s after it connected, for what came
just before: the first result can take that long. When that search, or
any other, could not be done, "after" stays at --after (with a warning),
so that the next run looks again: an item may then be printed twice.

Prints JSON:
  user_id    you
  after      the --after to pass to wait for what comes next: the ts of
             the newest item (or the --after waited from, on timeout)
  truncated  as in "inbox", for the catch-up search
  items      as in "inbox", newest first; count and first_ts only cover
             the messages after --after
  timed_out  true when --timeout ran out with nothing new (items is [])
Exit codes: 0 for both, 1 on error, 130 when interrupted. It has no time
limit unless --timeout is given, and is not cut by the command timeout.

Like "watch", it needs browser session credentials or a classic token, and
like "inbox", search (for --after): not the token of a Slack app.`,
		Example: `  slackcli inbox wait
  slackcli inbox wait --after 1770165109.628379 --timeout 2h
  after=$(slackcli inbox wait --after "$after" | jq -r .after)`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := cmdutil.CheckMaxChars("--max-body-chars", maxBodyChars); err != nil {
				return err
			}
			if maxConversations < 1 {
				return fmt.Errorf("invalid --max-conversations value %d: must be a positive integer", maxConversations)
			}
			if timeout < 0 {
				return fmt.Errorf("--timeout must be a positive duration such as 30m or 2h, got %s", timeout)
			}
			cursor, err := cmdutil.ParseTS("--after", after)
			if err != nil {
				return err
			}
			catchUpFirst := cursor != ""
			if cursor == "" {
				micro := now().UnixMicro()
				cursor = strconv.FormatInt(micro/1e6, 10) + "." + fmt.Sprintf("%06d", micro%1e6)
			}
			return runWait(cmd.Context(), rt, waitArgs{
				workspace: workspace, cursor: cursor, catchUpFirst: catchUpFirst, timeout: timeout,
				opts: inbox.Options{
					MaxBodyChars: maxBodyChars, MaxConversations: maxConversations, NoThreads: noThreads,
					Names: channels.NewNames(channels.NamesOptions{}),
				},
			})
		},
	}
	clictx.AddWorkspaceFlag(cmd, &workspace)
	cmd.Flags().StringVar(&after, "after", "", `Only messages after this ts, what came since included (by default: from now on); pass the "after" of the previous result`)
	cmd.Flags().DurationVar(&timeout, "timeout", 0, "Give up after this long, such as 30m or 2h (0: wait until something comes or the command is interrupted)")
	cmd.Flags().IntVar(&maxBodyChars, "max-body-chars", inbox.DefaultMaxBodyChars, "Max text characters per item (-1 for unlimited)")
	cmd.Flags().IntVar(&maxConversations, "max-conversations", inbox.DefaultMaxConversations, "Max conversations and threads the catch-up search inspects")
	cmd.Flags().BoolVar(&noThreads, "no-threads", false, "Leave out the new replies in threads you posted in (kind thread)")
	return clictx.NoTimeout(cmd)
}

type waitArgs struct {
	workspace    string
	cursor       string
	catchUpFirst bool
	timeout      time.Duration
	opts         inbox.Options
}

func runWait(parent context.Context, rt *clictx.Context, a waitArgs) error {
	// ctx ends everything: an interrupt, or --timeout.
	ctx, cancel := context.WithCancelCause(parent)
	defer cancel(nil)
	// Read once, here: the goroutines below are not all waited for.
	settle, clockStep, indexLag, retryDelay := settle, clockStep, indexLag, retryDelay
	if a.timeout > 0 {
		// The wall clock (Round(0) drops the monotonic reading, which
		// stands still while a Mac sleeps): a 2h timeout is 2h of the day.
		deadline := now().Round(0).Add(a.timeout)
		go func() {
			t := time.NewTicker(min(clockStep, a.timeout))
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					if !now().Round(0).Before(deadline) {
						cancel(errTimedOut)
						return
					}
				}
			}
		}()
	}

	ws := rt.EffectiveWorkspace(a.workspace)
	results := &found{items: map[string]inbox.Item{}}
	// stream only closes the connection, settle after the first item: a
	// message being judged and a catch-up under way still finish, with
	// ctx.
	stream, stopStream := context.WithCancel(ctx)
	defer stopStream()
	results.onFirst = func() { time.AfterFunc(settle, stopStream) }

	var (
		mu sync.Mutex
		// lastHello is when the connection was last (re)opened, and
		// lastCatchUp when the last catch-up that succeeded started: the
		// messages before lastCatchUp-indexLag are covered by the search,
		// those after lastHello by the stream.
		lastHello, lastCatchUp time.Time
		// missed is set when a message could not be judged.
		missed bool
	)
	var catchUps sync.WaitGroup
	// catchUp looks for what the stream may have missed, after delay. A
	// transient failure is tried again later while the stream is open;
	// once it is closed, the final catch-up covers it.
	catchUp := func(w *inbox.Waiter, delay time.Duration) {
		catchUps.Go(func() {
			for {
				if delay > 0 {
					t := time.NewTimer(delay)
					select {
					case <-stream.Done():
						t.Stop()
						return
					case <-t.C:
					}
				}
				start := now()
				res, err := w.CatchUp(ctx)
				switch {
				case err == nil:
					mu.Lock()
					if start.After(lastCatchUp) {
						lastCatchUp = start
					}
					mu.Unlock()
					results.add(res.Items, res.Truncated)
					return
				case ctx.Err() != nil:
					return
				case slackapi.IsTransient(err):
					if stream.Err() != nil {
						return
					}
					delay = min(max(2*delay, retryDelay), maxRetryDelay)
					rt.Hintf("Cannot look for what came meanwhile (%v): trying again in %s.", err, delay)
				default:
					results.fail(err)
					stopStream()
					return
				}
			}
		})
	}

	var (
		me          string
		w           *inbox.Waiter
		connections int
	)
	_, err := clictx.WithAutoRefresh(stream, rt, func() (struct{}, error) {
		r, err := rt.Client(stream, ws)
		if err != nil {
			return struct{}{}, err
		}
		w, err = inbox.NewWaiter(stream, r.Client, a.cursor, a.opts)
		if err != nil {
			return struct{}{}, err
		}
		me = w.Me()
		opts := streamOptions
		opts.OnConnect = func(gap time.Duration) {
			connections++
			first := connections == 1
			mu.Lock()
			lastHello = now()
			mu.Unlock()
			switch {
			case gap > 0:
				rt.Hintf("Reconnected after %s: looking for what came meanwhile.", gap.Round(time.Second))
			case first:
				rt.Hintf("Connected to %s: waiting for the inbox.", cmp.Or(r.WorkspaceURL, "Slack"))
			}
			if !first {
				w.Reconnected()
			}
			// The first connection of a run without --after has next to
			// nothing to catch up on (the final catch-up covers it); every
			// other one may have missed messages (a reconnection, or a
			// new stream after the credentials were refreshed).
			if first && !a.catchUpFirst {
				return
			}
			catchUp(w, 0)
		}
		opts.OnDisconnect = func(err error) { rt.Hintf("Connection lost (%v): reconnecting.", err) }
		err = rtm.Stream(stream, r.Client, opts, func(raw map[string]any) error {
			item, err := w.Message(ctx, raw)
			switch {
			case err == nil:
			case ctx.Err() == nil && slackapi.IsTransient(err):
				rt.Hintf("Cannot judge a message (%v): looking for it again in %s.", err, retryDelay)
				mu.Lock()
				missed = true
				mu.Unlock()
				catchUp(w, retryDelay)
				return nil
			default:
				return err
			}
			if item != nil {
				results.add([]inbox.Item{*item}, false)
			}
			return nil
		})
		return struct{}{}, err
	})
	catchUps.Wait()

	items := results.list()
	// covered is whether everything after --after was looked at: else the
	// cursor printed stays at --after, so that the next run looks again.
	covered := results.err == nil
	if len(items) > 0 && covered && w != nil {
		// The search index lags behind: what came just before the
		// connection opened may not have been in it when the last
		// catch-up ran. One more, late enough, before moving the cursor.
		mu.Lock()
		hello, need := lastHello, missed || lastCatchUp.Before(lastHello.Add(indexLag))
		mu.Unlock()
		if need {
			covered = false
			t := time.NewTimer(time.Until(hello.Add(indexLag)))
			select {
			case <-ctx.Done():
				t.Stop()
			case <-t.C:
				res, err := w.CatchUp(ctx)
				if err == nil {
					results.add(res.Items, res.Truncated)
					covered = true
				} else {
					results.err = err
				}
			}
			items = results.list()
		}
	}
	if len(items) > 0 {
		after := items[0].TS
		if !covered {
			after = a.cursor
			reason := "the command was stopped first"
			if results.err != nil {
				reason = results.err.Error()
			}
			rt.ResultHintf("Warning: not everything since %s could be looked at (%s): \"after\" is left at it, so that the next run looks again.", a.cursor, reason)
		}
		return cmdutil.PrintList(rt, waitResult{UserID: me, After: after, Truncated: results.truncated, Items: items}, "items")
	}
	if results.err != nil {
		return results.err
	}
	if errors.Is(context.Cause(ctx), errTimedOut) {
		rt.ResultHintf("Nothing new came within %s: to wait again, run the same command with --after %s.", a.timeout, a.cursor)
		return cmdutil.PrintList(rt, waitResult{UserID: me, After: a.cursor, TimedOut: true}, "items")
	}
	if parent.Err() != nil {
		return parent.Err()
	}
	return err
}
