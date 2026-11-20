// Package channelcmd implements the "channel" command.
package channelcmd

import (
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/maelvls/slackcli/internal/appinfo"
	"github.com/maelvls/slackcli/internal/clictx"
	"github.com/maelvls/slackcli/internal/cmd/cmdutil"
	"github.com/maelvls/slackcli/internal/slack/channeladmin"
	"github.com/maelvls/slackcli/internal/slack/channels"
	"github.com/maelvls/slackcli/internal/slack/users"
)

// maxListLimit is the largest page Slack serves (the library clamps to
// it; the command refuses instead of truncating silently).
const maxListLimit = 1000

// New returns the "channel" command.
func New(rt *clictx.Context) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "channel",
		Short: "List and describe conversations, create channels, manage invites, and mark as read",
		Args:  cobra.NoArgs,
	}
	cmd.AddCommand(newListCommand(rt), newInfoCommand(rt), newNewCommand(rt), newInviteCommand(rt), newMarkCommand(rt))
	return cmd
}

// listResult is the output of "list": channels are channels.Conversation
// values, or Slack's own objects with --raw.
type listResult struct {
	Channels   any    `json:"channels"`
	NextCursor string `json:"next_cursor,omitempty"`
}

func newListCommand(rt *clictx.Context) *cobra.Command {
	var (
		workspace, user, cursor, name string
		all, viaCounts, raw           bool
		limit                         int
	)
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List conversations for a user (or current user), or all conversations",
		Long: `List conversations: public and private channels, DMs and group DMs;
archived channels are left out.

By default, lists the conversations you are a member of
(users.conversations); with --user, those of another user that you can
see; with --all, every conversation visible to you (conversations.list);
with --via-counts, your conversations as the Slack client lists them.

Prints {"channels": […], "next_cursor": …}, where a channel is {id, name,
user, is_private, is_im, is_mpim, is_archived, num_members}: a flag is
present only when true, a DM has "user" (the other member) instead of
"name". --raw prints Slack's conversation objects instead (topic, purpose,
creator, …), which are some twenty times larger. "channels" is [] when
there are none. One call returns one page; next_cursor is present when
there are more: pass it to --cursor, with the same other flags.

To find a channel whose exact name you do not know, pass --name <text>:
the whole listing is then read, and the conversations whose name contains
<text> are printed (at most --limit). A channel whose exact name you know
needs no lookup: commands take #name.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			switch {
			case all && user != "":
				return errors.New("--all cannot be used with --user")
			case viaCounts && all:
				return errors.New("--via-counts cannot be used with --all")
			case viaCounts && user != "":
				return errors.New("--via-counts cannot be used with --user")
			case cmd.Flags().Changed("name") && strings.Trim(strings.TrimSpace(name), "#") == "":
				return errors.New("--name is empty: pass part of a channel name, or drop --name to list every conversation")
			case name != "" && cursor != "":
				return errors.New("--name cannot be used with --cursor: it reads the whole listing at once")
			}
			if err := cmdutil.CheckLimit(limit, maxListLimit, "use --cursor with next_cursor to get more"); err != nil {
				return err
			}
			ctx := cmd.Context()
			ws := rt.EffectiveWorkspace(workspace)
			payload, err := clictx.WithAutoRefresh(ctx, rt, func() (channels.ConversationsPage, error) {
				var none channels.ConversationsPage
				r, err := rt.Client(ctx, ws)
				if err != nil {
					return none, err
				}
				opts := channels.ListOptions{Limit: limit, Cursor: cursor}
				list := channels.ListUser
				switch {
				case viaCounts:
					if !r.Auth.IsBrowser() {
						return none, fmt.Errorf(`--via-counts uses Slack's internal client.counts API, which only works with browser session credentials, not with a bot or user token (xoxb-/xoxp-): drop --via-counts, or import browser credentials with "%s auth import-desktop"`, appinfo.Name)
					}
					list = channels.ListViaCounts
				case all:
					list = channels.ListAll
				case user != "":
					if opts.User, err = users.ResolveUserID(ctx, r.Client, user); err != nil {
						return none, err
					}
				}
				if name != "" {
					return channels.FindByName(ctx, r.Client, list, opts, name)
				}
				return list(ctx, r.Client, opts)
			})
			if err != nil {
				return err
			}
			for _, w := range payload.Warnings {
				rt.Warnf("%s", w)
			}
			if name != "" {
				switch n := len(payload.Channels); {
				case n > limit:
					payload.Channels = payload.Channels[:limit]
					rt.ResultHintf("%d conversations match --name %q, the first %d are listed: make --name more specific, or raise --limit.", n, name, limit)
				case n == 0 && !all:
					rt.ResultHintf("No conversation in this listing has %q in its name. It only has the conversations you are a member of: add --all to look at every public channel.", name)
				case n == 0:
					rt.ResultHintf("No conversation visible to you has %q in its name.", name)
				}
			}
			result := listResult{Channels: channels.Compact(payload.Channels), NextCursor: payload.NextCursor}
			if raw {
				result.Channels = payload.Channels
			}
			return cmdutil.PrintList(rt, result, "channels")
		},
	}
	clictx.AddWorkspaceFlag(cmd, &workspace)
	cmd.Flags().StringVar(&user, "user", "", "List the conversations of this user instead of yours: user ID (U.../W...), email, or @handle/handle")
	cmd.Flags().BoolVar(&all, "all", false, "List all conversations (conversations.list); incompatible with --user")
	cmd.Flags().BoolVar(&viaCounts, "via-counts", false, "List joined conversations via client.counts, or users.counts where it is restricted too (works on Enterprise Grid where users.conversations/conversations.list are restricted for browser tokens; needs browser credentials; one conversations.info call per conversation listed; incompatible with --all/--user)")
	cmd.Flags().StringVar(&name, "name", "", "Only the conversations whose name contains this text, ignoring case, looked for in the whole listing rather than in one page (incompatible with --cursor)")
	cmd.Flags().BoolVar(&raw, "raw", false, "Print Slack's conversation objects (topic, purpose, creator, properties, …) instead of the compact ones")
	cmd.Flags().IntVar(&limit, "limit", 100, fmt.Sprintf("Max conversations in one page, or with --name max conversations printed (1-%d)", maxListLimit))
	cmd.Flags().StringVar(&cursor, "cursor", "", "Pagination cursor for the next page: the next_cursor value of the previous page (with --via-counts it is a numeric offset)")
	return cmd
}

func newNewCommand(rt *clictx.Context) *cobra.Command {
	var (
		workspace, name string
		private         bool
	)
	cmd := &cobra.Command{
		Use:   "new",
		Short: "Create a new channel",
		Long: `Create a new channel. You become its only member: add people with
"channel invite".

Prints {"channel": {"id": …, "name": …, "is_private": …}}. This is a write
action that others see: it is refused in safe mode.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := rt.CheckSafeMode("channel new"); err != nil {
				return err
			}
			// Checked before any credential lookup: it needs no network.
			if _, err := channeladmin.NormalizeName(name); err != nil {
				return err
			}
			ctx := cmd.Context()
			ws := rt.EffectiveWorkspace(workspace)
			payload, err := clictx.WithAutoRefresh(ctx, rt, func() (channeladmin.Created, error) {
				r, err := rt.Client(ctx, ws)
				if err != nil {
					return channeladmin.Created{}, err
				}
				return channeladmin.Create(ctx, r.Client, name, private)
			})
			if err != nil {
				return err
			}
			return rt.Print(payload)
		},
	}
	cmd.Flags().StringVar(&name, "name", "", fmt.Sprintf(`Channel name (required): lowercase letters, digits, hyphens and underscores, at most %d characters; a leading "#" is ignored`, channeladmin.MaxNameLength))
	_ = cmd.MarkFlagRequired("name")
	cmd.Flags().BoolVar(&private, "private", false, "Create as a private channel")
	clictx.AddWorkspaceFlag(cmd, &workspace)
	return cmd
}
