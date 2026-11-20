// Package unreads reports the conversations with unread messages, the way
// the Slack client fills its sidebar (client.counts), with the unread
// messages themselves.
//
// client.counts is an internal Slack method, which only accepts browser
// session credentials. On an Enterprise Grid that restricts it
// (team_is_restricted), users.counts is read instead.
package unreads

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/maelvls/slackcli/internal/appinfo"
	"github.com/maelvls/slackcli/internal/jsonx"
	"github.com/maelvls/slackcli/internal/slack/channels"
	"github.com/maelvls/slackcli/internal/slack/enrich"
	"github.com/maelvls/slackcli/internal/slack/messages"
	"github.com/maelvls/slackcli/internal/slack/slacktext"
	"github.com/maelvls/slackcli/internal/slackapi"
)

const (
	// DefaultMaxMessages is the number of unread messages fetched per
	// conversation when Options.MaxMessages is 0.
	DefaultMaxMessages = 10
	// MaxMessagesLimit is the most conversations.history returns at once.
	MaxMessagesLimit = 999
	// DefaultMaxBodyChars is the content length limit when
	// Options.MaxBodyChars is 0: the same as everywhere else.
	DefaultMaxBodyChars = messages.DefaultMaxBodyChars

	// fetchConcurrency bounds the per-conversation lookups: unbounded, a
	// user with hundreds of unread conversations would fire them all at
	// once and get rate limited.
	fetchConcurrency = 8
)

// Conversation types, as reported in Channel.ChannelType.
const (
	TypeChannel = "channel"
	TypeDM      = "dm"
	TypeMPIM    = "mpim"
)

// Options tunes Fetch.
type Options struct {
	// CountsOnly skips fetching the unread messages.
	CountsOnly bool
	// MaxMessages is the number of unread messages fetched per
	// conversation, system messages included; 0 means DefaultMaxMessages.
	// When there are more, Slack answers the oldest ones (those right
	// after the read cursor).
	MaxMessages int
	// MaxBodyChars truncates message content to that many characters; 0
	// means DefaultMaxBodyChars, a negative value means no limit.
	MaxBodyChars int
	// IncludeSystem keeps joins, leaves, topic changes and the like.
	IncludeSystem bool
}

// Author is who posted a message.
type Author struct {
	UserID string `json:"user_id,omitempty"`
	BotID  string `json:"bot_id,omitempty"`
}

// Message is one unread message.
type Message struct {
	TS string `json:"ts"`
	// Time is TS as a local date and time (messages.FormatTime).
	Time   string  `json:"time,omitempty"`
	Author *Author `json:"author,omitempty"`
	// Content is the message rendered as Markdown, possibly truncated.
	Content    string `json:"content,omitempty"`
	ThreadTS   string `json:"thread_ts,omitempty"`
	ReplyCount int    `json:"reply_count,omitempty"`
}

// Channel is a conversation with unread messages.
type Channel struct {
	ChannelID   string `json:"channel_id"`
	ChannelName string `json:"channel_name,omitempty"`
	// ChannelType is TypeChannel (public or private), TypeDM or TypeMPIM.
	ChannelType string `json:"channel_type"`
	// UnreadCount is the number of unread messages, hidden system
	// messages included: Slack's count when it gives one (client.counts
	// usually only says "has unreads"), else the number of unread messages
	// fetched. It is nil when unknown (no count from Slack, and no message
	// fetched), and never 0: every Channel has unreads.
	UnreadCount *int `json:"unread_count,omitempty"`
	// UnreadCountIsLowerBound is true when UnreadCount only counts the
	// messages fetched and Slack has more.
	UnreadCountIsLowerBound bool `json:"unread_count_is_lower_bound,omitempty"`
	MentionCount            int  `json:"mention_count"`
	// Messages are the unread messages fetched, oldest first, without the
	// system messages unless Options.IncludeSystem.
	Messages []Message `json:"messages,omitempty"`
	// SystemMessagesHidden is the number of fetched unread messages left
	// out of Messages for being system messages (joins, leaves, topic
	// changes …).
	SystemMessagesHidden int `json:"system_messages_hidden,omitempty"`
	// MessagesTruncated is true when Slack has more unread messages than
	// the Options.MaxMessages that were fetched.
	MessagesTruncated bool `json:"messages_truncated,omitempty"`
	// MessagesError is Slack's error code when the unread messages could
	// not be fetched ("ratelimited" when Slack was rate limiting the
	// history calls: see Result.Warnings), or NoReadCursorCode.
	MessagesError string `json:"messages_error,omitempty"`
}

// Threads summarises unread thread replies.
type Threads struct {
	HasUnreads   bool `json:"has_unreads"`
	MentionCount int  `json:"mention_count"`
}

// Result is what `unreads` prints.
type Result struct {
	// Channels are sorted by mention count, then unread count, descending.
	Channels []Channel `json:"channels"`
	// Threads is nil when no thread has unread replies, and when Slack
	// does not say (the users.counts fallback of Fetch).
	Threads *Threads `json:"threads,omitempty"`
	// Warnings say what is missing from Channels because Slack rate
	// limited the lookups (names, messages), for the caller to print in
	// the "warnings" of the result.
	Warnings []string `json:"-"`
}

// entry is a conversation with unreads, as listed by client.counts or
// users.counts.
type entry struct {
	id   string
	kind string
	// lastRead is the read cursor; users.counts does not give it (see
	// Fetch).
	lastRead string
	count    *int
	mentions int
}

// NoReadCursorCode is the Channel.MessagesError of a conversation listed
// by users.counts for which Slack gave no read cursor: its unread messages
// cannot be told from the others, so none is fetched.
const NoReadCursorCode = "no_read_cursor"

// errNotLookedUp is what lookup answers once Slack has rate limited the
// name lookups.
var errNotLookedUp = errors.New("not looked up")

// Fetch returns every conversation with unread messages, with its name
// and, unless opts.CountsOnly, its unread messages.
//
// It makes one client.counts call, then per unread conversation one or
// two calls for its name and one for its messages, at most
// fetchConcurrency at a time. A conversation whose messages cannot be
// read is still listed, with Channel.MessagesError set.
//
// On an Enterprise Grid where client.counts answers team_is_restricted
// (channels.IsRestricted), the conversations come from users.counts
// instead: those unread or mentioned, without the archived ones and the
// muted ones nobody mentioned the caller in (client.counts lists the
// muted ones with unreads, so this list can be shorter). users.counts has
// no read cursor, so it is taken from the conversations.info call that
// gives the name, and it says nothing about threads: Result.Threads is
// nil.
//
// Only client.counts, a cancelled context and dead credentials fail the
// listing. Names and messages are enrichments (see package enrich): once
// Slack rate limits one kind of lookup, no more of that kind are made, the
// conversations concerned are listed without a name or with
// MessagesError "ratelimited", and Result.Warnings says so.
func Fetch(ctx context.Context, client *slackapi.Client, opts Options) (Result, error) {
	if !client.Auth().IsBrowser() {
		return Result{}, fmt.Errorf(`unread counts come from Slack's internal client.counts API, which only works with browser session credentials, not with a bot or user token (xoxb-/xoxp-). `+
			`Import browser credentials with "%s auth import-desktop" (also: import-chrome, import-brave, import-firefox), or set SLACK_TOKEN to an xoxc- token together with SLACK_COOKIE_D`, appinfo.Name)
	}
	if opts.MaxMessages <= 0 {
		opts.MaxMessages = DefaultMaxMessages
	}
	opts.MaxMessages = min(opts.MaxMessages, MaxMessagesLimit)
	if opts.MaxBodyChars == 0 {
		opts.MaxBodyChars = DefaultMaxBodyChars
	}

	var entries []entry
	resp, err := client.API(ctx, "client.counts", map[string]any{"thread_count_by_channel": true})
	// viaUsersCounts: the entries have no read cursor.
	viaUsersCounts := false
	switch {
	case err == nil:
		for _, group := range []struct{ key, kind string }{{"channels", TypeChannel}, {"mpims", TypeMPIM}, {"ims", TypeDM}} {
			for _, raw := range jsonx.Maps(resp[group.key]) {
				id := jsonx.Str(raw["id"])
				if id == "" || !jsonx.Bool(raw["has_unreads"]) {
					continue
				}
				e := entry{id: id, kind: group.kind, lastRead: jsonx.Str(raw["last_read"])}
				e.mentions, _ = jsonx.Int(raw["mention_count"])
				for _, key := range []string{"unread_count_display", "unread_count"} {
					// A count of 0 next to has_unreads says nothing.
					if n, ok := jsonx.Int(raw[key]); ok && n > 0 {
						e.count = &n
						break
					}
				}
				entries = append(entries, e)
			}
		}
	case channels.IsRestricted(err):
		viaUsersCounts = true
		counts, err := channels.UsersCounts(ctx, client)
		if err != nil {
			return Result{}, fmt.Errorf("fetching unread counts: %w", err)
		}
		for _, c := range counts {
			if !c.HasUnreads() {
				continue
			}
			// The kinds of users.counts are those of ChannelType.
			e := entry{id: c.ID, kind: c.Kind, mentions: c.MentionCount}
			if n := c.UnreadCount; n > 0 {
				e.count = &n
			}
			entries = append(entries, e)
		}
	default:
		return Result{}, fmt.Errorf("fetching unread counts: %w", err)
	}

	result := Result{Channels: make([]Channel, len(entries))}
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	var wg sync.WaitGroup
	sem := make(chan struct{}, fetchConcurrency)
	var namesLimited, messagesLimited enrich.Gate
	run := func(task func()) {
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			if ctx.Err() == nil {
				task()
			}
		})
	}
	// lookup fills the name of ch; the read cursor it returns is only
	// known when the error is nil or info.LastRead is set.
	lookup := func(e entry, ch *Channel) (channels.Info, error) {
		if namesLimited.Closed() {
			return channels.Info{}, errNotLookedUp
		}
		info, err := channels.Lookup(ctx, client, e.id)
		ch.ChannelName = info.Name
		switch {
		case err == nil:
		case enrich.MustAbort(ctx, err):
			cancel(fmt.Errorf("looking up the name of %s: %w", e.id, err))
		default:
			namesLimited.Failed(err)
		}
		return info, err
	}
	load := func(e entry, ch *Channel) {
		if messagesLimited.Closed() {
			ch.MessagesError = enrich.RateLimitedCode
			return
		}
		err := fetchMessages(ctx, client, e, opts, ch)
		switch {
		case err == nil:
		case enrich.MustAbort(ctx, err):
			cancel(fmt.Errorf("fetching unread messages of %s: %w", e.id, err))
		case messagesLimited.Failed(err):
			ch.MessagesError = enrich.RateLimitedCode
		default:
			ch.MessagesError = errorCode(err)
		}
	}
	// noCursor counts the conversations whose messages were not fetched
	// for want of a read cursor.
	var noCursor atomic.Int32
	for i, e := range entries {
		ch := &result.Channels[i]
		*ch = Channel{ChannelID: e.id, ChannelType: e.kind, UnreadCount: e.count, MentionCount: e.mentions}
		if !viaUsersCounts {
			run(func() { _, _ = lookup(e, ch) })
			if !opts.CountsOnly {
				run(func() { load(e, ch) })
			}
			continue
		}
		// The read cursor comes with the name: one task does both.
		run(func() {
			info, err := lookup(e, ch)
			if opts.CountsOnly || ctx.Err() != nil {
				return
			}
			if info.LastRead == "" {
				// Without the cursor, the newest messages would be taken
				// for the unread ones.
				switch {
				case err == nil:
					// conversations.info answered without last_read.
					ch.MessagesError = NoReadCursorCode
				case errors.Is(err, errNotLookedUp) || enrich.IsRateLimit(err):
					ch.MessagesError = enrich.RateLimitedCode
					noCursor.Add(1)
				default:
					ch.MessagesError = errorCode(err)
				}
				return
			}
			e.lastRead = info.LastRead
			load(e, ch)
		})
	}
	wg.Wait()
	if err := context.Cause(ctx); err != nil {
		return Result{}, err
	}
	if n, err := messagesLimited.Skipped(); n > 0 {
		result.Warnings = append(result.Warnings, fmt.Sprintf(
			`the unread messages of %d of %d conversations were not fetched (%v): they are listed with "messages_error": %q; %s`,
			n, len(entries), enrich.Cause(err), enrich.RateLimitedCode, enrich.Hint()))
	}
	if n := noCursor.Load(); n > 0 {
		_, err := namesLimited.Skipped()
		result.Warnings = append(result.Warnings, fmt.Sprintf(
			`the unread messages of %d of %d conversations were not fetched, as how far they were read could not be looked up (%v): they are listed with "messages_error": %q; %s`,
			n, len(entries), enrich.Cause(err), enrich.RateLimitedCode, enrich.Hint()))
	}
	if n, err := namesLimited.Skipped(); n > 0 {
		result.Warnings = append(result.Warnings, fmt.Sprintf(
			"the names of %d of %d conversations were not looked up (%v): they are listed by ID only; %s",
			n, len(entries), enrich.Cause(err), enrich.Hint()))
	}

	slices.SortStableFunc(result.Channels, func(a, b Channel) int {
		if a.MentionCount != b.MentionCount {
			return b.MentionCount - a.MentionCount
		}
		return unreadCount(b) - unreadCount(a)
	})
	if threads := jsonx.Map(resp["threads"]); jsonx.Bool(threads["has_unreads"]) {
		result.Threads = &Threads{HasUnreads: true}
		result.Threads.MentionCount, _ = jsonx.Int(threads["mention_count"])
	}
	return result, nil
}

func unreadCount(c Channel) int {
	if c.UnreadCount == nil {
		return 0
	}
	return *c.UnreadCount
}

func errorCode(err error) string {
	if code := slackapi.ErrorCode(err); code != "" {
		return code
	}
	return err.Error()
}

// fetchMessages fills ch with the messages newer than the read cursor.
func fetchMessages(ctx context.Context, client *slackapi.Client, e entry, opts Options, ch *Channel) error {
	params := map[string]any{"channel": e.id, "limit": opts.MaxMessages, "inclusive": false}
	// With "oldest" and no "latest", conversations.history pages forward
	// from the read cursor: a truncated page holds the oldest unread
	// messages. Without a read cursor (a conversation never opened)
	// everything is unread, and the newest messages are fetched.
	if e.lastRead != "" {
		params["oldest"] = e.lastRead
	}
	history, err := client.API(ctx, "conversations.history", params)
	if err != nil {
		return err
	}
	fetched := jsonx.Maps(history["messages"])
	ch.MessagesTruncated = jsonx.Bool(history["has_more"])
	ch.Messages = []Message{}
	for _, m := range fetched {
		if !opts.IncludeSystem && messages.IsSystem(jsonx.Str(m["subtype"])) {
			ch.SystemMessagesHidden++
			continue
		}
		msg := Message{
			TS:       jsonx.Str(m["ts"]),
			Time:     messages.FormatTime(jsonx.Str(m["ts"]), nil),
			Content:  truncate(slacktext.RenderMessageContent(m), opts.MaxBodyChars),
			ThreadTS: jsonx.Str(m["thread_ts"]),
		}
		msg.ReplyCount, _ = jsonx.Int(m["reply_count"])
		if user, bot := jsonx.Str(m["user"]), jsonx.Str(m["bot_id"]); user != "" || bot != "" {
			msg.Author = &Author{UserID: user, BotID: bot}
		}
		ch.Messages = append(ch.Messages, msg)
	}
	slices.SortStableFunc(ch.Messages, func(a, b Message) int { return messages.CompareTS(a.TS, b.TS) })
	// What was fetched is the only count there is, unless Slack gave a
	// (larger) one. Hidden system messages count: they are what makes the
	// conversation unread. Nothing fetched (unread thread replies only,
	// deleted messages …) leaves the count unknown rather than 0.
	if n := len(fetched); n > 0 && (ch.UnreadCount == nil || *ch.UnreadCount < n) {
		ch.UnreadCount = &n
		ch.UnreadCountIsLowerBound = ch.MessagesTruncated
	}
	return nil
}

// truncate cuts s to maxChars characters (negative: no limit), on a
// character boundary, and marks the cut.
func truncate(s string, maxChars int) string {
	if maxChars < 0 {
		return s
	}
	runes := []rune(s)
	if len(runes) <= maxChars {
		return s
	}
	return string(runes[:maxChars]) + "\n..."
}
