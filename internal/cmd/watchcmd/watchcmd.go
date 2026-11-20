// Package watchcmd implements the "watch" command.
package watchcmd

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/spf13/cobra"

	"github.com/maelvls/slackcli/internal/clictx"
	"github.com/maelvls/slackcli/internal/cmd/cmdutil"
	"github.com/maelvls/slackcli/internal/jsonx"
	"github.com/maelvls/slackcli/internal/slack/channels"
	"github.com/maelvls/slackcli/internal/slack/enrich"
	"github.com/maelvls/slackcli/internal/slack/messages"
	"github.com/maelvls/slackcli/internal/slack/rtm"
	"github.com/maelvls/slackcli/internal/slack/slacktext"
	"github.com/maelvls/slackcli/internal/slack/users"
	"github.com/maelvls/slackcli/internal/slackapi"
)

const defaultMaxBodyChars = 4000

// Conversation types, as printed in Event.ChannelType (those of unreads).
const (
	typeChannel = "channel"
	typeDM      = "dm"
	typeMPIM    = "mpim"
)

// Event is one message, as printed.
type Event struct {
	ChannelID   string `json:"channel_id"`
	ChannelName string `json:"channel_name,omitempty"`
	ChannelType string `json:"channel_type,omitempty"`
	TS          string `json:"ts"`
	// Time is TS as a local date and time (messages.FormatTime).
	Time   string           `json:"time,omitempty"`
	Author *messages.Author `json:"author,omitempty"`
	// Content is the message rendered as Markdown, possibly truncated.
	Content string `json:"content,omitempty"`
	// ThreadTS is set on a reply: the ts of the first message of its
	// thread.
	ThreadTS string `json:"thread_ts,omitempty"`
	Subtype  string `json:"subtype,omitempty"`
}

// hiddenSubtypes are the message events that are not a new message: an
// edit, a deletion, and the notice that a thread got a reply (the reply
// itself is an event of its own).
var hiddenSubtypes = []string{"message_changed", "message_deleted", "message_replied"}

// streamOptions lets tests shorten the timings of the connection.
var streamOptions rtm.Options

// New returns the "watch" command.
func New(rt *clictx.Context) *cobra.Command {
	var (
		workspace     string
		only          []string
		maxBodyChars  int
		includeSystem bool
		text          bool
		duration      time.Duration
	)
	cmd := &cobra.Command{
		Use:   "watch",
		Short: "Print the messages of every conversation as they arrive",
		Long: `Stay connected to Slack and print each new message as it arrives: every
conversation you are a member of (channels, private channels, DMs, group
DMs), thread replies included, whether you read it or not, and your own
messages too. It runs until interrupted (Ctrl-C) or for --for.

Prints one JSON object per message (one per line, unless on a terminal):
  channel_id, channel_name
  channel_type  channel, dm or mpim, when Slack says
  ts, time      time is ts as a local date and time
  author        {user_id, bot_id, name}
  content       the message as Markdown
  thread_ts     on a thread reply: the ts of the first message of its thread
  subtype       Slack's subtype, if any (bot_message, thread_broadcast …)
With --text, one readable line per message instead:
  15:04:05 #channel author: content

Not printed: edits, deletions, reactions, and system messages (joins,
leaves, topic changes …) unless --include-system. Channels you are not a
member of are not received: use "search messages" for those. Messages sent
while the connection is down are not replayed: a line on stderr says how
long each gap was.

Nothing is marked as read. This command uses Slack's real-time API
(rtm.connect and a WebSocket): it works with browser session credentials
and classic tokens, not with the token of a Slack app.`,
		Example: `  slackcli watch --text
  slackcli watch --channel "#general" --channel C0123456789
  slackcli watch --for 10m | jq -c 'select(.channel_type == "dm")'`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := cmdutil.CheckMaxChars("--max-body-chars", maxBodyChars); err != nil {
				return err
			}
			if cmd.Flags().Changed("for") && duration <= 0 {
				return fmt.Errorf("--for must be a positive duration such as 30s or 10m, got %s", duration)
			}
			ws := rt.EffectiveWorkspace(workspace)
			if err := rt.AssertWorkspaceForChannelNames(ws, only...); err != nil {
				return err
			}
			ctx := cmd.Context()
			if duration > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, duration)
				defer cancel()
			}
			// downSince is since when messages may be missing because a
			// stream ended on rejected credentials: the stream opened
			// with the refreshed ones knows nothing of that gap. Zero
			// while connected, and before the first connection.
			var downSince time.Time
			_, err := clictx.WithAutoRefresh(ctx, rt, func() (struct{}, error) {
				r, err := rt.Client(ctx, ws)
				if err != nil {
					return struct{}{}, err
				}
				w := &watcher{
					rt: rt, client: r.Client,
					maxBodyChars: maxBodyChars, includeSystem: includeSystem, text: text,
					channelNames: map[string]string{}, userNames: map[string]string{},
					unnamedUntil: map[string]time.Time{},
				}
				if err := w.resolveFilter(ctx, only); err != nil {
					return struct{}{}, err
				}
				opts := streamOptions
				connected := false
				opts.OnConnect = func(gap time.Duration) {
					connected = true
					if gap == 0 && !downSince.IsZero() {
						gap = max(time.Since(downSince), time.Nanosecond)
					}
					downSince = time.Time{}
					if gap > 0 {
						rt.Hintf("Reconnected after %s: the messages sent meanwhile were not received.", gap.Round(time.Second))
						return
					}
					rt.Hintf("Connected to %s: waiting for messages.", cmp.Or(r.WorkspaceURL, "Slack"))
				}
				opts.OnDisconnect = func(err error) { rt.Hintf("Connection lost (%v): reconnecting.", err) }
				err = rtm.Stream(ctx, r.Client, opts, func(raw map[string]any) error { return w.handle(ctx, raw) })
				if err != nil && connected && downSince.IsZero() {
					// Without the monotonic reading, which stops while
					// the machine sleeps.
					downSince = time.Now().Round(0)
				}
				return struct{}{}, err
			})
			// Being interrupted, or running out of --for, is how the
			// command ends, not a failure: whatever call that cut short
			// (a channel being resolved, a name looked up, the stream)
			// is not an error to report.
			if ctx.Err() != nil {
				return nil
			}
			return err
		},
	}
	clictx.AddWorkspaceFlag(cmd, &workspace)
	cmd.Flags().StringSliceVar(&only, "channel", nil, "Print only the messages of these conversations (#name, name or ID); repeat the flag or separate them with commas")
	cmd.Flags().DurationVar(&duration, "for", 0, "Stop after this long (30s, 10m, 2h) instead of running until interrupted")
	cmd.Flags().BoolVar(&text, "text", false, "Print one readable line per message instead of JSON")
	cmd.Flags().IntVar(&maxBodyChars, "max-body-chars", defaultMaxBodyChars, "Max content characters per message (-1 for unlimited)")
	cmd.Flags().BoolVar(&includeSystem, "include-system", false, "Print system messages too (joins, leaves, topic changes, etc.)")
	return clictx.NoTimeout(cmd)
}

// watcher turns the events of the stream into printed messages.
type watcher struct {
	rt     *clictx.Context
	client *slackapi.Client

	maxBodyChars  int
	includeSystem bool
	text          bool
	// only holds the IDs of the conversations to print; empty means all.
	only []string

	// Names already looked up, by ID. They are kept for the run: a
	// renamed channel keeps its old name until the next one.
	channelNames, userNames map[string]string
	// namesPausedUntil is when names may be looked up again after Slack
	// rate limited a lookup.
	namesPausedUntil time.Time
	// unnamedUntil is, by channel or user ID, when a name that could not
	// be found may be looked up again: without it, each message of a
	// conversation or of an author Slack gives no name for (a channel of
	// another organisation, a deleted user) would cost a call.
	unnamedUntil map[string]time.Time
}

// namesPause is how long no name is looked up after a rate limit, and how
// long one name is not looked up again after it could not be found.
const namesPause = time.Minute

func (w *watcher) resolveFilter(ctx context.Context, only []string) error {
	for _, input := range only {
		id, err := channels.ResolveID(ctx, w.client, input)
		if err != nil {
			return err
		}
		w.only = append(w.only, id)
	}
	return nil
}

// handle prints raw if it is a new message.
func (w *watcher) handle(ctx context.Context, raw map[string]any) error {
	if jsonx.Str(raw["type"]) != "message" || jsonx.Bool(raw["hidden"]) {
		return nil
	}
	subtype := jsonx.Str(raw["subtype"])
	if slices.Contains(hiddenSubtypes, subtype) || (!w.includeSystem && messages.IsSystem(subtype)) {
		return nil
	}
	ev := Event{
		ChannelID:   jsonx.Str(raw["channel"]),
		ChannelType: channelType(raw),
		TS:          jsonx.Str(raw["ts"]),
		Content:     truncate(slacktext.RenderMessageContent(raw), w.maxBodyChars),
		Subtype:     subtype,
	}
	if ev.ChannelID == "" || ev.TS == "" {
		return nil
	}
	if len(w.only) > 0 && !slices.Contains(w.only, ev.ChannelID) {
		return nil
	}
	ev.Time = messages.FormatTime(ev.TS, nil)
	if threadTS := jsonx.Str(raw["thread_ts"]); threadTS != ev.TS {
		ev.ThreadTS = threadTS
	}

	name, err := w.name(ctx, w.channelNames, ev.ChannelID, func() (string, error) {
		return channels.LookupName(ctx, w.client, ev.ChannelID)
	})
	if err != nil {
		return err
	}
	ev.ChannelName = name

	if user, bot := jsonx.Str(raw["user"]), jsonx.Str(raw["bot_id"]); user != "" || bot != "" {
		ev.Author = &messages.Author{UserID: user, BotID: bot}
		if user != "" {
			ev.Author.Name, err = w.name(ctx, w.userNames, user, func() (string, error) { return w.lookupUser(ctx, user) })
			if err != nil {
				return err
			}
		}
		if ev.Author.Name == "" {
			ev.Author.Name = cmp.Or(jsonx.GetStr(raw, "bot_profile", "name"), jsonx.Str(raw["username"]))
		}
	}

	if w.text {
		_, err := fmt.Fprintln(w.rt.Stdout, textLine(ev))
		return err
	}
	return w.rt.Print(ev)
}

// name returns the name of id, from cache or from lookup. A name is an
// enrichment (see package enrich): the message is printed without it when
// the lookup fails, and the error is only that of a lookup that must end
// the command (cancelled context, dead credentials).
func (w *watcher) name(ctx context.Context, cache map[string]string, id string, lookup func() (string, error)) (string, error) {
	if name, ok := cache[id]; ok {
		return name, nil
	}
	if now := time.Now(); now.Before(w.namesPausedUntil) || now.Before(w.unnamedUntil[id]) {
		return "", nil
	}
	name, err := lookup()
	switch {
	case enrich.MustAbort(ctx, err):
		return "", err
	case enrich.IsRateLimit(err):
		w.namesPausedUntil = time.Now().Add(namesPause)
		w.rt.Hintf("Warning: Slack is rate limiting the name lookups: messages are printed with IDs only for %s.", namesPause)
	case err == nil && name != "":
		cache[id] = name
	default:
		w.unnamedUntil[id] = time.Now().Add(namesPause)
	}
	return name, nil
}

// lookupUser returns the name to call a user by, "" when Slack has none.
//
// users.ResolveByID takes a rate limit for an incomplete result: it
// returns no error and tells the collector of the context, which the
// command only prints when it ends. Here the lookups must pause instead,
// so the lookup gets a collector of its own and a rate limit is returned
// as the error it is.
func (w *watcher) lookupUser(ctx context.Context, id string) (string, error) {
	lookupCtx, incomplete := enrich.Collect(ctx)
	found, err := users.ResolveByID(lookupCtx, w.client, []string{id}, users.ResolveOptions{})
	u := found[id]
	name := cmp.Or(u.DisplayName, u.RealName, u.Name)
	if err == nil && name == "" && len(incomplete.List()) > 0 {
		return "", &slackapi.RateLimitError{Method: "users.info"}
	}
	return name, err
}

// channelType reads the type of conversation off a message event. Slack
// does not always say; a D… ID is always a DM.
func channelType(raw map[string]any) string {
	switch jsonx.Str(raw["channel_type"]) {
	case "im":
		return typeDM
	case "mpim":
		return typeMPIM
	case "channel", "group":
		return typeChannel
	}
	if strings.HasPrefix(jsonx.Str(raw["channel"]), "D") {
		return typeDM
	}
	return ""
}

// textLine renders ev for --text: "15:04:05 #general alice: hello", the
// following lines of the content indented.
func textLine(ev Event) string {
	var b strings.Builder
	if t, err := time.Parse(time.RFC3339, ev.Time); err == nil {
		b.WriteString(t.Format(time.TimeOnly) + " ")
	}
	sigil := "#"
	if ev.ChannelType == typeDM {
		sigil = "@"
	}
	b.WriteString(sigil + cmp.Or(ev.ChannelName, ev.ChannelID))
	if ev.ThreadTS != "" {
		b.WriteString(" (thread)")
	}
	author := "unknown"
	if a := ev.Author; a != nil {
		author = cmp.Or(a.Name, a.UserID, a.BotID)
	}
	b.WriteString(" " + author + ": ")
	b.WriteString(strings.ReplaceAll(ev.Content, "\n", "\n    "))
	// The line goes to a terminal as it is (JSON escapes them): without
	// its control characters, so that what someone wrote in Slack cannot
	// move the cursor, rewrite the lines above or set the title.
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			return -1
		}
		return r
	}, b.String())
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
