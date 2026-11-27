package messagecmd

import (
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/maelvls/slackcli/internal/appinfo"
	"github.com/maelvls/slackcli/internal/clictx"
	"github.com/maelvls/slackcli/internal/cmd/cmdutil"
	"github.com/maelvls/slackcli/internal/slack/scheduled"
)

type scheduledListOptions struct {
	workspace string
	channel   string
	oldest    string
	latest    string
	cursor    string
	// limit is 0 when --limit was not given: Slack's default.
	limit int
}

// cancelResult is the output of "scheduled cancel".
type cancelResult struct {
	OK                 bool   `json:"ok"`
	ChannelID          string `json:"channel_id"`
	ScheduledMessageID string `json:"scheduled_message_id"`
}

const scheduledChannelHelp = "a channel (#name, name or ID), a user ID (its DM) or a Slack URL of the conversation"

func addScheduledListFlags(cmd *cobra.Command, o *scheduledListOptions) {
	f := cmd.Flags()
	f.StringVar(&o.workspace, "workspace", "", cmdutil.TargetWorkspaceUsage)
	f.StringVar(&o.channel, "channel", "", "Only messages scheduled in this conversation: "+scheduledChannelHelp)
	f.StringVar(&o.oldest, "oldest", "", "Only messages scheduled to post after this Unix timestamp (seconds)")
	f.StringVar(&o.latest, "latest", "", "Only messages scheduled to post before this Unix timestamp (seconds)")
	f.StringVar(&o.cursor, "cursor", "", `Fetch the next page: the "next_cursor" of the previous output (app tokens only: a browser session gets everything at once)`)
	f.IntVar(&o.limit, "limit", 0, "Max scheduled messages per page (positive integer; default: Slack's, or all with a browser session)")
}

func (c *command) newScheduled() *cobra.Command {
	var opts scheduledListOptions
	cmd := &cobra.Command{
		Use:   "scheduled",
		Short: "List or cancel pending scheduled messages",
		Long: fmt.Sprintf(`List or cancel pending scheduled messages: with a browser session, the
ones scheduled in the Slack client; with an app token, the ones that token
scheduled with "%[1]s message send --schedule/--schedule-in".

"%[1]s message scheduled" is short for "%[1]s message scheduled list".`, appinfo.Name),
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return c.runScheduledList(cmd, opts) },
	}
	addScheduledListFlags(cmd, &opts)
	cmd.AddCommand(c.newScheduledList(), c.newScheduledCancel())
	return cmd
}

func (c *command) newScheduledList() *cobra.Command {
	var opts scheduledListOptions
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List pending scheduled messages",
		Long: `List pending scheduled messages.

Prints {ok, scheduled_messages: [{id, channel_id, post_at, date_created, text}],
next_cursor}. "scheduled_messages" is [] when there are none. "id" and
"channel_id" are what "scheduled cancel" needs; pass "next_cursor", when
present, as --cursor to get the next page.

With an app token (xoxb-/xoxp-), the list is chat.scheduledMessages.list:
the messages that token scheduled, not those scheduled in the Slack client.
Slack refuses that method to a browser session (the default credentials):
the list is then the messages scheduled in the Slack client, which are
drafts with a send time ("id" is a draft ID, "Dr…"), read with drafts.list,
soonest first. --channel, --oldest, --latest and --limit are applied to
what drafts.list returns; there is no next_cursor and --cursor is refused.
drafts.list returns at most 100 drafts: when Slack has more, "warnings"
says that scheduled messages may be missing.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return c.runScheduledList(cmd, opts) },
	}
	addScheduledListFlags(cmd, &opts)
	return cmd
}

func (c *command) runScheduledList(cmd *cobra.Command, opts scheduledListOptions) error {
	ctx := cmd.Context()
	if err := cmdutil.CheckOptionalLimit(cmd, opts.limit); err != nil {
		return err
	}
	listOpts := scheduled.ListOptions{
		Cursor: strings.TrimSpace(opts.cursor),
		Oldest: opts.oldest,
		Latest: opts.latest,
		Limit:  opts.limit,
	}

	var (
		result scheduled.ListResult
		err    error
	)
	if strings.TrimSpace(opts.channel) == "" {
		if cmd.Flags().Changed("channel") {
			return errors.New("--channel needs a value: " + scheduledChannelHelp)
		}
		result, err = clictx.WithAutoRefresh(ctx, c.rt, func() (scheduled.ListResult, error) {
			r, err := c.rt.Client(ctx, c.rt.EffectiveWorkspace(opts.workspace))
			if err != nil {
				return scheduled.ListResult{}, err
			}
			return scheduled.List(ctx, r.Client, listOpts)
		})
	} else {
		var target cmdutil.Target
		if target, err = cmdutil.ParseTarget(c.rt, opts.channel, opts.workspace); err != nil {
			return fmt.Errorf("--channel: %w", err)
		}
		result, err = cmdutil.WithTarget(ctx, c.rt, target, func(loc cmdutil.Located) (scheduled.ListResult, error) {
			listOpts := listOpts
			listOpts.ChannelID = loc.ChannelID
			return scheduled.List(ctx, loc.Client, listOpts)
		})
	}
	if err != nil {
		return err
	}
	if result.Incomplete {
		c.rt.Warnf("Slack has more drafts than the %d it returns at once, and gives no way to get the rest: scheduled messages may be missing from this list. The Slack client shows them all", scheduled.DraftsPageSize)
	}
	return cmdutil.PrintList(c.rt, result, "scheduled_messages")
}

func (c *command) newScheduledCancel() *cobra.Command {
	var workspace, channel string
	cmd := &cobra.Command{
		Use:   "cancel <id>",
		Short: "Cancel a pending scheduled message",
		Long: `Cancel a pending scheduled message, so that it is never sent.

<id> is the "id" printed by "message scheduled list", or the
scheduled_message_id printed by "message send --schedule". Slack refuses to
cancel a message due within the next 60 seconds.

With a browser session, a draft ID ("Dr…", which "list" prints for such a
session) is cancelled by deleting that draft (drafts.delete), as the Slack
client does; a draft that has no send time, or is scheduled in another
conversation than --channel, is refused. Any other ID, and every ID with an
app token, goes to chat.deleteScheduledMessage.

Prints {ok, channel_id, scheduled_message_id}. Refused in safe mode: a human
must decide that a message they scheduled is not to be sent after all.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := c.rt.CheckSafeMode("message scheduled cancel"); err != nil {
				return err
			}
			ctx := cmd.Context()
			id := strings.TrimSpace(args[0])
			if id == "" {
				return errors.New("<id> is empty: pass the scheduled_message_id of the message to cancel")
			}
			target, err := cmdutil.ParseTarget(c.rt, channel, workspace)
			if err != nil {
				return fmt.Errorf("--channel: %w", err)
			}
			result, err := cmdutil.WithTarget(ctx, c.rt, target, func(loc cmdutil.Located) (cancelResult, error) {
				if err := scheduled.Cancel(ctx, loc.Client, loc.ChannelID, id); err != nil {
					return cancelResult{}, err
				}
				return cancelResult{OK: true, ChannelID: loc.ChannelID, ScheduledMessageID: id}, nil
			})
			if err != nil {
				return err
			}
			return c.rt.Print(result)
		},
	}
	cmd.Flags().StringVar(&channel, "channel", "", "Required: the conversation the message was scheduled in ("+scheduledChannelHelp+`); it is the "channel_id" printed when scheduling`)
	cmdutil.AddTargetWorkspaceFlag(cmd, &workspace)
	// MarkFlagRequired only fails for a flag name that does not exist.
	_ = cmd.MarkFlagRequired("channel")
	return cmd
}
