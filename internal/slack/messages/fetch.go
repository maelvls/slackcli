package messages

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/maelvls/slackcli/internal/jsonx"
	"github.com/maelvls/slackcli/internal/slack/enrich"
	"github.com/maelvls/slackcli/internal/slack/slackurl"
	"github.com/maelvls/slackcli/internal/slackapi"
)

const (
	// DefaultHistoryLimit is the number of messages FetchChannelHistory
	// returns when no limit is given.
	DefaultHistoryLimit = 25
	// MaxHistoryLimit is the most messages one FetchChannelHistory call
	// returns; larger limits are clamped.
	MaxHistoryLimit = 1000
	// pageSize is the page size for history and replies, as Slack
	// recommends.
	pageSize = 200
)

// ErrMessageNotFound means the message does not exist or is not visible
// to these credentials.
var ErrMessageNotFound = errors.New("message not found")

// CompareTS orders two Slack timestamps ("1770165109.628379")
// chronologically. Timestamps are compared digit by digit: as float64
// they lose their last digit of precision, which is enough to swap two
// messages posted in the same instant.
func CompareTS(a, b string) int {
	aSec, aFrac, _ := strings.Cut(a, ".")
	bSec, bFrac, _ := strings.Cut(b, ".")
	aSec, bSec = strings.TrimLeft(aSec, "0"), strings.TrimLeft(bSec, "0")
	if c := len(aSec) - len(bSec); c != 0 {
		return c
	}
	if c := strings.Compare(aSec, bSec); c != 0 {
		return c
	}
	return strings.Compare(strings.TrimRight(aFrac, "0"), strings.TrimRight(bFrac, "0"))
}

func sortChronologically(msgs []Message) {
	slices.SortStableFunc(msgs, func(a, b Message) int { return CompareTS(a.TS, b.TS) })
}

// metadataParam mirrors the TypeScript: reactions travel with the message
// either way, include_all_metadata additionally asks for message metadata.
func metadataParam(params map[string]any, include bool) {
	if include {
		params["include_all_metadata"] = true
	}
}

func nextCursor(resp map[string]any) string {
	return jsonx.GetStr(resp, "response_metadata", "next_cursor")
}

// FindTS returns the raw message with that ts among the "messages" of a
// conversations.history or conversations.replies response, or nil when it
// is not there (a nil response included). Both methods answer with the
// nearest messages rather than fail, so a response must be checked for the
// message that was asked for.
func FindTS(resp map[string]any, ts string) map[string]any {
	for _, m := range jsonx.Maps(resp["messages"]) {
		if jsonx.Str(m["ts"]) == ts {
			return m
		}
	}
	return nil
}

// FetchMessage fetches the message ref points at, whether it is a
// top-level message or a thread reply (replies are not part of the channel
// history; ref.ThreadTSHint, when set, says which thread to look in).
//
// It fails with an error wrapping ErrMessageNotFound when the message
// cannot be found.
func FetchMessage(ctx context.Context, client *slackapi.Client, ref slackurl.MessageRef, includeReactions bool) (Message, error) {
	raw, err := findMessage(ctx, client, ref, includeReactions)
	if err != nil {
		return Message{}, err
	}
	msgs := []Message{ParseMessage(ref.ChannelID, raw)}
	if err := enrichFiles(ctx, client, msgs); err != nil {
		return Message{}, err
	}
	return msgs[0], nil
}

func findMessage(ctx context.Context, client *slackapi.Client, ref slackurl.MessageRef, includeReactions bool) (map[string]any, error) {
	channel, ts := ref.ChannelID, ref.MessageTS

	// 1. Top-level messages: the newest message at or before ts.
	params := map[string]any{"channel": channel, "latest": ts, "inclusive": true, "limit": 1}
	metadataParam(params, includeReactions)
	resp, err := client.API(ctx, "conversations.history", params)
	if err != nil {
		return nil, fmt.Errorf("fetching message %s in %s: %w", ts, channel, err)
	}
	if m := FindTS(resp, ts); m != nil {
		return m, nil
	}

	// 2. A reply in the thread named by the permalink. Bounding the range
	// to ts makes Slack return the root and that one reply instead of
	// having us page through the whole thread.
	if hint := ref.ThreadTSHint; hint != "" && hint != ts {
		cursor := ""
		for {
			params := map[string]any{"channel": channel, "ts": hint, "oldest": ts, "latest": ts, "inclusive": true, "limit": pageSize}
			if cursor != "" {
				params["cursor"] = cursor
			}
			metadataParam(params, includeReactions)
			resp, err := client.API(ctx, "conversations.replies", params)
			if err != nil {
				if enrich.MustAbortRequired(ctx, err) {
					return nil, fmt.Errorf("fetching message %s in %s: %w", ts, channel, err)
				}
				break // e.g. thread_not_found: the hint was wrong
			}
			if m := FindTS(resp, ts); m != nil {
				return m, nil
			}
			next := nextCursor(resp)
			if next == "" || next == cursor {
				break
			}
			cursor = next
		}
	}

	// 3. No (usable) hint: conversations.replies accepts the ts of any
	// message of a thread and returns that message first.
	params = map[string]any{"channel": channel, "ts": ts, "limit": 1}
	metadataParam(params, includeReactions)
	resp, err = client.API(ctx, "conversations.replies", params)
	if enrich.MustAbortRequired(ctx, err) {
		return nil, fmt.Errorf("fetching message %s in %s: %w", ts, channel, err)
	}
	if err == nil {
		if m := FindTS(resp, ts); m != nil {
			return m, nil
		}
	}

	return nil, fmt.Errorf("%w: no message with ts %s in channel %s. It may have been deleted, the ts or URL may be wrong or truncated (quote URLs containing \"&\"), or these credentials cannot see it",
		ErrMessageNotFound, ts, channel)
}

// HistoryOptions selects messages of a channel.
type HistoryOptions struct {
	// Limit is the number of messages to return: 0 means
	// DefaultHistoryLimit; values are clamped to 1..MaxHistoryLimit.
	Limit int
	// Latest and Oldest bound the range, exclusively: only messages
	// strictly before Latest and strictly after Oldest are returned. ""
	// means unbounded.
	Latest string
	Oldest string
	// IncludeReactions asks for the complete message payload.
	IncludeReactions bool
	// WithReactions keeps only messages carrying every one of these
	// reaction names; WithoutReactions drops messages carrying any of
	// them. With either set, the channel is scanned back to Oldest (set
	// it, or the scan covers the whole channel) until Limit messages
	// match.
	WithReactions    []string
	WithoutReactions []string
	// IncludeSystem keeps the system messages (see IsSystem), which are
	// otherwise skipped and counted in History.SystemHidden: they do not
	// count towards Limit.
	IncludeSystem bool
}

// History is the result of FetchChannelHistory.
type History struct {
	// Messages are in chronological order (oldest first).
	Messages []Message
	// SystemHidden is the number of system messages that were skipped
	// while collecting Messages.
	SystemHidden int
	// HasMore is true when Messages stopped at the limit while older
	// messages may be left: some of the last page that would have been
	// returned were not taken, or Slack has more pages (which can hold
	// only system messages, or only messages the reaction filters
	// reject). Page back with Latest set to the ts of the oldest message
	// (Messages[0]).
	HasMore bool
}

// FetchChannelHistory returns the most recent messages of a channel that
// match opts, in chronological order (oldest first). Thread replies are
// not included, only top-level messages.
func FetchChannelHistory(ctx context.Context, client *slackapi.Client, channelID string, opts HistoryOptions) (History, error) {
	limit := opts.Limit
	if limit <= 0 {
		limit = DefaultHistoryLimit
	}
	limit = min(limit, MaxHistoryLimit)
	filtered := len(opts.WithReactions) > 0 || len(opts.WithoutReactions) > 0

	var out []Message
	hidden := 0
	hasMore := false
	latest, cursor := opts.Latest, ""
	// Given oldest alone, Slack walks forward from it: its first page is
	// the messages right after oldest, the next cursor goes newer, and a
	// listing cut by the limit would be the oldest messages of the range
	// (with a gap where a page was cut). A latest in the future makes it
	// walk back from the newest message, like without oldest.
	if latest == "" && opts.Oldest != "" {
		latest = strconv.FormatInt(time.Now().Add(24*time.Hour).Unix(), 10) + ".000000"
	}
	// keep reports whether a raw message of the history is one to return.
	keep := func(m map[string]any) bool {
		if !opts.IncludeSystem && IsSystem(jsonx.Str(m["subtype"])) {
			return false
		}
		return !filtered || PassesReactionFilters(m, opts.WithReactions, opts.WithoutReactions)
	}
	for {
		params := map[string]any{"channel": channelID, "limit": pageSize}
		// Ask for what is missing, no more. Once system messages have
		// been skipped, how many messages a page must hold to yield that
		// many is anyone's guess (joins can outnumber the rest five to
		// one): full pages then, rather than a call per handful.
		if !filtered && hidden == 0 {
			params["limit"] = min(limit-len(out), pageSize)
		}
		if latest != "" {
			params["latest"] = latest
		}
		if opts.Oldest != "" {
			params["oldest"] = opts.Oldest
		}
		if cursor != "" {
			params["cursor"] = cursor
		}
		metadataParam(params, opts.IncludeReactions || filtered)
		resp, err := client.API(ctx, "conversations.history", params)
		if err != nil {
			return History{}, fmt.Errorf("fetching the history of %s: %w", channelID, err)
		}
		page := jsonx.Maps(resp["messages"])
		if len(page) == 0 {
			break
		}
		for i, m := range page {
			if !opts.IncludeSystem && IsSystem(jsonx.Str(m["subtype"])) {
				hidden++
				continue
			}
			if filtered && !PassesReactionFilters(m, opts.WithReactions, opts.WithoutReactions) {
				continue
			}
			out = append(out, ParseMessage(channelID, m))
			if len(out) >= limit {
				// Full: is anything left that would have been returned?
				hasMore = jsonx.Bool(resp["has_more"]) || slices.ContainsFunc(page[i+1:], keep)
				break
			}
		}
		if len(out) >= limit || !jsonx.Bool(resp["has_more"]) {
			break
		}

		// Next page: Slack's cursor when it gives one, else everything
		// older than the last (oldest) message of this page.
		if next := nextCursor(resp); next != "" {
			if next == cursor {
				break
			}
			cursor = next
			continue
		}
		last := jsonx.Str(page[len(page)-1]["ts"])
		if last == "" || last == latest {
			break
		}
		latest, cursor = last, ""
	}

	if err := enrichFiles(ctx, client, out); err != nil {
		return History{}, fmt.Errorf("fetching the history of %s: %w", channelID, err)
	}
	// conversations.history returns newest first.
	sortChronologically(out)
	return History{Messages: out, SystemHidden: hidden, HasMore: hasMore}, nil
}

// expandConcurrency bounds the conversations.replies calls of
// ExpandThreads.
const expandConcurrency = 4

// ExpandThreads returns msgs, the top-level messages of a channel in
// chronological order, with the replies of each thread inserted after its
// root: what reading a channel and then each of its threads gives, in one
// go. Messages without replies are left as they are.
//
// The replies are an enrichment (see package enrich): the error is only
// for a cancelled context or dead credentials. When conversations.replies
// is rate limited no further thread is fetched, the roots that are left
// keep their ReplyCount for the reader to list them, and the collector of
// ctx is told.
func ExpandThreads(ctx context.Context, client *slackapi.Client, channelID string, msgs []Message, includeReactions bool) ([]Message, error) {
	var roots []int
	for i, m := range msgs {
		if m.ReplyCount > 0 {
			roots = append(roots, i)
		}
	}
	if len(roots) == 0 {
		return msgs, nil
	}

	var (
		mu      sync.Mutex
		wg      sync.WaitGroup
		fatal   error
		sem     = make(chan struct{}, expandConcurrency)
		threads = map[int][]Message{}
		// limited stops the lookups once Slack has refused one.
		limited enrich.Gate
	)
	for _, i := range roots {
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			mu.Lock()
			abort := fatal != nil
			mu.Unlock()
			if abort || limited.Closed() {
				return
			}
			thread, err := FetchThread(ctx, client, channelID, msgs[i].TS, includeReactions)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				threads[i] = thread
			case enrich.MustAbort(ctx, err):
				fatal = cmp.Or(fatal, err)
			default:
				limited.Failed(err)
			}
		})
	}
	wg.Wait()
	if fatal != nil {
		return nil, fatal
	}
	if skipped, rateLimit := limited.Skipped(); skipped > 0 {
		enrich.Warnf(ctx, "the replies of %d of %d threads could not be fetched (%v): those threads are listed with their first message only, see its reply_count; %s",
			skipped, len(roots), enrich.Cause(rateLimit), enrich.Hint())
	}

	out := make([]Message, 0, len(msgs))
	for i, m := range msgs {
		// A thread that could not be read, or that came back empty (the
		// root was deleted meanwhile), leaves the message as it was.
		if thread := threads[i]; len(thread) > 0 {
			out = append(out, thread...)
			continue
		}
		out = append(out, m)
	}
	return out, nil
}

// PassesReactionFilters reports whether a raw Slack message carries every
// reaction named in with and none of those named in without. Names are
// bare ("eyes", not ":eyes:").
func PassesReactionFilters(msg map[string]any, with, without []string) bool {
	names := map[string]bool{}
	for _, r := range jsonx.Maps(msg["reactions"]) {
		if name := strings.TrimSpace(jsonx.Str(r["name"])); name != "" {
			names[name] = true
		}
	}
	for _, name := range with {
		if !names[name] {
			return false
		}
	}
	for _, name := range without {
		if names[name] {
			return false
		}
	}
	return true
}

// FetchThread returns a whole thread, root first then every reply in
// chronological order. threadTS is normally the root's ts; given the ts
// of a reply, the thread it belongs to is returned all the same.
func FetchThread(ctx context.Context, client *slackapi.Client, channelID, threadTS string, includeReactions bool) ([]Message, error) {
	out, err := fetchReplies(ctx, client, channelID, threadTS, includeReactions)
	if err != nil {
		return nil, err
	}
	// Asked about a reply, Slack answers with that reply (alone, or
	// followed by whatever else it sees fit) instead of the thread: the
	// first message it returns then names the real root.
	if len(out) > 0 && out[0].ThreadTS != "" && out[0].ThreadTS != threadTS {
		out, err = fetchReplies(ctx, client, channelID, out[0].ThreadTS, includeReactions)
		if err != nil {
			return nil, err
		}
	}
	if err := enrichFiles(ctx, client, out); err != nil {
		return nil, fmt.Errorf("fetching thread %s in %s: %w", threadTS, channelID, err)
	}
	sortChronologically(out)
	nameThread(out)
	return out, nil
}

// nameThread makes every message of a thread say which thread it is in,
// as the messages of a channel history do: a message of a thread listing
// is then the same {channel_id, ts, thread_ts} wherever it came from.
// Slack sets thread_ts on the replies and on a root that has some; the
// messages it left out get the one of the others. A lone message without
// replies is not a thread and stays as it is.
func nameThread(msgs []Message) {
	root := ""
	for _, m := range msgs {
		if m.ThreadTS != "" {
			root = m.ThreadTS
			break
		}
	}
	if root == "" {
		return
	}
	for i := range msgs {
		if msgs[i].ThreadTS == "" {
			msgs[i].ThreadTS = root
		}
	}
}

func fetchReplies(ctx context.Context, client *slackapi.Client, channelID, threadTS string, includeReactions bool) ([]Message, error) {
	var out []Message
	seen := map[string]bool{}
	cursor := ""
	for {
		params := map[string]any{"channel": channelID, "ts": threadTS, "limit": pageSize}
		if cursor != "" {
			params["cursor"] = cursor
		}
		metadataParam(params, includeReactions)
		resp, err := client.API(ctx, "conversations.replies", params)
		if err != nil {
			return nil, fmt.Errorf("fetching thread %s in %s: %w", threadTS, channelID, err)
		}
		for _, m := range jsonx.Maps(resp["messages"]) {
			// Slack repeats the thread root on every page.
			ts := jsonx.Str(m["ts"])
			if ts != "" && seen[ts] {
				continue
			}
			seen[ts] = true
			out = append(out, ParseMessage(channelID, m))
		}
		next := nextCursor(resp)
		if next == "" || next == cursor {
			return out, nil
		}
		cursor = next
	}
}
