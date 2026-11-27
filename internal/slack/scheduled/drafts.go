package scheduled

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/maelvls/slackcli/internal/appinfo"
	"github.com/maelvls/slackcli/internal/jsonx"
	"github.com/maelvls/slackcli/internal/slack/drafts"
	"github.com/maelvls/slackcli/internal/slackapi"
)

// Slack refuses the chat.* scheduled message methods to a browser session
// (not_allowed_token_type), and they would not show what the user
// scheduled anyway: chat.scheduledMessages.list only returns what the
// same token scheduled with chat.scheduleMessage, never what was
// scheduled in the Slack client. The Slack client keeps its scheduled
// messages as drafts that have a date_scheduled, which drafts.list
// returns and drafts.delete cancels (the client's "Scheduled" tab reads
// and deletes them this way). So, for a browser session, the scheduled
// messages are those drafts, and their ID is the draft's ("Dr…").

// DraftsPageSize is how many drafts are asked of drafts.list: it has no
// cursor, and returns at most about 100 drafts, with has_more set when
// there are more.
const DraftsPageSize = 100

// isDraftID reports whether id is a draft ID ("Dr0123ABCD"), as opposed
// to the ID of a message scheduled with chat.scheduleMessage ("Q0123…").
func isDraftID(id string) bool { return strings.HasPrefix(id, "Dr") }

// activeDrafts returns the drafts that are not sent or deleted, and
// whether Slack has more of them than it returned.
func activeDrafts(ctx context.Context, client *slackapi.Client) ([]drafts.Draft, bool, error) {
	resp, err := client.API(ctx, "drafts.list", map[string]any{"is_active": true, "limit": DraftsPageSize})
	if err != nil {
		return nil, false, err
	}
	var found []drafts.Draft
	for _, raw := range jsonx.Slice(resp["drafts"]) {
		if d, ok := drafts.ParseRecord(raw); ok {
			found = append(found, d)
		}
	}
	return found, jsonx.Bool(resp["has_more"]), nil
}

// listDrafts is List for a browser session: the scheduled drafts, soonest
// first. drafts.list takes none of the filters of
// chat.scheduledMessages.list, so they are applied here, except the
// cursor: drafts.list has no pages.
func listDrafts(ctx context.Context, client *slackapi.Client, opts ListOptions) (ListResult, error) {
	if strings.TrimSpace(opts.Cursor) != "" {
		return ListResult{}, fmt.Errorf("--cursor cannot be used with a browser session: its scheduled messages are listed through drafts.list, which has no pages and prints no next_cursor. Run the command again without --cursor")
	}
	oldest, err := parseBound(opts.Oldest)
	if err != nil {
		return ListResult{}, err
	}
	latest, err := parseBound(opts.Latest)
	if err != nil {
		return ListResult{}, err
	}
	found, hasMore, err := activeDrafts(ctx, client)
	if err != nil {
		return ListResult{}, fmt.Errorf("listing scheduled messages (drafts.list): %w", err)
	}
	result := ListResult{OK: true, Incomplete: hasMore}
	for _, d := range found {
		if d.DateScheduled <= 0 || len(d.Destinations) == 0 {
			continue
		}
		post := float64(d.DateScheduled)
		if (oldest != nil && post < *oldest) || (latest != nil && post > *latest) {
			continue
		}
		// A draft addressed to several conversations is listed once, with
		// the one asked for, or else its first.
		dest := d.Destinations[0]
		if opts.ChannelID != "" {
			i := slices.IndexFunc(d.Destinations, func(x drafts.Destination) bool { return x.ChannelID == opts.ChannelID })
			if i < 0 {
				continue
			}
			dest = d.Destinations[i]
		}
		result.ScheduledMessages = append(result.ScheduledMessages, Message{
			ID:          d.ID,
			ChannelID:   dest.ChannelID,
			PostAt:      d.DateScheduled,
			DateCreated: d.DateCreated,
			Text:        d.Text,
		})
	}
	slices.SortStableFunc(result.ScheduledMessages, func(a, b Message) int { return cmp.Compare(a.PostAt, b.PostAt) })
	if opts.Limit > 0 && len(result.ScheduledMessages) > opts.Limit {
		result.ScheduledMessages = result.ScheduledMessages[:opts.Limit]
	}
	return result, nil
}

// parseBound reads a --oldest or --latest value that List has validated;
// nil when there is none.
func parseBound(s string) (*float64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return nil, fmt.Errorf("invalid bound %q: must be a Unix timestamp in seconds", s)
	}
	return &f, nil
}

// cancelDraft is Cancel for the ID of a scheduled draft: it deletes the
// draft, which is how the Slack client cancels a scheduled message. It
// only deletes a draft that is scheduled and addressed to channelID, so
// that a wrong ID cannot delete a draft the user is still writing.
func cancelDraft(ctx context.Context, client *slackapi.Client, channelID, draftID string) error {
	found, hasMore, err := activeDrafts(ctx, client)
	if err != nil {
		return fmt.Errorf("looking up scheduled message %s (drafts.list): %w", draftID, err)
	}
	i := slices.IndexFunc(found, func(d drafts.Draft) bool { return d.ID == draftID })
	if i < 0 {
		why := "it was already sent or cancelled"
		if hasMore {
			why = fmt.Sprintf("it was already sent or cancelled, or Slack has more drafts than the %d it returns at once and this one is not among them (cancel it in the Slack client)", DraftsPageSize)
		}
		return fmt.Errorf(`no pending scheduled message %s: %s. Run "%s message scheduled list" to see the pending ones`, draftID, why, appinfo.Name)
	}
	d := found[i]
	if d.DateScheduled <= 0 {
		return fmt.Errorf(`%s is a draft that is not scheduled: nothing to cancel. To delete the draft itself, run "%s message draft delete %s"`, draftID, appinfo.Name, draftID)
	}
	if !slices.ContainsFunc(d.Destinations, func(x drafts.Destination) bool { return x.ChannelID == channelID }) {
		var in []string
		for _, x := range d.Destinations {
			in = append(in, x.ChannelID)
		}
		return fmt.Errorf("scheduled message %s is scheduled in %s, not in %s: pass its channel_id, as printed by \"%s message scheduled list\", to --channel",
			draftID, strings.Join(in, ", "), channelID, appinfo.Name)
	}
	if d.LastUpdatedTS == "" {
		return fmt.Errorf("scheduled message %s has no last_updated_ts, which Slack needs to delete it: cancel it in the Slack client", draftID)
	}
	if err := drafts.Delete(ctx, client, d.ID, d.LastUpdatedTS); err != nil {
		return fmt.Errorf("cancelling scheduled message %s (drafts.delete): %w", draftID, err)
	}
	return nil
}
