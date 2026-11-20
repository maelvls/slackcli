package channels

import (
	"context"

	"github.com/maelvls/slackcli/internal/jsonx"
	"github.com/maelvls/slackcli/internal/slackapi"
)

// Conversation kinds of a CountsEntry.
const (
	CountsChannel = "channel"
	CountsMPIM    = "mpim"
	CountsDM      = "dm"
)

// CountsEntry is one conversation of the sidebar, as listed by
// users.counts.
type CountsEntry struct {
	ID string
	// Kind is CountsChannel (public or private), CountsMPIM or CountsDM.
	Kind string
	// UnreadCount is the number of unread messages; for a DM, Slack's
	// dm_count.
	UnreadCount int
	// MentionCount is the number of unread mentions; for a DM every unread
	// message is one.
	MentionCount int
	// IsMember is false for a channel the caller is not in, and for a DM
	// or a group DM that is closed (the caller is still a member of a
	// closed group DM, but the sidebar does not list it).
	IsMember   bool
	IsMuted    bool
	IsArchived bool
}

// HasUnreads reports whether the sidebar would flag the conversation:
// unread or mentioned, not archived, and when muted only if mentioned.
func (e CountsEntry) HasUnreads() bool {
	if e.IsArchived {
		return false
	}
	if e.MentionCount > 0 {
		return true
	}
	return e.UnreadCount > 0 && !e.IsMuted
}

// UsersCounts reads users.counts, the sidebar method that still works on
// an Enterprise Grid where client.counts answers team_is_restricted (see
// slackapi.IsRestricted). Unlike client.counts it has no read cursor
// (conversations.info has it, as last_read) and says nothing about
// threads.
//
// Entries come in Slack's order: public and private channels, group DMs
// included ("channels", then "groups", where a group DM is a group with
// is_mpim), then DMs.
func UsersCounts(ctx context.Context, client *slackapi.Client) ([]CountsEntry, error) {
	resp, err := client.API(ctx, "users.counts", nil)
	if err != nil {
		return nil, err
	}
	var out []CountsEntry
	for _, key := range []string{"channels", "groups"} {
		for _, raw := range jsonx.Maps(resp[key]) {
			id := jsonx.Str(raw["id"])
			if id == "" {
				continue
			}
			e := CountsEntry{
				ID:           id,
				Kind:         CountsChannel,
				UnreadCount:  firstInt(raw, "unread_count_display", "unread_count"),
				MentionCount: firstInt(raw, "mention_count_display", "mention_count"),
				// users.counts only lists joined channels, so a missing
				// is_member means "member".
				IsMember:   raw["is_member"] != false,
				IsMuted:    jsonx.Bool(raw["is_muted"]),
				IsArchived: jsonx.Bool(raw["is_archived"]),
			}
			if jsonx.Bool(raw["is_mpim"]) {
				e.Kind = CountsMPIM
				// Every group DM the caller was ever in is listed, with
				// is_member true: is_open says which ones the sidebar
				// shows.
				if open, ok := raw["is_open"].(bool); ok {
					e.IsMember = open
				}
			}
			out = append(out, e)
		}
	}
	for _, raw := range jsonx.Maps(resp["ims"]) {
		id := jsonx.Str(raw["id"])
		if id == "" {
			continue
		}
		n := firstInt(raw, "dm_count")
		out = append(out, CountsEntry{ID: id, Kind: CountsDM, UnreadCount: n, MentionCount: n, IsMember: jsonx.Bool(raw["is_open"])})
	}
	return out, nil
}

// firstInt returns the first of the given fields that holds a number, 0
// when none does.
func firstInt(raw map[string]any, keys ...string) int {
	for _, key := range keys {
		if n, ok := jsonx.Int(raw[key]); ok {
			return n
		}
	}
	return 0
}
