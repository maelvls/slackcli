package messages

import (
	"context"

	"github.com/maelvls/slackcli/internal/jsonx"
	"github.com/maelvls/slackcli/internal/slack/enrich"
	"github.com/maelvls/slackcli/internal/slack/slackurl"
	"github.com/maelvls/slackcli/internal/slackapi"
)

// SentPermalink returns the URL of a message that was just posted ("" when
// ts is empty). It is built from workspaceURL when there is one. A token
// from SLACK_TOKEN that came without a workspace has none: Slack is asked
// instead (chat.getPermalink).
//
// That is an optional lookup in the sense of package enrich, and more so:
// the message is posted, so whatever goes wrong (dead credentials and
// cancellation included) only costs the permalink and a warning reported
// through enrich.Warnf, never the command. Failing would invite a retry
// that posts twice, which is why no error is returned.
func SentPermalink(ctx context.Context, client *slackapi.Client, workspaceURL, channelID, ts, threadTS string) string {
	if ts == "" {
		return ""
	}
	if workspaceURL != "" {
		return slackurl.BuildMessageURL(workspaceURL, channelID, ts, threadTS)
	}
	resp, err := client.API(ctx, "chat.getPermalink", map[string]any{"channel": channelID, "message_ts": ts})
	if err != nil {
		enrich.Warnf(ctx, "the message was sent (ts %s in %s), but its permalink could not be fetched: %v. Do not send it again", ts, channelID, err)
		return ""
	}
	return jsonx.Str(resp["permalink"])
}
