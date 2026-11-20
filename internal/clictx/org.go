package clictx

import (
	"context"
	"net/url"
	"strings"
	"sync"

	"github.com/maelvls/slackcli/internal/jsonx"
	"github.com/maelvls/slackcli/internal/slackapi"
	"github.com/maelvls/slackcli/internal/trace"
)

// orgHostSuffix ends the host of an Enterprise Grid organisation
// ("acme.enterprise.slack.com"), as opposed to one of its workspaces
// ("acme-eng.slack.com").
const orgHostSuffix = ".enterprise.slack.com"

// IsOrgURL reports whether workspaceURL is that of an Enterprise Grid
// organisation rather than of a workspace.
func IsOrgURL(workspaceURL string) bool {
	u, err := url.Parse(workspaceURL)
	return err == nil && strings.HasSuffix(strings.ToLower(u.Hostname()), orgHostSuffix)
}

// orgFallbackApplies says which refused calls are made again with the
// session of the organisation: the drafts.* methods, when Slack refuses
// them with a "*_is_restricted" code. On a Grid, drafts.list and
// drafts.delete refuse the session of a workspace (team_is_restricted)
// and accept the one of its organisation.
//
// The list is short on purpose. Drafts are the user's own, the same
// whichever of their sessions asks, and their channel IDs are valid
// throughout the organisation. Other methods are not retried this way:
// what they answer may depend on the workspace asked (client.counts lists
// the unreads of the whole organisation), and for those the commands have
// a fallback of their own.
func orgFallbackApplies(method string, err error) bool {
	return strings.HasPrefix(method, "drafts.") && slackapi.IsRestricted(err)
}

// orgFallback is the fallback of a client for the stored browser session a
// of workspaceURL: the calls orgFallbackApplies to are made again as the
// same person, with the session stored for the organisation the workspace
// belongs to. The organisation is only looked for once a call was refused.
func (c *Context) orgFallback(a slackapi.Auth, workspaceURL string) slackapi.Fallback {
	var (
		once sync.Once
		org  *slackapi.Client
	)
	return slackapi.Fallback{
		Applies: orgFallbackApplies,
		Client: func(ctx context.Context) *slackapi.Client {
			once.Do(func() { org = c.orgClient(ctx, a, workspaceURL) })
			return org
		},
	}
}

// orgClient returns a client for the stored session of the Enterprise Grid
// organisation that workspaceURL belongs to, or nil when there is none.
//
// Nothing is taken on trust: a stored organisation is only used when
// auth.test says, for both sessions, that it is the organisation of that
// workspace and that the person signed in is the same. A command never
// ends up acting as somebody else, or in another company's Slack.
func (c *Context) orgClient(ctx context.Context, a slackapi.Auth, workspaceURL string) *slackapi.Client {
	if IsOrgURL(workspaceURL) {
		return nil
	}
	creds, err := c.Store.Load()
	if err != nil {
		return nil
	}
	var enterpriseID, userID string
	for _, w := range creds.Workspaces {
		if w.URL == workspaceURL || !w.Auth.IsBrowser() || !IsOrgURL(w.URL) {
			continue
		}
		orgAuth, err := c.Store.Resolve(w)
		if err != nil {
			trace.Logf("organisation %s: its stored secrets cannot be read", w.URL)
			continue
		}
		if enterpriseID == "" {
			// Asked once, and only when there is an organisation to compare
			// with.
			who, err := slackapi.New(a, workspaceURL, c.ClientOptions...).API(ctx, "auth.test", nil)
			if err != nil {
				return nil
			}
			enterpriseID, userID = jsonx.Str(who["enterprise_id"]), jsonx.Str(who["user_id"])
			if enterpriseID == "" || userID == "" {
				return nil // not a workspace of a Grid
			}
		}
		org := slackapi.New(orgAuth, w.URL, c.ClientOptions...)
		who, err := org.API(ctx, "auth.test", nil)
		if err != nil {
			trace.Logf("organisation %s: auth.test failed (%v)", w.URL, err)
			continue
		}
		sameOrg := jsonx.Str(who["enterprise_id"]) == enterpriseID || jsonx.Str(who["team_id"]) == enterpriseID
		if sameOrg && jsonx.Str(who["user_id"]) == userID {
			return org
		}
		trace.Logf("organisation %s is not that of %s, or is signed in as somebody else: not used", w.URL, workspaceURL)
	}
	return nil
}
