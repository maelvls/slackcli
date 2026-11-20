package channels

import (
	"context"
	"fmt"

	"github.com/maelvls/slackcli/internal/jsonx"
	"github.com/maelvls/slackcli/internal/slackapi"
)

// Details is what `channel info` prints of a conversation: what
// Conversation has, and what describes it.
type Details struct {
	Conversation
	// IsMember says that the caller is in the channel.
	IsMember bool   `json:"is_member,omitempty"`
	Topic    string `json:"topic,omitempty"`
	Purpose  string `json:"purpose,omitempty"`
	// Creator is the ID of the user who created the channel.
	Creator string `json:"creator,omitempty"`
	// Created is a Unix time in seconds.
	Created int64 `json:"created,omitempty"`
}

// FetchInfo returns Slack's object for one conversation (conversations.info),
// with its number of members when Slack counts them (not for a DM).
func FetchInfo(ctx context.Context, client *slackapi.Client, channelID string) (map[string]any, error) {
	resp, err := client.API(ctx, "conversations.info", map[string]any{"channel": channelID, "include_num_members": true})
	if err != nil {
		return nil, fmt.Errorf("looking up conversation %s: %w", channelID, err)
	}
	ch := jsonx.Map(resp["channel"])
	if jsonx.Str(ch["id"]) == "" {
		return nil, fmt.Errorf("looking up conversation %s: conversations.info returned no channel", channelID)
	}
	return ch, nil
}

// Describe converts a raw Slack conversation object to the shape printed
// by `channel info`.
func Describe(ch map[string]any) Details {
	created, _ := jsonx.Int64(ch["created"])
	return Details{
		Conversation: Compact([]map[string]any{ch})[0],
		IsMember:     jsonx.Bool(ch["is_member"]),
		Topic:        jsonx.GetStr(ch, "topic", "value"),
		Purpose:      jsonx.GetStr(ch, "purpose", "value"),
		Creator:      jsonx.Str(ch["creator"]),
		Created:      created,
	}
}
