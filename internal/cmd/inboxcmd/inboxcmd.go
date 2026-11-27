// Package inboxcmd implements the "inbox" command.
package inboxcmd

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/maelvls/slackcli/internal/clictx"
	"github.com/maelvls/slackcli/internal/cmd/cmdutil"
	"github.com/maelvls/slackcli/internal/slack/inbox"
)

// New returns the "inbox" command.
func New(rt *clictx.Context) *cobra.Command {
	var (
		workspace, since               string
		maxBodyChars, maxConversations int
		noThreads                      bool
	)
	cmd := &cobra.Command{
		Use:   "inbox",
		Short: "Show what is waiting on you: unanswered DMs, @mentions and thread replies",
		Long: `Show the conversations where someone is waiting for your answer:

  dm, mpim  DMs and group DMs whose newest message is not yours
  mention   @mentions of you that you have not answered in the thread (or,
            for a top-level mention nobody replied to, later in the channel)
  thread    new replies in threads you posted in (also after a mention
            you answered)

"unreads" is about what you have not read; this is about what you have not
answered, read or not. Your last message, your reaction or (for a top-level
message) your reply in its thread answers everything before it. Bots,
Slackbot, system messages and the DM with yourself are ignored.

Prints JSON:
  user_id    you
  since      start of the window, in UTC
  truncated  true when there was more to look at than was looked at: more
             candidates than --max-conversations (raise it), or more
             matches than the 200 read from each search, or a
             conversation with more than 1000 messages in the window, whose
             item may undercount or be answered already (only a shorter
             --since helps with these two)
  items      newest first, each with:
    kind          dm, mpim, mention or thread
    channel_id, channel_name
                  for a DM, channel_name is the name of the other user
    thread_ts     the thread root (the message itself for a top-level
                  mention); absent for the top level of a DM or group DM.
                  A mention inside a DM is a dm item; a thread that is both
                  yours and mentions you is one mention item.
    count         number of messages from others since your last message
                  there
    first_ts      the oldest of them
    ts, author {user_id, name}, text (as Markdown), permalink
                  the newest of them

The candidates come from Slack's search (messages to you, mentions of you,
your own thread posts), which a bot token cannot use; each one is then read
to see whether it still waits. Nothing is marked as read.

The search goes back to two days before --since, for your own thread posts
too: a thread whose last post of yours is older than that is not found,
however recent its replies, unless one of them mentions you.

To wait until something new comes instead of asking again: "inbox wait".`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			start, err := inbox.ParseSince(since, time.Now())
			if err != nil {
				return err
			}
			if err := cmdutil.CheckMaxChars("--max-body-chars", maxBodyChars); err != nil {
				return err
			}
			if err := checkMaxConversations(maxConversations); err != nil {
				return err
			}
			ctx := cmd.Context()
			ws := rt.EffectiveWorkspace(workspace)
			payload, err := clictx.WithAutoRefresh(ctx, rt, func() (inbox.Result, error) {
				r, err := rt.Client(ctx, ws)
				if err != nil {
					return inbox.Result{}, err
				}
				return inbox.Fetch(ctx, r.Client, inbox.Options{
					Since:            start,
					MaxBodyChars:     maxBodyChars,
					MaxConversations: maxConversations,
					NoThreads:        noThreads,
				})
			})
			if err != nil {
				return err
			}
			return cmdutil.PrintList(rt, payload, "items")
		},
	}
	clictx.AddWorkspaceFlag(cmd, &workspace)
	cmd.Flags().StringVar(&since, "since", inbox.DefaultSince, "Start of the window: a number of days or hours (3d, 12h), or a day (2026-01-31, from 00:00 local time)")
	cmd.Flags().IntVar(&maxBodyChars, "max-body-chars", inbox.DefaultMaxBodyChars, "Max text characters per item (-1 for unlimited)")
	cmd.Flags().IntVar(&maxConversations, "max-conversations", inbox.DefaultMaxConversations, "Max conversations and threads to inspect, the most recently active first; when there are more, truncated is true")
	cmd.Flags().BoolVar(&noThreads, "no-threads", false, "Leave out the new replies in threads you posted in (kind thread)")
	cmd.AddCommand(newWait(rt))
	return cmd
}

// checkMaxConversations rejects a --max-conversations below 1.
func checkMaxConversations(n int) error {
	if n < 1 {
		return fmt.Errorf("invalid --max-conversations value %d: must be a positive integer", n)
	}
	return nil
}
