// Package inbox reports what is waiting on the caller: the DMs and group
// DMs whose newest message is someone else's, the @mentions the caller has
// not answered, and the new replies in threads the caller posted in.
//
// Where package unreads is about the read state, this is about who is
// waiting for an answer: a message that was read and not answered is
// listed, an unread one that needs no answer from the caller is not.
// Nothing is marked as read.
package inbox

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/maelvls/slackcli/internal/jsonx"
	"github.com/maelvls/slackcli/internal/slack/channels"
	"github.com/maelvls/slackcli/internal/slack/enrich"
	"github.com/maelvls/slackcli/internal/slack/messages"
	"github.com/maelvls/slackcli/internal/slack/slacktext"
	"github.com/maelvls/slackcli/internal/slack/slackurl"
	"github.com/maelvls/slackcli/internal/slack/users"
	"github.com/maelvls/slackcli/internal/slackapi"
	"github.com/maelvls/slackcli/internal/slackid"
)

const (
	// DefaultSince is the window looked at when --since is not given.
	DefaultSince = "3d"
	// DefaultMaxBodyChars is the text length limit when
	// Options.MaxBodyChars is 0.
	DefaultMaxBodyChars = 300
	// DefaultMaxConversations is the number of conversations and threads
	// inspected when Options.MaxConversations is 0.
	DefaultMaxConversations = 60

	// concurrency is the number of searches and conversation reads in
	// flight. The names are looked up once those are done, by package
	// users, which has a bound of its own (5).
	concurrency = 4

	searchPages    = 2
	searchPageSize = 100
	// pageSize and maxPages bound how much of one conversation or thread
	// is read: 1000 messages. Past that, the result is marked truncated.
	pageSize = 200
	maxPages = 5
)

// Kinds of Item.
const (
	KindDM      = "dm"
	KindMPIM    = "mpim"
	KindMention = "mention"
	KindThread  = "thread"
)

// Author is who wrote the newest message of an Item.
type Author struct {
	UserID string `json:"user_id"`
	// Name is their display name, else their handle, else their real name;
	// empty when the user could not be looked up.
	Name string `json:"name,omitempty"`
}

// Item is one conversation or thread waiting on the caller.
type Item struct {
	// Kind is KindDM, KindMPIM, KindMention or KindThread.
	Kind      string `json:"kind"`
	ChannelID string `json:"channel_id"`
	// ChannelName is the name of the channel; for a DM, the name of the
	// other user (their ID when it could not be looked up).
	ChannelName string `json:"channel_name,omitempty"`
	// ThreadTS is the thread root (the message itself for a top-level
	// mention); empty for the top level of a DM or group DM.
	ThreadTS string `json:"thread_ts,omitempty"`
	// TS is the newest message from someone else since the caller's last
	// message there; Author, Text and Permalink describe that message.
	TS string `json:"ts"`
	// FirstTS is the oldest such message, and Count their number.
	FirstTS string `json:"first_ts"`
	Count   int    `json:"count"`
	Author  Author `json:"author"`
	// Text is the message as Markdown. Like every blank string it is
	// pruned when printed, should a message render to nothing.
	Text      string `json:"text"`
	Permalink string `json:"permalink,omitempty"`
}

// Result is what `inbox` prints.
type Result struct {
	// UserID is the caller.
	UserID string `json:"user_id"`
	// Since is the start of the window, in UTC (RFC 3339 with
	// milliseconds).
	Since string `json:"since"`
	// Truncated is true when there was more to look at than was: a search
	// with more pages than were read, more candidates than
	// Options.MaxConversations, or a conversation or thread with more
	// messages in the window than are read (1000): an item may then
	// undercount, or be about something the caller answered since.
	Truncated bool `json:"truncated"`
	// Items are sorted by TS, newest first.
	Items []Item `json:"items"`
}

// Options tunes Fetch.
type Options struct {
	// Since is the start of the window (see ParseSince).
	Since time.Time
	// MaxBodyChars truncates Item.Text to that many characters; 0 means
	// DefaultMaxBodyChars, a negative value means no limit.
	MaxBodyChars int
	// MaxConversations caps the conversations and threads inspected, the
	// most recently active first; 0 means DefaultMaxConversations.
	MaxConversations int
	// NoThreads leaves out the items of kind thread.
	NoThreads bool
	// CacheDir overrides where the user cache lives (tests).
	CacheDir string
	// Names gives the names of the channels a Waiter looks up; nil looks
	// each one up.
	Names *channels.Names
}

// latestMargin is how far past the local clock fetcher.latest is put.
const latestMargin = 24 * time.Hour

// maxSince is the longest relative --since accepted: 100 years.
const maxSince = 100 * 365 * 24 * time.Hour

// skippableCodes are the Slack errors that only say that one candidate
// cannot be read with these credentials, or is gone: the caller has
// nothing to answer there, and the rest of the inbox stands. Any other
// error (internal_error, missing_scope …) would make an empty inbox a lie.
var skippableCodes = []string{
	"channel_not_found", // left, or a private conversation no longer visible
	"thread_not_found",  // the root was deleted
	"not_in_channel",    // found by the search in a public channel the caller is not in
	"is_archived",       // nothing can be answered there
	"access_denied",     // a conversation the organisation hides from this session
}

var relativeSinceRe = regexp.MustCompile(`^(\d+)([dh])$`)

// ParseSince parses the value of --since: "Nd" or "Nh" before now, or a
// day "YYYY-MM-DD", which starts at midnight in now's location: the local
// time zone for time.Now(), as for a day given to "message list --oldest".
func ParseSince(input string, now time.Time) (time.Time, error) {
	value := strings.TrimSpace(input)
	if m := relativeSinceRe.FindStringSubmatch(value); m != nil {
		unit := time.Hour
		if m[2] == "d" {
			unit = 24 * time.Hour
		}
		// Bounded well below what a Duration holds (292 years), which a
		// larger number would silently wrap around.
		if n, err := strconv.ParseInt(m[1], 10, 64); err == nil && n <= int64(maxSince/unit) {
			return now.Add(-time.Duration(n) * unit), nil
		}
	} else if t, err := time.ParseInLocation("2006-01-02", value, now.Location()); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf(`invalid --since value %q: expected a number of days or hours such as 3d or 12h, or a day such as 2026-01-31 (from 00:00 local time)`, input)
}

// candidate is a conversation or a thread that may be waiting on the
// caller, as found by the searches.
type candidate struct {
	channelID   string
	channelName string
	// imUser is the other user of a DM.
	imUser string
	// convKind is KindDM, KindMPIM or "channel".
	convKind string
	// root is the thread root; "" for the top level of a DM or group DM.
	root string
	// mentionTS are the messages of a channel that mention the caller.
	mentionTS []string
	// mine is true when the thread was found among the caller's own
	// thread posts.
	mine   bool
	newest float64
	// posted is set by evaluate: whether the caller wrote in what it read.
	posted bool
}

const convChannel = "channel"

// draft is an Item before the names are known.
type draft struct {
	item        Item
	imUser      string
	channelName string
}

type fetcher struct {
	client       *slackapi.Client
	sem          chan struct{}
	me           string
	sinceTS      float64
	oldest       string
	baseURL      string
	maxBodyChars int
	noThreads    bool
	// incomplete is set when a conversation or a thread had more messages
	// than were read.
	incomplete atomic.Bool
	// after, when set, is a Slack timestamp: only the messages strictly
	// after it count, instead of those from sinceTS on (see Waiter).
	after string
	// cacheDir is Options.CacheDir.
	cacheDir string
}

// latest is a Slack timestamp after every message there is: with it,
// conversations.history pages from the newest message back. It is a day
// ahead of the local clock, so that a clock running behind Slack's does
// not hide the newest messages; it is computed for each read, as a Waiter
// can run for days.
func latest() string {
	return strconv.FormatFloat(float64(time.Now().Add(latestMargin).UnixMicro())/1e6, 'f', 6, 64)
}

// inWindow reports whether a message of timestamp ts is in the window.
func (f *fetcher) inWindow(ts string) bool {
	if f.after != "" {
		return messages.CompareTS(ts, f.after) > 0
	}
	return tsNum(ts) >= f.sinceTS
}

// Fetch lists what is waiting on the caller since opts.Since.
//
// The candidates come from three searches (search.messages: what was sent
// to the caller, what mentions them, and their own thread posts); each
// candidate is then read (conversations.history for the top level of a DM
// or group DM, conversations.replies for a thread) to see whether it still
// waits: everything up to the caller's last message, reaction or, at the
// top level, thread reply counts as answered, and a later message from a
// person counts as waiting. Bots, Slackbot, system messages and the DM
// with oneself are ignored.
//
// The searches only go back to two days before opts.Since, the caller's
// own thread posts included: a thread whose last post by the caller is
// older than that is not found, however recent its replies, unless one of
// them mentions the caller.
//
// A candidate that cannot be read (skippableCodes: channel_not_found,
// thread_not_found …) is left out. Everything else fails the command,
// rate limits, missing scopes and Slack's own failures included: an inbox
// with holes would read as "nobody else is waiting". Names are an
// enrichment (see package enrich).
func Fetch(ctx context.Context, client *slackapi.Client, opts Options) (Result, error) {
	f, err := newFetcher(ctx, client, opts)
	if err != nil {
		return Result{}, err
	}
	return f.fetch(ctx, opts)
}

// newFetcher applies the defaults of opts and asks Slack who the caller is.
func newFetcher(ctx context.Context, client *slackapi.Client, opts Options) (*fetcher, error) {
	if opts.MaxBodyChars == 0 {
		opts.MaxBodyChars = DefaultMaxBodyChars
	}

	auth, err := client.API(ctx, "auth.test", nil)
	if err != nil {
		return nil, err
	}
	me := jsonx.Str(auth["user_id"])
	if me == "" {
		return nil, fmt.Errorf("auth.test returned no user_id")
	}
	sinceTS := float64(opts.Since.UnixMicro()) / 1e6
	f := &fetcher{
		client:       client,
		sem:          make(chan struct{}, concurrency),
		me:           me,
		sinceTS:      sinceTS,
		oldest:       strconv.FormatFloat(sinceTS, 'f', 6, 64),
		baseURL:      strings.TrimRight(cmp.Or(jsonx.Str(auth["url"]), client.WorkspaceURL()), "/"),
		maxBodyChars: opts.MaxBodyChars,
		noThreads:    opts.NoThreads,
		cacheDir:     opts.CacheDir,
	}
	return f, nil
}

// fetch runs the searches and judges each candidate they find.
func (f *fetcher) fetch(ctx context.Context, opts Options) (Result, error) {
	if opts.MaxConversations <= 0 {
		opts.MaxConversations = DefaultMaxConversations
	}
	me := f.me
	// Slack's after: is an exclusive day in the user's timezone: two days
	// back covers any timezone.
	after := opts.Since.UTC().Add(-48 * time.Hour).Format("2006-01-02")
	const toMe, mentions, mine = 0, 1, 2
	queries := []string{toMe: "to:<@" + me + ">", mentions: "<@" + me + ">"}
	if !opts.NoThreads {
		queries = append(queries, "is:thread from:<@"+me+">")
	}
	found := make([][]map[string]any, len(queries))
	capped := make([]bool, len(queries))
	err := each(ctx, len(queries), func(ctx context.Context, i int) (err error) {
		found[i], capped[i], err = f.search(ctx, queries[i]+" after:"+after)
		return err
	})
	if err != nil {
		return Result{}, err
	}

	var set candidates
	for how, matches := range found {
		for _, m := range matches {
			set.add(f, m, how == mentions, how == mine)
		}
	}
	// The most recently active first: they are the ones inspected.
	slices.SortStableFunc(set.list, func(a, b *candidate) int { return cmp.Compare(b.newest, a.newest) })
	inspected := set.list[:min(len(set.list), opts.MaxConversations)]
	truncated := len(inspected) < len(set.list) || slices.Contains(capped, true)

	evaluated := make([]*draft, len(inspected))
	err = each(ctx, len(inspected), func(ctx context.Context, i int) error {
		d, err := f.evaluate(ctx, inspected[i])
		switch {
		case err == nil:
			evaluated[i] = d
			return nil
		case !enrich.MustAbortRequired(ctx, err) && slackapi.IsErrorCode(err, skippableCodes...):
			return nil
		}
		return fmt.Errorf("reading %s: %w", inspected[i].channelID, err)
	})
	if err != nil {
		return Result{}, err
	}
	var drafts []*draft
	for _, d := range evaluated {
		if d != nil {
			drafts = append(drafts, d)
		}
	}
	items, err := f.finish(ctx, drafts)
	if err != nil {
		return Result{}, err
	}
	return Result{
		UserID:    me,
		Since:     opts.Since.UTC().Format("2006-01-02T15:04:05.000Z"),
		Truncated: truncated || f.incomplete.Load(),
		Items:     items,
	}, nil
}

// finish turns drafts into items, newest first, with the names of the
// users looked up.
func (f *fetcher) finish(ctx context.Context, drafts []*draft) ([]Item, error) {
	var ids []string
	for _, d := range drafts {
		ids = append(ids, d.item.Author.UserID, d.imUser)
		ids = append(ids, users.MentionedInContent(d.item.Text)...)
	}
	slices.SortStableFunc(drafts, func(a, b *draft) int { return cmp.Compare(tsNum(b.item.TS), tsNum(a.item.TS)) })

	byID, err := users.ResolveByID(ctx, f.client, ids, users.ResolveOptions{CacheDir: f.cacheDir})
	if err != nil {
		return nil, err
	}
	name := func(id string) string { return users.Label(byID[id]) }
	items := make([]Item, len(drafts))
	for i, d := range drafts {
		item := d.item
		item.ChannelName = d.channelName
		if d.imUser != "" {
			item.ChannelName = cmp.Or(name(d.imUser), d.imUser)
		}
		item.Author.Name = name(item.Author.UserID)
		item.Text = users.NameContent(item.Text, byID)
		items[i] = item
	}
	return items, nil
}

// each runs fn(ctx, i) for every i below n, all at once (fetcher.api
// bounds the calls in flight), and returns the first error, after which
// the context given to the others is cancelled.
func each(ctx context.Context, n int, fn func(ctx context.Context, i int) error) error {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			if ctx.Err() != nil {
				return
			}
			if err := fn(ctx, i); err != nil {
				cancel(err)
			}
		})
	}
	wg.Wait()
	return context.Cause(ctx)
}

// api calls a method once one of the concurrency slots is free.
func (f *fetcher) api(ctx context.Context, method string, params map[string]any) (map[string]any, error) {
	select {
	case f.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, context.Cause(ctx)
	}
	defer func() { <-f.sem }()
	return f.client.API(ctx, method, params)
}

// search returns the matches of a query, newest first, from at most
// searchPages pages; capped is true when Slack has more pages.
func (f *fetcher) search(ctx context.Context, query string) (matches []map[string]any, capped bool, err error) {
	for page := 1; page <= searchPages; page++ {
		resp, err := f.api(ctx, "search.messages", map[string]any{
			"query":    query,
			"count":    searchPageSize,
			"page":     page,
			"sort":     "timestamp",
			"sort_dir": "desc",
		})
		if err != nil {
			if slackapi.IsErrorCode(err, "not_allowed_token_type") {
				return nil, false, fmt.Errorf("the inbox is built from Slack's search API, which a bot token (xoxb-) cannot use: use a user token or browser session credentials: %w", err)
			}
			return nil, false, fmt.Errorf("searching messages for %q: %w", query, err)
		}
		section := jsonx.Map(resp["messages"])
		matches = append(matches, jsonx.Maps(section["matches"])...)
		// Slack describes the pages in "paging" or, on newer responses, in
		// "pagination".
		pages := 1
		for _, path := range [][2]string{{"paging", "pages"}, {"pagination", "page_count"}} {
			if n, ok := jsonx.Int(jsonx.Get(section, path[0], path[1])); ok && n > 0 {
				pages = n
				break
			}
		}
		if page >= pages {
			return matches, false, nil
		}
	}
	return matches, true, nil
}

// candidates are the candidates found, in the order they were first seen,
// one per conversation (top level of a DM or group DM) or thread.
type candidates struct {
	list  []*candidate
	byKey map[string]*candidate
}

// add records a search match: a message that mentions the caller
// (mention), one of the caller's own thread posts (mine), or a message
// sent to the caller.
func (s *candidates) add(f *fetcher, m map[string]any, mention, mine bool) {
	ch := jsonx.Map(m["channel"])
	channelID, ts := jsonx.Str(ch["id"]), jsonx.Str(m["ts"])
	if channelID == "" || ts == "" {
		return
	}
	if !mine && (!f.inWindow(ts) || jsonx.Str(m["user"]) == f.me) {
		return
	}
	name := jsonx.Str(ch["name"])
	convKind, imUser := convChannel, ""
	switch {
	case jsonx.Bool(ch["is_im"]):
		// The search names a DM after the ID of the other user.
		convKind, imUser = KindDM, jsonx.Str(ch["user"])
		if imUser == "" && slackid.IsUserID(name) {
			imUser = name
		}
		if imUser == f.me {
			return // the DM with oneself
		}
		name = ""
	case jsonx.Bool(ch["is_mpim"]):
		convKind = KindMPIM
	}
	threadTS := threadTSFromPermalink(jsonx.Str(m["permalink"]))
	root := ""
	switch {
	case mine || convKind == convChannel:
		root = cmp.Or(threadTS, ts)
	case threadTS != ts:
		// DM or group DM: the top-level messages are judged together, per
		// conversation; the replies per thread.
		root = threadTS
	}
	key := channelID
	if root != "" {
		key += ":" + root
	}
	c := s.byKey[key]
	if c == nil {
		c = &candidate{channelID: channelID, channelName: name, imUser: imUser, convKind: convKind, root: root}
		if s.byKey == nil {
			s.byKey = map[string]*candidate{}
		}
		s.byKey[key] = c
		s.list = append(s.list, c)
	}
	if mention && convKind == convChannel {
		c.mentionTS = append(c.mentionTS, ts)
	}
	c.mine = c.mine || mine
	c.newest = max(c.newest, tsNum(ts))
}

// evaluate reads a candidate and returns the item it makes, nil when
// nothing waits there.
func (f *fetcher) evaluate(ctx context.Context, c *candidate) (*draft, error) {
	var msgs []map[string]any
	var err error
	if c.root != "" {
		msgs, err = f.replies(ctx, c.channelID, c.root)
	} else {
		msgs, err = f.history(ctx, c.channelID)
	}
	if err != nil {
		return nil, err
	}

	// What the caller posted, reacted to or (at the top level) replied to
	// in a thread acknowledges everything up to it.
	ack, posted := 0.0, false
	for _, m := range msgs {
		ts := tsNum(jsonx.Str(m["ts"]))
		switch {
		case f.wrote(m):
			ack, posted = max(ack, ts), true
		case f.acknowledged(m, c.root == ""):
			ack = max(ack, ts)
		}
	}
	c.posted = posted
	var waiting []map[string]any
	for _, m := range msgs {
		if ts := jsonx.Str(m["ts"]); tsNum(ts) > ack && f.inWindow(ts) && isFromOther(m, f.me) {
			waiting = append(waiting, m)
		}
	}
	if len(waiting) == 0 {
		return nil, nil
	}

	kind := c.convKind
	if kind == convChannel {
		switch {
		case slices.ContainsFunc(c.mentionTS, func(ts string) bool { return tsNum(ts) > ack }):
			kind = KindMention
		case (c.mine || posted) && !f.noThreads:
			// Also a mention the caller answered, with replies since.
			kind = KindThread
		default:
			return nil, nil
		}
	}

	if replyCount, _ := jsonx.Int(msgs[0]["reply_count"]); kind == KindMention && len(msgs) <= 1 && replyCount == 0 {
		// A top-level mention nobody replied to: a later message of the
		// caller's in the channel counts as the answer.
		mention := slices.MaxFunc(c.mentionTS, func(a, b string) int { return cmp.Compare(tsNum(a), tsNum(b)) })
		// Without "latest" the pages go forward from the mention: an
		// answer, most often written soon after it, is on the first
		// pages, however busy the channel has been since.
		later, err := f.pages(ctx, "conversations.history", map[string]any{
			"channel":   c.channelID,
			"oldest":    mention,
			"inclusive": false,
		}, f.wrote)
		if err != nil {
			return nil, err
		}
		if slices.ContainsFunc(later, f.wrote) {
			return nil, nil
		}
	}

	newest := waiting[len(waiting)-1]
	ts := jsonx.Str(newest["ts"])
	item := Item{
		Kind:      kind,
		ChannelID: c.channelID,
		ThreadTS:  c.root,
		TS:        ts,
		FirstTS:   jsonx.Str(waiting[0]["ts"]),
		Count:     len(waiting),
		Author:    Author{UserID: jsonx.Str(newest["user"])},
		Text:      truncate(slacktext.RenderMessageContent(newest), f.maxBodyChars),
	}
	if f.baseURL != "" {
		item.Permalink = slackurl.BuildMessageURL(f.baseURL, c.channelID, ts, c.root)
	}
	return &draft{item: item, imUser: c.imUser, channelName: c.channelName}, nil
}

// wrote reports whether m is a message of the caller's.
func (f *fetcher) wrote(m map[string]any) bool {
	return isHuman(m) && jsonx.Str(m["user"]) == f.me
}

// acknowledged reports whether the caller wrote m, reacted to it or, for
// a top-level message, replied in its thread.
func (f *fetcher) acknowledged(m map[string]any, topLevel bool) bool {
	return f.wrote(m) || reactedBy(m, f.me) || topLevel && slices.Contains(jsonx.Slice(m["reply_users"]), any(f.me))
}

// pages reads a conversation or a thread page after page (pageSize
// messages each) and returns the messages, in Slack's order and without
// duplicates. It stops at the end, after a page for which enough says
// that the rest is not needed (nil: never), or after maxPages pages: the
// result is then marked incomplete.
func (f *fetcher) pages(ctx context.Context, method string, params map[string]any, enough func(m map[string]any) bool) ([]map[string]any, error) {
	var out []map[string]any
	seen := map[string]bool{}
	params["limit"] = pageSize
	for page := 1; ; page++ {
		resp, err := f.api(ctx, method, params)
		if err != nil {
			return nil, err
		}
		msgs := jsonx.Maps(resp["messages"])
		for _, m := range msgs {
			// conversations.replies repeats the root on every page.
			if ts := jsonx.Str(m["ts"]); ts != "" && !seen[ts] {
				seen[ts] = true
				out = append(out, m)
			}
		}
		cursor := jsonx.GetStr(resp, "response_metadata", "next_cursor")
		if cursor == "" || resp["has_more"] == false {
			return out, nil
		}
		if enough != nil && slices.ContainsFunc(msgs, enough) {
			return out, nil
		}
		if page == maxPages {
			f.incomplete.Store(true)
			return out, nil
		}
		// A copy: the previous map may still be held by the client.
		params = maps.Clone(params)
		params["cursor"] = cursor
	}
}

// history returns the top-level messages of a conversation since the
// start of the window, oldest first.
//
// With "oldest" alone, conversations.history starts at the oldest page
// and moves forward; with "latest" as well it starts at the newest page
// and moves back, which is what is wanted here: the newest messages are
// the ones that wait, no more needs reading once a page holds a sign of the
// caller (a message, a reaction, a thread reply), which answers everything
// older, and a conversation too long to be read whole loses its oldest
// messages, not its newest.
func (f *fetcher) history(ctx context.Context, channelID string) ([]map[string]any, error) {
	msgs, err := f.pages(ctx, "conversations.history", map[string]any{
		"channel":   channelID,
		"oldest":    f.oldest,
		"latest":    latest(),
		"inclusive": true,
	}, func(m map[string]any) bool { return f.acknowledged(m, true) })
	if err != nil {
		return nil, err
	}
	return sortByTS(msgs), nil
}

// replies returns the messages of a thread, oldest first, those from
// before the window included (evaluate leaves them out).
//
// The whole thread is read, without "oldest": with it,
// conversations.replies loses messages as soon as there is a second page
// (seen on a thread of 5 with a limit of 3: the next page held the root
// alone, and the newest reply was never given). Nor is the reading cut
// short the way history's is: which end the pages start from is not
// something Slack documents.
func (f *fetcher) replies(ctx context.Context, channelID, root string) ([]map[string]any, error) {
	msgs, err := f.pages(ctx, "conversations.replies", map[string]any{
		"channel": channelID,
		"ts":      root,
	}, nil)
	if err != nil {
		return nil, err
	}
	return sortByTS(msgs), nil
}

// humanSubtypes are the subtypes of a message that is a person talking;
// every other subtype (joins, topic changes, bot messages …) is noise.
var humanSubtypes = []string{"thread_broadcast", "file_share", "me_message"}

func isHuman(m map[string]any) bool {
	subtype := jsonx.Str(m["subtype"])
	return subtype == "" || slices.Contains(humanSubtypes, subtype)
}

// isFromOther reports whether m was written by a person other than me.
func isFromOther(m map[string]any, me string) bool {
	user := jsonx.Str(m["user"])
	return user != "" && user != me && user != "USLACKBOT" && jsonx.Str(m["bot_id"]) == "" && isHuman(m)
}

func reactedBy(m map[string]any, me string) bool {
	return slices.ContainsFunc(jsonx.Maps(m["reactions"]), func(r map[string]any) bool {
		return slices.Contains(jsonx.Slice(r["users"]), any(me))
	})
}

// tsNum is a Slack timestamp as a number of seconds (0 when it is not
// one). A float64 tells apart any two timestamps of this century.
func tsNum(ts string) float64 {
	n, err := strconv.ParseFloat(ts, 64)
	if err != nil {
		return 0
	}
	return n
}

func sortByTS(msgs []map[string]any) []map[string]any {
	slices.SortStableFunc(msgs, func(a, b map[string]any) int {
		return cmp.Compare(tsNum(jsonx.Str(a["ts"])), tsNum(jsonx.Str(b["ts"])))
	})
	return msgs
}

// threadTSFromPermalink returns the thread root named by the permalink of
// a search match ("" for a message that is not in a thread).
func threadTSFromPermalink(permalink string) string {
	u, err := url.Parse(permalink)
	if err != nil {
		return ""
	}
	return u.Query().Get("thread_ts")
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
	return string(runes[:maxChars]) + "..."
}
