package channelcmd

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/maelvls/slackcli/internal/clictx"
	"github.com/maelvls/slackcli/internal/cmd/cmdutil"
	"github.com/maelvls/slackcli/internal/slack/channels"
)

func newInfoCommand(rt *clictx.Context) *cobra.Command {
	var (
		workspace string
		raw       bool
	)
	cmd := &cobra.Command{
		Use:   "info <target>",
		Short: "Describe one conversation: name, topic, purpose, member count",
		Long: `Describe one conversation (conversations.info).

<target> is a channel (#name, name or ID), a Slack message or channel URL
(its channel), or a user ID (U…/W…: the DM you have with that user; none
is opened, and it fails when there is none).

Prints {id, name, user, is_private, is_im, is_mpim, is_archived,
is_member, num_members, topic, purpose, creator, created}: a flag is
present only when true, a DM has "user" (the other member) instead of
"name", "created" is a Unix time in seconds. --raw prints Slack's
conversation object instead.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			target, err := cmdutil.ParseTarget(rt, args[0], workspace)
			if err != nil {
				return err
			}
			target.ReadOnly = true
			ctx := cmd.Context()
			ch, err := cmdutil.WithTarget(ctx, rt, target, func(loc cmdutil.Located) (map[string]any, error) {
				return channels.Info(ctx, loc.Client, loc.ChannelID)
			})
			var noDM *channels.NoDMError
			if errors.As(err, &noDM) {
				// The error itself is worded for reading messages.
				return fmt.Errorf("there is no direct message with user %s: nothing was ever written there, so there is no conversation to describe", noDM.UserID)
			}
			if err != nil {
				return err
			}
			if raw {
				return rt.Print(ch)
			}
			return rt.Print(channels.Describe(ch))
		},
	}
	cmd.Flags().BoolVar(&raw, "raw", false, "Print Slack's conversation object (properties, tabs, last_read, …) instead of the compact one")
	cmdutil.AddTargetWorkspaceFlag(cmd, &workspace)
	return cmd
}
