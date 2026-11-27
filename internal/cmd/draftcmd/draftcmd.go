// Package draftcmd implements "message draft": Slack-native drafts, which
// appear in the user's own Slack client for them to review and send.
package draftcmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/maelvls/slackcli/internal/appinfo"
	"github.com/maelvls/slackcli/internal/clictx"
	"github.com/maelvls/slackcli/internal/cmd/cmdutil"
)

const browserAuthNote = `Drafts need browser auth (xoxc token + xoxd cookie, e.g. from "auth
import-desktop"): Slack refuses bot and user tokens here.`

const targetHelp = `<target> is one of:
  a Slack message URL       draft a reply in the thread of that message
  #name, name, channel ID   draft a message to that channel (a reply with
                            --thread-ts)
  a user ID (U…/W…)         draft a direct message to that user`

// New returns the "draft" command, to be added under "message".
func New(rt *clictx.Context) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "draft",
		Short: "Manage Slack-native message drafts (appear in the user's Slack client; requires browser auth)",
		Long: `Manage Slack-native message drafts. A draft is not posted: it shows up in
the user's Slack client (Drafts & sent, and the composer of its channel),
where they can edit and send it.

` + browserAuthNote + `

Drafts are the user's own, unposted state: safe mode does not cover them.

` + fmt.Sprintf(`To write a message in a browser editor instead, see
"%s message compose".`, appinfo.Name),
		// "message draft <target> [text]" is how the predecessor opened
		// the browser editor: say where that went, on top of the usual
		// "unknown command".
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return nil
			}
			return fmt.Errorf(`unknown command %[1]q for %[2]q: its commands are list, create, update and delete. To write to %[1]s in the browser editor, run: %[3]s message compose %[1]q [text]`,
				args[0], cmd.CommandPath(), appinfo.Name)
		},
		RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	cmd.AddCommand(newList(rt), newCreate(rt), newUpdate(rt), newDelete(rt))
	return cmd
}

func newList(rt *clictx.Context) *cobra.Command {
	var opts listOptions
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List unsent message drafts",
		Long: `List the user's unsent drafts.

Prints {ok, drafts: [{id, text, destinations: [{channel_id, channel_name,
thread_ts, broadcast}], last_updated_ts, date_created, date_scheduled,
file_ids}], count}. "drafts" is [] when there are none.

` + browserAuthNote,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := cmdutil.CheckOptionalLimit(cmd, opts.limit); err != nil {
				return err
			}
			out, err := list(cmd.Context(), rt, opts)
			if err != nil {
				return err
			}
			return cmdutil.PrintList(rt, out, "drafts")
		},
	}
	clictx.AddWorkspaceFlag(cmd, &opts.workspace)
	cmd.Flags().IntVar(&opts.limit, "limit", 0, "Max drafts to return (positive integer; default: all that Slack returns)")
	cmd.Flags().BoolVar(&opts.all, "all", false, "Include sent/inactive drafts")
	return cmd
}

func newCreate(rt *clictx.Context) *cobra.Command {
	var opts createOptions
	cmd := &cobra.Command{
		Use:   "create <target> <text>",
		Short: "Create a Slack-native draft addressed to a channel, DM, or thread",
		Long: `Create a Slack-native draft addressed to a channel, DM, or thread. Nothing
is posted: the draft waits in the user's Slack client.

` + targetHelp + `

With a message URL, --thread-ts is redundant: it may repeat the thread of
that message but not name another one.

<text> is the draft text in Slack mrkdwn (*bold*, _italic_, ` + "`code`" + `, lists,
> quotes, <url|label>). It may be "" when --attach is given. Text of several
words may start with a dash; a single word that looks like a flag ("-1")
must come after "--".

Prints {ok, draft: {id, text, destinations, last_updated_ts, date_created,
file_ids}}.

With --attach the command does not time out, because uploads can be long
(an upload that makes no progress still fails). If the draft cannot be
created, the files uploaded for it are deleted.

` + browserAuthNote,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(opts.attach) > 0 {
				// An upload may take longer than the command timeout, and
				// timing out between the upload and drafts.create would
				// orphan the uploaded files. The upload has its own stall
				// timeout.
				rt.DisableTimeout()
			}
			out, err := create(cmd.Context(), rt, args[0], args[1], opts)
			if err != nil {
				return err
			}
			return rt.Print(out)
		},
	}
	cmdutil.AddTargetWorkspaceFlag(cmd, &opts.workspace)
	cmd.Flags().StringVar(&opts.threadTS, "thread-ts", "", "Thread root ts to draft a reply into, for channel and user-ID targets; redundant with a message URL target, which already says which thread and which it must not contradict")
	cmd.Flags().BoolVar(&opts.broadcast, "broadcast", false, "Also send the thread reply to the channel when posted (requires thread context; not for DMs)")
	cmd.Flags().StringArrayVar(&opts.attach, "attach", nil, "Attach a local file to the draft (repeatable)")
	// <text> may start with "- ".
	return cmdutil.AllowDashText(cmd)
}

func newUpdate(rt *clictx.Context) *cobra.Command {
	var (
		opts                   updateOptions
		broadcast, noBroadcast bool
	)
	cmd := &cobra.Command{
		Use:   "update <draft-id> <text>",
		Short: "Replace a draft's text (and optionally re-address it)",
		Long: `Replace a draft's text, keeping its destination, thread, broadcast flag and
attached files unless flags say otherwise.

<draft-id> is the id returned by "draft list" or "draft create".
<text> is the new draft text (Slack mrkdwn); it replaces the existing body.

--attach ADDS files to those already attached; it never removes any (remove
attachments in the Slack client). Changing the thread or the channel resets
the broadcast flag to what the flags ask for.

Refused: drafts with a scheduled send time, and (without --channel) drafts
addressed to several destinations. Rewriting them would silently drop the
schedule or the extra recipients.

Prints {ok, draft: {id, text, destinations, last_updated_ts, date_created,
file_ids}}.

With --attach the command does not time out, because uploads can be long
(an upload that makes no progress still fails). If the draft cannot be
updated, the files newly uploaded for it are deleted.

` + browserAuthNote,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			switch {
			case cmd.Flags().Changed("no-broadcast") && noBroadcast:
				off := false
				opts.broadcast = &off
			case cmd.Flags().Changed("broadcast"):
				opts.broadcast = &broadcast
			}
			if len(opts.attach) > 0 {
				rt.DisableTimeout() // see newCreate
			}
			out, err := update(cmd.Context(), rt, args[0], args[1], opts)
			if err != nil {
				return err
			}
			return rt.Print(out)
		},
	}
	cmdutil.AddTargetWorkspaceFlag(cmd, &opts.workspace)
	cmd.Flags().StringVar(&opts.channel, "channel", "", "Re-address the draft to a different channel (#name or id), DM (user id) or thread (message URL)")
	cmd.Flags().StringVar(&opts.threadTS, "thread-ts", "", "Thread root ts to draft a reply into; redundant with a message URL given to --channel, which it must not contradict")
	cmd.Flags().BoolVar(&broadcast, "broadcast", false, "Also send the thread reply to the channel when posted (requires thread context; not for DMs)")
	cmd.Flags().BoolVar(&noBroadcast, "no-broadcast", false, "Clear an inherited broadcast flag (keeps the thread)")
	cmd.MarkFlagsMutuallyExclusive("broadcast", "no-broadcast")
	cmd.Flags().StringArrayVar(&opts.attach, "attach", nil, "Attach one more local file to the draft, in addition to the files it already has (repeatable)")
	cmd.Flags().StringVar(&opts.lastUpdatedTS, "last-updated-ts", "",
		"The draft's last_updated_ts as you last read it: the update is refused if the draft changed since (default: the current value, i.e. no conflict detection)")
	// <text> may start with "- ".
	return cmdutil.AllowDashText(cmd)
}

func newDelete(rt *clictx.Context) *cobra.Command {
	var opts deleteOptions
	cmd := &cobra.Command{
		Use:   "delete <draft-id>",
		Short: "Delete a draft",
		Long: `Delete a draft.

<draft-id> is the id returned by "draft list" or "draft create".

Prints {ok, draft_id}.

` + browserAuthNote,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			out, err := remove(cmd.Context(), rt, args[0], opts)
			if err != nil {
				return err
			}
			return rt.Print(out)
		},
	}
	clictx.AddWorkspaceFlag(cmd, &opts.workspace)
	cmd.Flags().StringVar(&opts.lastUpdatedTS, "last-updated-ts", "",
		"The draft's last_updated_ts as you last read it, for Slack's conflict detection (default: looked up, i.e. delete whatever the draft now is)")
	return cmd
}
