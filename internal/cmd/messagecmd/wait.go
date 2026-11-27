package messagecmd

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/spf13/cobra"

	"github.com/maelvls/slackcli/internal/clictx"
	"github.com/maelvls/slackcli/internal/cmd/cmdutil"
	"github.com/maelvls/slackcli/internal/slack/messages"
	"github.com/maelvls/slackcli/internal/slack/slackurl"
	"github.com/maelvls/slackcli/internal/slack/users"
	"github.com/maelvls/slackcli/internal/slackapi"
)

const defaultWaitInterval = time.Minute

// wakeStep bounds each sleep between two polls: timers stand still while a
// Mac sleeps, so the wall clock is read again at least this often, and a
// poll that fell due during the sleep runs soon after waking up.
var wakeStep = 5 * time.Second

type waitOptions struct {
	readOptions
	after    string
	interval time.Duration
	timeout  time.Duration
}

// waitResult is the output of "wait": that of "list", plus timed_out.
type waitResult struct {
	ChannelID       string                       `json:"channel_id"`
	Messages        []messages.CompactMessage    `json:"messages"`
	ReferencedUsers map[string]users.CompactUser `json:"referenced_users"`
	// TimedOut is true when --timeout ran out with nothing new.
	TimedOut bool `json:"timed_out,omitempty"`
}

func (c *command) newWait() *cobra.Command {
	var opts waitOptions
	cmd := &cobra.Command{
		Use:   "wait <target>",
		Short: "Wait until someone posts after a message, then print what was posted",
		Long: `Wait until someone posts after a message, then print the new messages and
exit: the way to wait for the answer to a message you sent.

<target> is a Slack message URL (the permalink "message send" prints), or a
channel or a user ID (the DM with that user) combined with --ts.

New messages are those after the message (or after --after) in its thread,
and at the top level of its conversation, so that both a reply in the
thread and a new message in a DM count. Your own messages count too.
Messages already posted when the command starts are printed at once.

It asks Slack every --interval (1m by default), and keeps trying when
Slack cannot be reached (closed lid, network down, rate limit, Slack
failing): a note on stderr says so, once. Any other error ends it, after
the usual attempt to re-import expired credentials from Slack Desktop. It
has no time limit unless --timeout is given, and is not cut by the command
timeout.

Prints {channel_id, messages, referenced_users}, the messages being those
of "list", oldest first. When --timeout runs out with nothing new,
"messages" is [], "timed_out" is true, and a hint gives the --after to
pass to wait again. Exit codes: 0 in both cases, 1 on error, 130 when
interrupted.`,
		Example: `  slackcli message wait "https://acme.slack.com/archives/D0123456789/p1770165109628379"
  slackcli message wait "#general" --ts 1770165109.628379 --timeout 2h`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return c.runWait(cmd, args[0], opts)
		},
	}
	addReadFlags(cmd, &opts.readOptions)
	f := cmd.Flags()
	f.StringVar(&opts.ts, "ts", "", cmdutil.MessageTSUsage)
	f.StringVar(&opts.after, "after", "", "Only messages after this ts (by default, after the message itself): to wait again after a timeout")
	f.DurationVar(&opts.interval, "interval", defaultWaitInterval, "How long to wait between two polls (at least 1s)")
	f.DurationVar(&opts.timeout, "timeout", 0, "Give up after this long, such as 30m or 2h (0: wait until something is posted or the command is interrupted)")
	return clictx.NoTimeout(cmd)
}

func (c *command) runWait(cmd *cobra.Command, targetInput string, opts waitOptions) error {
	ctx := cmd.Context()
	target, err := cmdutil.ParseTarget(c.rt, targetInput, opts.workspace)
	if err != nil {
		return err
	}
	target.ReadOnly = true
	ts, err := target.MessageTS(opts.ts)
	if err != nil {
		return err
	}
	if err := cmdutil.CheckMaxBodyChars(cmd, opts.maxBodyChars); err != nil {
		return err
	}
	after, err := cmdutil.ParseTS("--after", opts.after)
	if err != nil {
		return err
	}
	after = cmp.Or(after, ts)
	if opts.interval < time.Second {
		return fmt.Errorf("--interval must be at least 1s, got %s", opts.interval)
	}
	if opts.timeout < 0 {
		return fmt.Errorf("--timeout must be a positive duration such as 30m or 2h, got %s", opts.timeout)
	}
	// The thread to watch: FetchThread accepts the ts of a reply too.
	rootTS := ts
	if target.Kind == slackurl.TargetURL {
		rootTS = cmp.Or(target.Ref.ThreadTSHint, ts)
	}

	// Wall-clock times (Round(0) drops the monotonic reading, which
	// stands still while a Mac sleeps): a 2h timeout is 2h of the day.
	var deadline time.Time
	if opts.timeout > 0 {
		deadline = c.now().Round(0).Add(opts.timeout)
	}
	channelID := target.Ref.ChannelID
	failing := false
	for {
		result, err := cmdutil.WithTarget(ctx, c.rt, target, func(loc cmdutil.Located) (waitResult, error) {
			msgs, err := fetchAfter(ctx, loc, rootTS, after, opts.includeReactions)
			if err != nil || len(msgs) == 0 {
				return waitResult{ChannelID: loc.ChannelID}, err
			}
			compact, referenced, err := c.present(ctx, loc, msgs, opts.readOptions)
			if err != nil {
				return waitResult{}, err
			}
			return waitResult{ChannelID: loc.ChannelID, Messages: compact, ReferencedUsers: referenced}, nil
		})
		switch {
		case err == nil:
			channelID = result.ChannelID
			if failing {
				c.rt.Hintf("Slack answers again.")
				failing = false
			}
			if len(result.Messages) > 0 {
				return cmdutil.PrintList(c.rt, result, "messages")
			}
		case ctx.Err() != nil:
			return err
		case !slackapi.IsTransient(err):
			return err
		case !failing:
			c.rt.Hintf("Cannot reach Slack, trying again every %s: %v", opts.interval, err)
			failing = true
		}

		pause := opts.interval
		if !deadline.IsZero() {
			left := deadline.Sub(c.now().Round(0))
			if left <= 0 {
				c.rt.ResultHintf("Nothing was posted after %s within %s: to wait again, run the same command with --after %s.", describeBound(after), opts.timeout, after)
				return cmdutil.PrintList(c.rt, waitResult{ChannelID: channelID, TimedOut: true}, "messages")
			}
			pause = min(pause, left)
		}
		if err := c.sleepUntil(ctx, c.now().Round(0).Add(pause)); err != nil {
			return err
		}
	}
}

// sleepUntil returns when the wall clock reaches t, or with the context's
// error.
func (c *command) sleepUntil(ctx context.Context, t time.Time) error {
	for {
		left := t.Sub(c.now().Round(0))
		if left <= 0 {
			return nil
		}
		timer := time.NewTimer(min(left, wakeStep))
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// fetchAfter returns the messages after the ts after, oldest first: those
// of the thread rootTS, and those at the top level of the conversation. A
// page of history holds at most MaxHistoryLimit messages, the newest ones:
// after a long wait in a busy channel, it pages back with Latest while
// older messages are left (HasMore), so that the first answer is not left
// out.
func fetchAfter(ctx context.Context, loc cmdutil.Located, rootTS, after string, includeReactions bool) ([]messages.Message, error) {
	all, err := messages.FetchThread(ctx, loc.Client, loc.ChannelID, rootTS, includeReactions)
	if err != nil {
		return nil, err
	}
	latest := ""
	for {
		page, err := messages.FetchChannelHistory(ctx, loc.Client, loc.ChannelID, messages.HistoryOptions{
			Limit:            messages.MaxHistoryLimit,
			Oldest:           after,
			Latest:           latest,
			IncludeReactions: includeReactions,
		})
		if err != nil {
			return nil, err
		}
		all = append(all, page.Messages...)
		if !page.HasMore || len(page.Messages) == 0 {
			break
		}
		latest = page.Messages[0].TS
	}

	seen := map[string]bool{}
	out := all[:0]
	for _, m := range all {
		if messages.CompareTS(m.TS, after) > 0 && !seen[m.TS] {
			seen[m.TS] = true
			out = append(out, m)
		}
	}
	slices.SortStableFunc(out, func(a, b messages.Message) int { return messages.CompareTS(a.TS, b.TS) })
	return out, nil
}
