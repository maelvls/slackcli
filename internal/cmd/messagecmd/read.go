package messagecmd

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/maelvls/slackcli/internal/cmd/cmdutil"
	"github.com/maelvls/slackcli/internal/slack/messages"
	"github.com/maelvls/slackcli/internal/slack/slacktext"
	"github.com/maelvls/slackcli/internal/slack/slackurl"
	"github.com/maelvls/slackcli/internal/slack/users"
	"github.com/maelvls/slackcli/internal/slackapi"
)

// readOptions are the flags "get" and "list" share.
type readOptions struct {
	workspace        string
	ts               string
	threadTS         string
	maxBodyChars     int
	noDownload       bool
	includeReactions bool
	resolveUsers     bool
	refreshUsers     bool
}

type listOptions struct {
	readOptions
	limit           int
	oldest          string
	latest          string
	withReaction    []string
	withoutReaction []string
	includeSystem   bool
	expandThreads   bool
}

// getResult is the output of "get".
type getResult struct {
	Message         messages.CompactMessage      `json:"message"`
	Thread          *threadSummary               `json:"thread"`
	ReferencedUsers map[string]users.CompactUser `json:"referenced_users"`
}

// listResult is the output of "list", the same for a channel's history and
// for a thread: every message carries its own channel_id (and thread_ts
// when it belongs to a thread), so that a message taken out of the list
// still has the documented message shape.
type listResult struct {
	ChannelID string                    `json:"channel_id"`
	Messages  []messages.CompactMessage `json:"messages"`
	// SystemMessagesHidden is the number of system messages (joins,
	// leaves, topic changes …) left out of a channel's history.
	SystemMessagesHidden int                          `json:"system_messages_hidden,omitempty"`
	ReferencedUsers      map[string]users.CompactUser `json:"referenced_users"`
}

func addReadFlags(cmd *cobra.Command, o *readOptions) {
	f := cmd.Flags()
	f.StringVar(&o.workspace, "workspace", "", cmdutil.TargetWorkspaceUsage)
	cmdutil.AddMaxBodyCharsFlag(cmd, &o.maxBodyChars)
	f.BoolVar(&o.noDownload, cmdutil.NoDownloadFlagName, false, "Do not download attached files: they are listed without a local path, which is faster when only the text matters")
	f.BoolVar(&o.includeReactions, "include-reactions", false, "Include reactions and the IDs of the users who reacted")
	f.BoolVar(&o.resolveUsers, "resolve-users", false, `Name the users: add "name" to each author, and "referenced_users", the profiles of the user IDs appearing in the output (cached for 24h)`)
	f.BoolVar(&o.refreshUsers, "refresh-users", false, "Like --resolve-users, but fetch the profiles again instead of using the cache")
}

func addGetFlags(cmd *cobra.Command, o *readOptions) {
	addReadFlags(cmd, o)
	cmd.Flags().StringVar(&o.ts, "ts", "", cmdutil.MessageTSUsage)
	cmd.Flags().StringVar(&o.threadTS, "thread-ts", "", "Thread root ts: where to look when the message is a thread reply (a permalink's ?thread_ts= already says so)")
}

func (c *command) newGet() *cobra.Command {
	var opts readOptions
	cmd := &cobra.Command{
		Use:   "get <target>",
		Short: "Fetch a single Slack message (with thread summary if any)",
		Long: `Fetch a single Slack message (with thread summary if any).

<target> is a Slack message URL, or a channel (#name, name, channel ID or
channel URL) or a user ID (U…: the DM with that user) combined with --ts.

Prints {message, thread, referenced_users}; see "list" for the fields of a
message. "thread" ({ts, length}) is present when the message belongs to a
thread: use "list" on the same target to read it. Attached files are
downloaded and listed with their local path; a file that cannot be
downloaded is listed with an "error" instead. With --no-download nothing is
downloaded and the files are listed without a path.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return c.runGet(cmd, args[0], opts)
		},
	}
	addGetFlags(cmd, &opts)
	return cmd
}

func (c *command) runGet(cmd *cobra.Command, targetInput string, opts readOptions) error {
	ctx := cmd.Context()
	// "message <target>" is "message get <target>". Reading opens no DM:
	// a user target is the DM that exists with that user.
	target, err := cmdutil.ParseTarget(c.rt, targetInput, opts.workspace)
	if err != nil {
		return err
	}
	target.ReadOnly = true
	ts, err := target.MessageTS(opts.ts)
	if err != nil {
		return err
	}
	threadTS, err := cmdutil.ParseTS("--thread-ts", opts.threadTS)
	if err != nil {
		return err
	}
	// 0 would mean "the default" to messages.CompactOptions.
	if err := cmdutil.CheckMaxBodyChars(cmd, opts.maxBodyChars); err != nil {
		return err
	}

	result, err := cmdutil.WithTarget(ctx, c.rt, target, func(loc cmdutil.Located) (getResult, error) {
		ref := target.Ref // zero unless the target is a message URL
		ref.WorkspaceURL, ref.ChannelID, ref.MessageTS, ref.Raw = loc.WorkspaceURL, loc.ChannelID, ts, targetInput
		if ref.ThreadTSHint == "" {
			ref.ThreadTSHint = threadTS
		}
		msg, err := messages.FetchMessage(ctx, loc.Client, ref, opts.includeReactions)
		if err != nil {
			return getResult{}, err
		}
		thread, err := getThreadSummary(ctx, loc.Client, loc.ChannelID, msg)
		if err != nil {
			return getResult{}, err
		}
		msgs := []messages.Message{msg}
		compact, referenced, err := c.present(ctx, loc, msgs, opts)
		if err != nil {
			return getResult{}, err
		}
		return getResult{Message: compact[0], Thread: thread, ReferencedUsers: referenced}, nil
	})
	if err != nil {
		return err
	}
	return c.rt.Print(result)
}

// present downloads the files of msgs (unless --no-download), converts
// them to the printed shape and resolves the users they reference when
// asked to.
func (c *command) present(ctx context.Context, loc cmdutil.Located, msgs []messages.Message, opts readOptions) ([]messages.CompactMessage, map[string]users.CompactUser, error) {
	downloads := listedFiles(msgs)
	if !opts.noDownload {
		var err error
		if downloads, err = c.downloadMessageFiles(ctx, loc.Client, msgs); err != nil {
			return nil, nil, err
		}
	}
	compact := make([]messages.CompactMessage, len(msgs))
	for i, m := range msgs {
		compact[i] = messages.ToCompact(m, messages.CompactOptions{
			MaxBodyChars:     opts.maxBodyChars,
			IncludeReactions: opts.includeReactions,
			Downloads:        downloads,
		})
	}
	if !opts.resolveUsers && !opts.refreshUsers {
		return compact, nil, nil
	}
	ids := users.CollectReferencedUserIDs(msgs, opts.includeReactions)
	byID, err := users.ResolveByID(ctx, loc.Client, ids, users.ResolveOptions{
		WorkspaceURL: loc.WorkspaceURL,
		ForceRefresh: opts.refreshUsers,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("resolving users: %w", err)
	}
	// Who wrote what is read off each message, without a detour through
	// referenced_users.
	for i := range compact {
		if a := compact[i].Author; a != nil {
			if u, ok := byID[a.UserID]; ok {
				a.Name = cmp.Or(u.DisplayName, u.RealName, u.Name)
			}
		}
	}
	return compact, users.ToReferencedUsers(ids, byID), nil
}

func (c *command) newList() *cobra.Command {
	var opts listOptions
	cmd := &cobra.Command{
		Use:   "list <target>",
		Short: "List recent channel messages, or fetch a full thread",
		Long: fmt.Sprintf(`List recent channel messages, or fetch a full thread.

  list <channel>                   the most recent top-level messages of the
                                   channel, oldest first (--limit, --oldest,
                                   --latest, --expand-threads, --include-system,
                                   --with-reaction, --without-reaction)
  list <message-url>               the whole thread that message belongs to
  list <channel> --ts <ts>         same, for the message with that ts
  list <channel> --thread-ts <ts>  the whole thread with that root

<channel> is #name, name, a channel ID or a channel URL; a user ID (U…) is
the DM with that user (none is opened: it is an error when there is none).

Channel history has top-level messages only: a message with "reply_count"
started a thread, whose replies are not listed. List that thread to read
them, or pass --expand-threads to get the replies of every thread listed,
each after its first message. System messages (joins, leaves, topic changes …)
are left out and counted in "system_messages_hidden", unless
--include-system is given. At most %d messages are returned per call; to page
back, pass the ts of the oldest message received as --latest.

Prints {channel_id, messages: [{channel_id, ts, time, thread_ts, reply_count,
author: {user_id, bot_id, name}, content, files, reactions,
forwarded_threads}], system_messages_hidden, referenced_users}, oldest
message first, in the same shape for a channel and for a thread.
"messages" is [] when there are none, with a hint in "hints". "time" is "ts"
as a local date and time. "content" is Markdown. "thread_ts" is the root of
the thread a message belongs to: every message of a listed thread has it,
and in channel history only the messages that started a thread, which also
have "reply_count". "reactions" needs --include-reactions; "author.name"
and "referenced_users" need --resolve-users. Attached files are downloaded
and listed with their local path, as by "get", unless --no-download is
given.`, messages.MaxHistoryLimit),
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return c.runList(cmd, args[0], opts)
		},
	}
	addReadFlags(cmd, &opts.readOptions)
	f := cmd.Flags()
	f.StringVar(&opts.threadTS, "thread-ts", "", "Thread root ts: list that thread (root and replies) instead of the channel history. Cannot be combined with a different --ts")
	f.StringVar(&opts.ts, "ts", "", "Message ts: list the thread that message belongs to. Cannot be combined with a different --thread-ts")
	f.IntVar(&opts.limit, "limit", messages.DefaultHistoryLimit, fmt.Sprintf("Max messages to return (channel history mode; 1 to %d)", messages.MaxHistoryLimit))
	f.StringVar(&opts.oldest, "oldest", "", "Only messages after this moment, exclusive: a day (2026-01-31, which starts at 00:00 local time), a date and time (2026-01-31T14:30 local time, or with an offset: 2026-01-31T14:30:00Z), a Slack ts or a Unix time (channel history mode)")
	f.StringVar(&opts.latest, "latest", "", "Only messages before this moment, exclusive, written like --oldest: --oldest 2026-01-30 --latest 2026-01-31 is the whole of January 30 (channel history mode)")
	f.BoolVar(&opts.expandThreads, "expand-threads", false, "Also list the replies of each thread, after its first message: one call instead of one per thread (channel history mode)")
	f.BoolVar(&opts.includeSystem, "include-system", false, `Include system messages (joins, leaves, topic changes, etc.) instead of only counting them in "system_messages_hidden" (channel history mode)`)
	f.StringArrayVar(&opts.withReaction, "with-reaction", nil, "Only messages carrying this reaction (:eyes:, eyes or 👀; repeatable, all must be present; channel history mode; requires --oldest)")
	f.StringArrayVar(&opts.withoutReaction, "without-reaction", nil, "Only messages not carrying this reaction (repeatable; channel history mode; requires --oldest)")
	return cmd
}

// historyOnlyFlags only make sense when listing a channel's history.
var historyOnlyFlags = []string{"limit", "oldest", "latest", "with-reaction", "without-reaction", "expand-threads", "include-system"}

// parseReactionFilters normalises repeatable reaction flags to bare
// reaction names, without duplicates.
func parseReactionFilters(flag string, raw []string) ([]string, error) {
	var out []string
	for _, v := range raw {
		name, err := slacktext.NormalizeReactionName(v)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", flag, err)
		}
		if !slices.Contains(out, name) {
			out = append(out, name)
		}
	}
	return out, nil
}

// requireOldestForReactionFilters refuses reaction filters without
// --oldest: they are applied client-side, so an unbounded scan would walk
// the whole channel.
func requireOldestForReactionFilters(oldest string, hasFilters bool) error {
	if hasFilters && oldest == "" {
		return errors.New(`reaction filters require --oldest (a day such as 2026-01-31, or a ts such as 1770165109.628379) to bound scan size`)
	}
	return nil
}

func (c *command) runList(cmd *cobra.Command, targetInput string, opts listOptions) error {
	ctx := cmd.Context()
	target, err := cmdutil.ParseTarget(c.rt, targetInput, opts.workspace)
	if err != nil {
		return err
	}
	// Reading opens no DM: a user target is the DM that exists with that
	// user.
	target.ReadOnly = true
	if err := cmdutil.CheckMaxBodyChars(cmd, opts.maxBodyChars); err != nil {
		return err
	}
	threadTS, err := cmdutil.ParseTS("--thread-ts", opts.threadTS)
	if err != nil {
		return err
	}

	// Which thread, if any? FetchThread accepts the ts of a reply too, so
	// the message itself need not be fetched first.
	rootTS := threadTS
	if target.Kind == slackurl.TargetURL {
		if _, err := target.MessageTS(opts.ts); err != nil {
			return err
		}
		// As elsewhere, a flag may repeat what the URL says but not
		// contradict it.
		if hint := target.Ref.ThreadTSHint; threadTS != "" && hint != "" && threadTS != hint {
			return fmt.Errorf("--thread-ts %s contradicts the message URL, whose thread is %s: drop --thread-ts, or pass the channel instead of the URL", threadTS, hint)
		}
		if rootTS == "" {
			rootTS = cmp.Or(target.Ref.ThreadTSHint, target.Ref.MessageTS)
		}
	} else {
		ts, err := cmdutil.ParseTS("--ts", opts.ts)
		if err != nil {
			return err
		}
		// Both name the thread to list; one of two different values would
		// have to be ignored.
		if ts != "" && threadTS != "" && ts != threadTS {
			return fmt.Errorf("--ts %s and --thread-ts %s both say which thread to list: pass only one of them (--thread-ts with the ts of the thread's first message, or --ts with the ts of any of its messages)", ts, threadTS)
		}
		rootTS = cmp.Or(rootTS, ts)
	}

	if rootTS != "" {
		for _, name := range historyOnlyFlags {
			if cmd.Flags().Changed(name) {
				return fmt.Errorf("--%s only applies to channel history, but this lists a thread (message URL, --ts or --thread-ts): drop --%[1]s, or pass only the channel", name)
			}
		}
		return c.listThread(ctx, target, rootTS, opts.readOptions)
	}
	return c.listHistory(ctx, target, opts)
}

func (c *command) listThread(ctx context.Context, target cmdutil.Target, rootTS string, opts readOptions) error {
	result, err := cmdutil.WithTarget(ctx, c.rt, target, func(loc cmdutil.Located) (listResult, error) {
		msgs, err := messages.FetchThread(ctx, loc.Client, loc.ChannelID, rootTS, opts.includeReactions)
		if slackapi.IsErrorCode(err, "thread_not_found") {
			return listResult{}, fmt.Errorf("no message with ts %s in channel %s: it may have been deleted, or the ts or URL is wrong or truncated (quote URLs containing \"&\"): %w", rootTS, loc.ChannelID, err)
		}
		if err != nil {
			return listResult{}, err
		}
		compact, referenced, err := c.present(ctx, loc, msgs, opts)
		if err != nil {
			return listResult{}, err
		}
		return listResult{ChannelID: loc.ChannelID, Messages: compact, ReferencedUsers: referenced}, nil
	})
	if err != nil {
		return err
	}
	return cmdutil.PrintList(c.rt, result, "messages")
}

func (c *command) listHistory(ctx context.Context, target cmdutil.Target, opts listOptions) error {
	if err := cmdutil.CheckLimit(opts.limit, messages.MaxHistoryLimit, "page further back with --latest"); err != nil {
		return err
	}
	oldest, err := parseHistoryBound("--oldest", opts.oldest)
	if err != nil {
		return err
	}
	latest, err := parseHistoryBound("--latest", opts.latest)
	if err != nil {
		return err
	}
	if oldest != "" && latest != "" && !boundBefore(oldest, latest) {
		return fmt.Errorf("--oldest %s is not before --latest %s: no message can be in between; swap them", oldest, latest)
	}
	with, err := parseReactionFilters("--with-reaction", opts.withReaction)
	if err != nil {
		return err
	}
	without, err := parseReactionFilters("--without-reaction", opts.withoutReaction)
	if err != nil {
		return err
	}
	for _, name := range with {
		if slices.Contains(without, name) {
			return fmt.Errorf("reaction %q is in both --with-reaction and --without-reaction: no message can match", name)
		}
	}
	if err := requireOldestForReactionFilters(oldest, len(with)+len(without) > 0); err != nil {
		return err
	}

	result, err := cmdutil.WithTarget(ctx, c.rt, target, func(loc cmdutil.Located) (listResult, error) {
		history, err := messages.FetchChannelHistory(ctx, loc.Client, loc.ChannelID, messages.HistoryOptions{
			Limit:            opts.limit,
			Latest:           latest,
			Oldest:           oldest,
			IncludeReactions: opts.includeReactions,
			WithReactions:    with,
			WithoutReactions: without,
			IncludeSystem:    opts.includeSystem,
		})
		if err != nil {
			return listResult{}, err
		}
		msgs := history.Messages
		if opts.expandThreads {
			if msgs, err = messages.ExpandThreads(ctx, loc.Client, loc.ChannelID, msgs, opts.includeReactions); err != nil {
				return listResult{}, err
			}
		}
		compact, referenced, err := c.present(ctx, loc, msgs, opts.readOptions)
		if err != nil {
			return listResult{}, err
		}
		return listResult{ChannelID: loc.ChannelID, Messages: compact, SystemMessagesHidden: history.SystemHidden, ReferencedUsers: referenced}, nil
	})
	if err != nil {
		return err
	}
	switch {
	case len(result.Messages) > 0:
	case result.SystemMessagesHidden > 0:
		c.rt.ResultHintf("No messages matched in %s, other than %d system messages (joins, leaves, topic changes …): pass --include-system to see them.",
			describeRange(oldest, latest), result.SystemMessagesHidden)
	default:
		c.rt.ResultHintf("No messages matched in %s.", describeRange(oldest, latest))
	}
	return cmdutil.PrintList(c.rt, result, "messages")
}

var historyBoundRe = regexp.MustCompile(`^\d+(\.\d+)?$`)

// historyBoundLayouts are the dates and times --oldest and --latest take,
// read in the local time zone: that is the one "yesterday" is meant in.
var historyBoundLayouts = []string{"2006-01-02", "2006-01-02T15:04", "2006-01-02T15:04:05", "2006-01-02 15:04", "2006-01-02 15:04:05"}

// parseHistoryBound turns --oldest / --latest into what Slack takes: a
// message ts or a Unix time in seconds, which are passed as they are, or a
// day or a date and time, which are converted. A day is its first instant
// in the local time zone, so that --oldest D --latest D+1 is the day D.
func parseHistoryBound(flag, raw string) (string, error) {
	v := strings.TrimSpace(raw)
	if v == "" || historyBoundRe.MatchString(v) {
		return v, nil
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return strconv.FormatInt(t.Unix(), 10), nil
	}
	for _, layout := range historyBoundLayouts {
		if t, err := time.ParseInLocation(layout, v, time.Local); err == nil {
			return strconv.FormatInt(t.Unix(), 10), nil
		}
	}
	return "", fmt.Errorf("invalid %s value %q: expected a day (2026-01-31), a date and time (2026-01-31T14:30, local time, or with an offset: 2026-01-31T14:30:00Z), a Slack ts (1770165109.628379) or a Unix time in seconds", flag, raw)
}

// boundBefore reports whether the history bound a (validated by
// parseHistoryBound) is strictly before b. Bounds have at most 6 decimals,
// which a float64 holds exactly enough at this magnitude.
func boundBefore(a, b string) bool {
	x, errA := strconv.ParseFloat(a, 64)
	y, errB := strconv.ParseFloat(b, 64)
	return errA == nil && errB == nil && x < y
}

// describeBound words a history bound: the ts Slack was given, and when
// that is.
func describeBound(ts string) string {
	if t := messages.FormatTime(ts, nil); t != "" {
		return ts + " (" + t + ")"
	}
	return ts
}

// describeRange words the --oldest/--latest window for the "no messages"
// hint, which says why "messages" is empty.
func describeRange(oldest, latest string) string {
	var parts []string
	if oldest != "" {
		parts = append(parts, "after "+describeBound(oldest))
	}
	if latest != "" {
		parts = append(parts, "before "+describeBound(latest))
	}
	if len(parts) == 0 {
		return "the channel"
	}
	return "the channel " + strings.Join(parts, " and ")
}
