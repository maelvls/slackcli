// Package usercmd implements the "user" command.
package usercmd

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/maelvls/slackcli/internal/clictx"
	"github.com/maelvls/slackcli/internal/cmd/cmdutil"
	"github.com/maelvls/slackcli/internal/slack/users"
)

// maxListLimit is the most users one `user list` call returns (the
// library clamps to it; the command refuses instead of truncating
// silently).
const maxListLimit = 1000

const userArgHelp = `A user is given as a user ID (U…/W…, no lookup needed), an email address,
a @handle (the "@" is optional), or the exact display name or real name of
a single member. A handle is found with one message search when that member
posted in a conversation you can see (not with a bot token). Otherwise, and
for names, the whole user directory is scanned, which is slow in large
workspaces and fails (rate limited) in huge ones: prefer IDs or emails.`

// New returns the "user" command.
func New(rt *clictx.Context) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "user",
		Short: "Workspace user directory",
		Args:  cobra.NoArgs,
	}
	cmd.AddCommand(newListCommand(rt), newSearchCommand(rt), newGetCommand(rt), newDMOpenCommand(rt))
	return cmd
}

func newListCommand(rt *clictx.Context) *cobra.Command {
	var (
		workspace, cursor string
		limit             int
		includeBots       bool
	)
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List users in the workspace",
		Long: `List users in the workspace, deactivated ones included (they have
"deleted": true).

Prints {"users": […], "next_cursor": …}. Each user has id, name (the
handle), real_name, display_name, email, title, tz, is_bot, deleted,
status_text, status_emoji, status_expiration, and dm_id when you already
have a DM with them. "users" is [] when the page is empty. next_cursor is
present when there are more users: pass it to --cursor.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := cmdutil.CheckLimit(limit, maxListLimit, "use --cursor with next_cursor to get more"); err != nil {
				return err
			}
			ctx := cmd.Context()
			ws := rt.EffectiveWorkspace(workspace)
			payload, err := clictx.WithAutoRefresh(ctx, rt, func() (users.UsersPage, error) {
				r, err := rt.Client(ctx, ws)
				if err != nil {
					return users.UsersPage{}, err
				}
				return users.ListUsers(ctx, r.Client, users.ListOptions{Limit: limit, Cursor: cursor, IncludeBots: includeBots})
			})
			if err != nil {
				return err
			}
			return cmdutil.PrintList(rt, payload, "users")
		},
	}
	clictx.AddWorkspaceFlag(cmd, &workspace)
	cmd.Flags().IntVar(&limit, "limit", 200, fmt.Sprintf("Max users to return (1-%d); bots that are filtered out do not count", maxListLimit))
	cmd.Flags().StringVar(&cursor, "cursor", "", "Pagination cursor: the next_cursor value of the previous page")
	cmd.Flags().BoolVar(&includeBots, "include-bots", false, "Include bot users")
	return cmd
}

func newSearchCommand(rt *clictx.Context) *cobra.Command {
	var (
		workspace string
		limit     int
	)
	cmd := &cobra.Command{
		Use:   "search <query>",
		Short: "Find users by part of a name, a handle or a title",
		Long: `Find the users a few words designate: part of a name or of a handle, a
title ("jane", "jane doe", "doe engineer"). Use it when you do not know
the exact handle or name that "user get" needs; several words may also be
given as several arguments.

It asks Slack's people search, which answers in one call and ranks the
candidates; it wants whole words or their beginning, spelled right, and
leaves out bots. With a bot token, or where Slack refuses that search, the
user directory is scanned instead for the active members, bots left out,
in whose handle, names, email or title every word is found, ignoring case
and accents: slow in large workspaces. "user get" finds a bot or a
deactivated member by handle, email or ID.

Prints {"users": […], "total": …}, best match first. Each user has id,
name (the handle), real_name, display_name and title; a directory scan
adds email and tz, which the people search does not give ("user get <id>"
does). "users" is [] when nobody matches. total is how many members
match, when Slack says: when it is more than the users listed, make the
query more specific or raise --limit (100 at most).`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := cmdutil.CheckLimit(limit, users.MaxSearchLimit, "make the query more specific instead"); err != nil {
				return err
			}
			query := strings.Join(args, " ")
			ctx := cmd.Context()
			ws := rt.EffectiveWorkspace(workspace)
			payload, err := clictx.WithAutoRefresh(ctx, rt, func() (users.SearchResult, error) {
				r, err := rt.Client(ctx, ws)
				if err != nil {
					return users.SearchResult{}, err
				}
				return users.SearchUsers(ctx, r.Client, query, limit)
			})
			if err != nil {
				return err
			}
			for _, w := range payload.Warnings {
				rt.Hintf("Warning: %s", w)
			}
			if n := len(payload.Users); payload.Total > n {
				advice := "make the query more specific, or raise --limit"
				if limit == users.MaxSearchLimit {
					advice = "make the query more specific"
				}
				rt.Hintf("%d users match %q, the first %d are listed: %s.", payload.Total, query, n, advice)
			}
			return cmdutil.PrintList(rt, payload, "users")
		},
	}
	clictx.AddWorkspaceFlag(cmd, &workspace)
	cmd.Flags().IntVar(&limit, "limit", 20, fmt.Sprintf("Max users to return (1-%d)", users.MaxSearchLimit))
	return cmd
}

func newGetCommand(rt *clictx.Context) *cobra.Command {
	var workspace string
	cmd := &cobra.Command{
		Use:   "get <user>",
		Short: "Get a single workspace user",
		Long: `Get a single workspace user.

` + userArgHelp + `

Prints the user: id, name (the handle), real_name, display_name, email,
title, tz, is_bot, deleted, status_text, status_emoji, status_expiration.
Fails when nobody matches, or when a name designates several members (the
error lists their IDs).`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			ws := rt.EffectiveWorkspace(workspace)
			payload, err := clictx.WithAutoRefresh(ctx, rt, func() (users.CompactUser, error) {
				r, err := rt.Client(ctx, ws)
				if err != nil {
					return users.CompactUser{}, err
				}
				return users.GetUser(ctx, r.Client, args[0])
			})
			if err != nil {
				return err
			}
			return rt.Print(payload)
		},
	}
	clictx.AddWorkspaceFlag(cmd, &workspace)
	return cmd
}

func newDMOpenCommand(rt *clictx.Context) *cobra.Command {
	var workspace string
	cmd := &cobra.Command{
		Use:   "dm-open <users...>",
		Short: "Open or get a DM / group DM channel",
		Long: `Open the DM with one user, or the group DM with several, and print its
channel ID. An existing conversation is reused; nothing is posted.

<users...>: One to 8 other users; the caller is implicit.

` + userArgHelp + `

Prints {"user_ids": […], "dm_channel_id": "D…"|"G…", "channel_type":
"dm"|"group_dm", "already_open": true|false}. Use dm_channel_id as the
target of other commands. already_open is what Slack reports: true when
the conversation was already open for you, false when this call opened it
(a new conversation, or an existing one that was closed).`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			ws := rt.EffectiveWorkspace(workspace)
			payload, err := clictx.WithAutoRefresh(ctx, rt, func() (users.DMChannel, error) {
				r, err := rt.Client(ctx, ws)
				if err != nil {
					return users.DMChannel{}, err
				}
				return users.GetDMChannelForUsers(ctx, r.Client, args)
			})
			if err != nil {
				return err
			}
			return rt.Print(payload)
		},
	}
	clictx.AddWorkspaceFlag(cmd, &workspace)
	return cmd
}
