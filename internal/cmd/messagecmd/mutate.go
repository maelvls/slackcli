package messagecmd

import (
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/maelvls/slackcli/internal/appinfo"
	"github.com/maelvls/slackcli/internal/cmd/cmdutil"
	"github.com/maelvls/slackcli/internal/slack/slacktext"
	"github.com/maelvls/slackcli/internal/slackapi"
)

// messageResult is the output of "message edit" and "message delete", and
// part of that of "react add" and "react remove": the message, by channel
// ID and ts (the same shape as "channel mark").
type messageResult struct {
	OK        bool   `json:"ok"`
	ChannelID string `json:"channel_id"`
	TS        string `json:"ts"`
}

// reactResult is the output of "react add" and "react remove". Already is
// set when the message was already as asked (the reaction was there for
// "add", absent for "remove"): a retry after a timeout succeeds.
type reactResult struct {
	messageResult
	Name    string `json:"name"`
	Already bool   `json:"already,omitempty"`
}

const messageTargetLong = `<target> is a Slack message URL, or a channel (#name, name, channel ID or
channel URL) combined with --ts.`

func (c *command) newEdit() *cobra.Command {
	var workspace, tsFlag, blocksPath string
	cmd := &cobra.Command{
		Use:   "edit <target> <text> [flags]",
		Short: "Edit a message",
		Long: fmt.Sprintf(`Edit one of your own messages: <text> replaces its whole content.

%s

<text> is Slack mrkdwn, as for "send": *bold*, _italic_,
<https://example.com|label>, <@U123> or @U123 mentions, lists. Markdown
links ([label](https://example.com)) are not converted: they stay literal
text. Text with links, mentions or formatting is sent as rich_text blocks,
because Slack leaves the links of an edited message's plain text unlinked.

With --blocks, pass "" as <text> to send the blocks alone. Prints
{ok, channel_id, ts}: the message edited.

Refused in safe mode (--safe-mode or %s=1).`, messageTargetLong, appinfo.EnvName("SAFE_MODE")),
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := c.rt.CheckSafeMode("message edit"); err != nil {
				return err
			}
			ctx := cmd.Context()
			target, ts, err := cmdutil.ParseMessageTarget(c.rt, cmd.CommandPath(), args[0], workspace, tsFlag)
			if err != nil {
				return err
			}
			text := args[1]
			hasBlocks := cmd.Flags().Changed("blocks")
			switch {
			case hasBlocks && strings.TrimSpace(blocksPath) == "":
				return errors.New(`--blocks needs a file path (or "-" for stdin), got an empty value`)
			case strings.TrimSpace(text) == "" && !hasBlocks:
				return fmt.Errorf(`<text> is empty: pass the new content of the message (to remove a message, use "%s message delete")`, appinfo.Name)
			}
			params := map[string]any{"ts": ts, "text": slacktext.FormatOutboundText(text)}
			if hasBlocks {
				blocks, err := loadBlocks(cmd.InOrStdin(), blocksPath)
				if err != nil {
					return err
				}
				params["blocks"] = blocks
			} else if blocks := slacktext.TextToRichTextBlocksInline(text); blocks != nil {
				// Unlike chat.postMessage, chat.update does not turn the
				// links of `text` into links: <https://example.com|label>,
				// <https://example.com> and bare URLs all stay literal
				// text in Slack. Links (and the rest of the formatting) only
				// survive an edit as rich_text elements.
				params["blocks"] = blocks
			} else {
				// chat.update keeps the blocks of the message when none
				// are sent, and Slack shows blocks rather than text: the
				// edit would leave what is displayed unchanged. An
				// explicit empty array removes them.
				params["blocks"] = []any{}
			}

			channelID, err := cmdutil.WithTarget(ctx, c.rt, target, func(loc cmdutil.Located) (string, error) {
				params["channel"] = loc.ChannelID
				_, err := loc.Client.API(ctx, "chat.update", params)
				return loc.ChannelID, explainMutationError(err, "edit", loc.ChannelID, ts)
			})
			if err != nil {
				return err
			}
			return c.rt.Print(messageResult{OK: true, ChannelID: channelID, TS: ts})
		},
	}
	cmdutil.AddTargetWorkspaceFlag(cmd, &workspace)
	cmd.Flags().StringVar(&tsFlag, "ts", "", cmdutil.MessageTSUsage)
	cmd.Flags().StringVar(&blocksPath, "blocks", "", blocksUsage)
	// <text> may start with "- ".
	return cmdutil.AllowDashText(cmd)
}

func (c *command) newDelete() *cobra.Command {
	var workspace, tsFlag string
	cmd := &cobra.Command{
		Use:   "delete <target>",
		Short: "Delete a message",
		Long: fmt.Sprintf(`Delete one of your own messages. This cannot be undone.

%s

Prints {ok, channel_id, ts}: the message deleted.
Refused in safe mode (--safe-mode or %s=1).`, messageTargetLong, appinfo.EnvName("SAFE_MODE")),
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := c.rt.CheckSafeMode("message delete"); err != nil {
				return err
			}
			ctx := cmd.Context()
			target, ts, err := cmdutil.ParseMessageTarget(c.rt, cmd.CommandPath(), args[0], workspace, tsFlag)
			if err != nil {
				return err
			}
			channelID, err := cmdutil.WithTarget(ctx, c.rt, target, func(loc cmdutil.Located) (string, error) {
				_, err := loc.Client.API(ctx, "chat.delete", map[string]any{"channel": loc.ChannelID, "ts": ts})
				return loc.ChannelID, explainMutationError(err, "delete", loc.ChannelID, ts)
			})
			if err != nil {
				return err
			}
			return c.rt.Print(messageResult{OK: true, ChannelID: channelID, TS: ts})
		},
	}
	cmdutil.AddTargetWorkspaceFlag(cmd, &workspace)
	cmd.Flags().StringVar(&tsFlag, "ts", "", cmdutil.MessageTSUsage)
	return cmd
}

// explainMutationError words the refusals of chat.update / chat.delete.
// action is "edit" or "delete".
func explainMutationError(err error, action, channelID, ts string) error {
	switch {
	case err == nil:
		return nil
	case slackapi.IsErrorCode(err, "cant_update_message", "cant_delete_message"):
		return fmt.Errorf("Slack refused to %[1]s message %[2]s in %[3]s: you can only %[1]s your own messages (and a workspace setting may forbid even that): %[4]w", action, ts, channelID, err)
	case slackapi.IsErrorCode(err, "edit_window_closed"):
		return fmt.Errorf("message %s in %s can no longer be edited: the workspace's editing window has passed: %w", ts, channelID, err)
	case slackapi.IsErrorCode(err, "message_not_found"):
		return fmt.Errorf("no message with ts %s in %s: it may already be deleted, or the ts or channel is wrong: %w", ts, channelID, err)
	}
	return fmt.Errorf("%s of message %s in %s failed: %w", action, ts, channelID, err)
}

func (c *command) newReact() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "react",
		Short: "Add or remove reactions",
		Long: `Add or remove your reactions on a message.

Reactions are not covered by safe mode: they are applied immediately.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	cmd.AddCommand(
		c.newReactAction("add", "reactions.add", "Add a reaction to a message", "to react with"),
		c.newReactAction("remove", "reactions.remove", "Remove your reaction from a message", "to remove"),
	)
	return cmd
}

// newReactAction returns "react add" or "react remove"; emojiUse says what
// the emoji is for ("to react with").
func (c *command) newReactAction(action, method, short, emojiUse string) *cobra.Command {
	var workspace, tsFlag string
	cmd := &cobra.Command{
		Use:   action + " <target> <emoji>",
		Short: short,
		Long: fmt.Sprintf(`%s.

%s

<emoji> is the emoji %s, as :rocket:, rocket or 🚀; a custom workspace
emoji goes by its name. Write the "-1" emoji as :-1: (a bare -1 is read as
a flag).

Prints {ok, channel_id, ts, name}: the message and the emoji name. When the
message was already as asked (the reaction is there for "add", absent for
"remove"), the command still succeeds, with "already": true and a note in
"hints": a retry after a timeout is safe. Not covered by safe mode.`, short, messageTargetLong, emojiUse),
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			target, ts, err := cmdutil.ParseMessageTarget(c.rt, cmd.CommandPath(), args[0], workspace, tsFlag)
			if err != nil {
				return err
			}
			name, err := slacktext.NormalizeReactionName(args[1])
			if err != nil {
				return err
			}
			result, err := cmdutil.WithTarget(ctx, c.rt, target, func(loc cmdutil.Located) (reactResult, error) {
				_, err := loc.Client.API(ctx, method, map[string]any{
					"channel":   loc.ChannelID,
					"timestamp": ts,
					"name":      name,
				})
				result := reactResult{messageResult: messageResult{OK: true, ChannelID: loc.ChannelID, TS: ts}, Name: name}
				// The message is already as asked: this is success, so
				// that a retry after a timeout (the first attempt may
				// have gone through) does not fail.
				switch {
				case action == "add" && slackapi.IsErrorCode(err, "already_reacted"):
					result.Already, err = true, nil
				case action == "remove" && slackapi.IsErrorCode(err, "no_reaction"):
					result.Already, err = true, nil
				case slackapi.IsErrorCode(err, "invalid_name"):
					err = fmt.Errorf("Slack does not know the emoji :%s: (custom emoji must exist in this workspace): %w", name, err)
				}
				return result, err
			})
			if err != nil {
				return err
			}
			switch {
			case result.Already && action == "add":
				c.rt.ResultHintf("you had already reacted with :%s: to this message: nothing changed", name)
			case result.Already:
				c.rt.ResultHintf("you had no :%s: reaction on this message: nothing changed", name)
			}
			return c.rt.Print(result)
		},
	}
	cmdutil.AddTargetWorkspaceFlag(cmd, &workspace)
	cmd.Flags().StringVar(&tsFlag, "ts", "", cmdutil.MessageTSUsage)
	return cmd
}
