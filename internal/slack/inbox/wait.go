package inbox

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/maelvls/slackcli/internal/jsonx"
	"github.com/maelvls/slackcli/internal/slack/channels"
	"github.com/maelvls/slackcli/internal/slack/enrich"
	"github.com/maelvls/slackcli/internal/slackapi"
)

// waitHiddenSubtypes are the message events that are not a new message: an
// edit, a deletion, and the notice that a thread got a reply (the reply
// itself is an event of its own).
var waitHiddenSubtypes = []string{"message_changed", "message_deleted", "message_replied"}

// Waiter tells, as messages arrive, which of them put something in the
// inbox: it judges each one by the rules of Fetch, reading the
// conversation or the thread it is in, but only the messages strictly
// after a timestamp count.
//
// Message is not safe for concurrent use; CatchUp may run alongside it.
type Waiter struct {
	f    *fetcher
	opts Options

	// threads holds, by "channel:root", whether the caller posted in that
	// thread of a channel, once known: a reply in a thread known not to be
	// the caller's is skipped without a call. The caller's own messages,
	// which arrive too, keep it current.
	threads map[string]bool
	// kinds holds the kind of conversation (KindDM, KindMPIM or
	// "channel") of the channels whose events did not say.
	kinds map[string]string
	// names holds the channel names looked up, for as long as the Waiter
	// lives: the on-disk cache (Options.Names) is only asked the first
	// time a channel is seen.
	names map[string]string
}

// NewWaiter returns a Waiter for which only the messages strictly after
// the Slack timestamp after count. opts.Since is ignored: it is after.
func NewWaiter(ctx context.Context, client *slackapi.Client, after string, opts Options) (*Waiter, error) {
	sec := tsNum(after)
	if sec <= 0 {
		return nil, fmt.Errorf("invalid Slack timestamp %q", after)
	}
	// The window the searches and history reads start from; inWindow
	// then compares the timestamps exactly.
	opts.Since = time.UnixMicro(int64(sec * 1e6))
	f, err := newFetcher(ctx, client, opts)
	if err != nil {
		return nil, err
	}
	f.after = after
	return &Waiter{f: f, opts: opts, threads: map[string]bool{}, kinds: map[string]string{}, names: map[string]string{}}, nil
}

// Reconnected forgets which threads the caller was known not to have
// posted in: they may have posted while the stream was down. It must not
// run alongside Message (call it from rtm.Options.OnConnect).
func (w *Waiter) Reconnected() {
	for key, posted := range w.threads {
		if !posted {
			delete(w.threads, key)
		}
	}
}

// Me is the caller's user ID.
func (w *Waiter) Me() string { return w.f.me }

// CatchUp is Fetch for the messages after the timestamp of the Waiter:
// what arrived while nothing was listening. It goes through Slack's
// search, whose index can lag a few seconds behind.
func (w *Waiter) CatchUp(ctx context.Context) (Result, error) {
	return w.f.fetch(ctx, w.opts)
}

// Message judges a message event of the real-time API (package rtm) and
// returns the item it makes, nil when it makes none: a message of the
// caller's, of a bot, a system message, a channel message that neither
// mentions the caller nor replies in a thread they posted in, or one the
// caller has since answered.
func (w *Waiter) Message(ctx context.Context, raw map[string]any) (*Item, error) {
	if jsonx.Str(raw["type"]) != "message" || jsonx.Bool(raw["hidden"]) {
		return nil, nil
	}
	if slices.Contains(waitHiddenSubtypes, jsonx.Str(raw["subtype"])) || !isHuman(raw) {
		return nil, nil
	}
	channelID, ts := jsonx.Str(raw["channel"]), jsonx.Str(raw["ts"])
	if channelID == "" || ts == "" {
		return nil, nil
	}
	threadTS := jsonx.Str(raw["thread_ts"])
	if threadTS == ts {
		threadTS = ""
	}
	me := w.f.me
	if jsonx.Str(raw["user"]) == me {
		// The caller's own message: the thread it is in, or starts, is
		// theirs.
		w.threads[channelID+":"+cmp.Or(threadTS, ts)] = true
		return nil, nil
	}
	if !isFromOther(raw, me) || !w.f.inWindow(ts) {
		return nil, nil
	}

	kind, err := w.kind(ctx, channelID, raw)
	if err != nil {
		return nil, w.skip(ctx, err)
	}
	c := &candidate{channelID: channelID, convKind: kind, root: threadTS}
	key := ""
	switch kind {
	case KindDM:
		c.imUser = jsonx.Str(raw["user"])
	case KindMPIM:
		if c.channelName, err = w.name(ctx, channelID); err != nil {
			return nil, err
		}
	default:
		c.root = cmp.Or(threadTS, ts)
		key = channelID + ":" + c.root
		if strings.Contains(jsonx.Str(raw["text"]), "<@"+me+">") {
			c.mentionTS = []string{ts}
		} else {
			// Without a mention, only a reply in a thread the caller
			// posted in waits on them.
			if threadTS == "" || w.opts.NoThreads {
				return nil, nil
			}
			if posted, known := w.threads[key]; known && !posted {
				return nil, nil
			}
		}
		if c.channelName, err = w.name(ctx, channelID); err != nil {
			return nil, err
		}
	}

	d, err := w.f.evaluate(ctx, c)
	if err != nil {
		return nil, w.skip(ctx, err)
	}
	if key != "" {
		w.threads[key] = w.threads[key] || c.posted
	}
	if d == nil {
		return nil, nil
	}
	items, err := w.f.finish(ctx, []*draft{d})
	if err != nil {
		return nil, err
	}
	return &items[0], nil
}

// skip returns nil for an error that only says the conversation cannot be
// read (skippableCodes), err otherwise.
func (w *Waiter) skip(ctx context.Context, err error) error {
	if !enrich.MustAbortRequired(ctx, err) && slackapi.IsErrorCode(err, skippableCodes...) {
		return nil
	}
	return err
}

// kind returns the kind of conversation of a message event: what the
// event says, else what its ID says (D… is a DM), else what
// conversations.info says.
func (w *Waiter) kind(ctx context.Context, channelID string, raw map[string]any) (string, error) {
	switch jsonx.Str(raw["channel_type"]) {
	case "im":
		return KindDM, nil
	case "mpim":
		return KindMPIM, nil
	case "channel", "group":
		return convChannel, nil
	}
	switch {
	case strings.HasPrefix(channelID, "D"):
		return KindDM, nil
	case strings.HasPrefix(channelID, "C"):
		return convChannel, nil
	}
	if kind, ok := w.kinds[channelID]; ok {
		return kind, nil
	}
	ch, err := channels.FetchInfo(ctx, w.f.client, channelID)
	if err != nil {
		return "", err
	}
	kind := convChannel
	switch {
	case jsonx.Bool(ch["is_im"]):
		kind = KindDM
	case jsonx.Bool(ch["is_mpim"]):
		kind = KindMPIM
	}
	w.kinds[channelID] = kind
	if name := jsonx.Str(ch["name"]); name != "" {
		w.names[channelID] = name
	}
	return kind, nil
}

// name returns the name of a channel. A name is an enrichment: the error
// is only that of a lookup that must end the command.
func (w *Waiter) name(ctx context.Context, channelID string) (string, error) {
	if name, ok := w.names[channelID]; ok {
		return name, nil
	}
	name, err := w.opts.Names.Lookup(ctx, w.f.client, channelID)
	switch {
	case enrich.MustAbort(ctx, err):
		return "", err
	case err == nil:
		w.names[channelID] = name
	case enrich.IsRateLimit(err):
		// Better a stale name than none; it is looked up again next time.
		name = w.opts.Names.Stale(w.f.client, channelID)
	}
	return name, nil
}
