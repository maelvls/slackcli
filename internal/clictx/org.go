package clictx

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"sync"

	"github.com/maelvls/slackcli/internal/appinfo"
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
// session of the organisation: the drafts.* methods that Slack refuses
// with team_is_restricted. On a Grid, drafts.list and drafts.delete refuse
// the session of a workspace and accept the one of its organisation.
//
// The list is short on purpose. Drafts are the user's own, the same
// whichever of their sessions asks, and their channel IDs are valid
// throughout the organisation. Other methods are not retried this way:
// what they answer depends on the session asked. client.counts, for one,
// answers the organisation's session with the conversations of all its
// workspaces, which the calls that follow, made with the session of the
// workspace, cannot all read. The commands that use it have a fallback of
// their own, and --workspace <org>.enterprise.slack.com runs a whole
// command as the organisation.
//
// enterprise_is_restricted is the opposite refusal (a method that the
// organisation itself is not allowed), and user_is_restricted is about a
// guest account: another session of the same person changes neither.
func orgFallbackApplies(method string, err error) bool {
	return strings.HasPrefix(method, "drafts.") && slackapi.IsErrorCode(err, "team_is_restricted")
}

// orgFallback is the fallback of a client for the stored browser session a
// of workspaceURL: the calls orgFallbackApplies to are made again as the
// same person, with the session stored for the organisation the workspace
// belongs to. The organisation is only looked for once a call was refused,
// and once per client.
func (c *Context) orgFallback(a slackapi.Auth, workspaceURL string) slackapi.Fallback {
	var (
		once sync.Once
		org  *slackapi.Client
		why  string
	)
	return slackapi.Fallback{
		Applies: orgFallbackApplies,
		Client: func(ctx context.Context) (*slackapi.Client, string) {
			once.Do(func() { org, why = c.orgClient(ctx, a, workspaceURL) })
			return org, why
		},
	}
}

// orgClient returns a client for the stored session of the Enterprise Grid
// organisation that workspaceURL belongs to. When there is none, it returns
// nil and, when a stored organisation was looked at and could not be used,
// why (see slackapi.Fallback).
//
// Nothing is taken on trust: a stored organisation is only used when
// auth.test says, for both sessions, that it is the organisation of that
// workspace and that the person signed in is the same. A command never
// ends up acting as somebody else, or in another company's Slack.
func (c *Context) orgClient(ctx context.Context, a slackapi.Auth, workspaceURL string) (*slackapi.Client, string) {
	if IsOrgURL(workspaceURL) {
		return nil, ""
	}
	creds, err := c.Store.Load()
	if err != nil {
		return nil, ""
	}
	const refused = "this workspace belongs to an Enterprise Grid organisation, and Slack refuses this method to the session of a workspace. "
	reimport := fmt.Sprintf("sign in to the organisation in Slack Desktop (or a browser) and run `%s auth import-desktop` (or import-chrome, import-brave, import-firefox)", appinfo.Name)
	var (
		enterpriseID, userID string
		why                  string
	)
	for _, w := range creds.Workspaces {
		if w.URL == workspaceURL || !w.Auth.IsBrowser() || !IsOrgURL(w.URL) {
			continue
		}
		orgAuth, err := c.Store.Resolve(w)
		if err != nil {
			trace.Logf("organisation %s: its stored secrets cannot be read", w.URL)
			why = fmt.Sprintf("%sThe session stored for the organisation %s may be allowed it, but its secrets cannot be read (keychain locked?): unlock the keychain, or %s", refused, w.URL, reimport)
			continue
		}
		if enterpriseID == "" {
			// Asked once, and only when there is an organisation to compare
			// with.
			who, err := slackapi.New(a, workspaceURL, c.ClientOptions...).API(ctx, "auth.test", nil)
			if err != nil {
				return nil, ""
			}
			enterpriseID, userID = jsonx.Str(who["enterprise_id"]), jsonx.Str(who["user_id"])
			if enterpriseID == "" || userID == "" {
				return nil, "" // not a workspace of a Grid
			}
		}
		org := slackapi.New(orgAuth, w.URL, c.ClientOptions...)
		who, err := org.API(ctx, "auth.test", nil)
		if err != nil {
			trace.Logf("organisation %s: auth.test failed (%v)", w.URL, err)
			if slackapi.IsAuthError(err) {
				why = fmt.Sprintf("%sThe session stored for the organisation %s may be allowed it, but Slack rejected it (%s): %s", refused, w.URL, slackapi.ErrorCode(err), reimport)
			}
			continue
		}
		sameOrg := jsonx.Str(who["enterprise_id"]) == enterpriseID || jsonx.Str(who["team_id"]) == enterpriseID
		if sameOrg && jsonx.Str(who["user_id"]) == userID {
			return org, ""
		}
		trace.Logf("organisation %s is not that of %s, or is signed in as somebody else: not used", w.URL, workspaceURL)
		if why == "" {
			why = fmt.Sprintf("%sThe session of its organisation is allowed it, but the one that is stored (%s) is that of another organisation, or of another person: %s", refused, w.URL, reimport)
		}
	}
	return nil, why
}
