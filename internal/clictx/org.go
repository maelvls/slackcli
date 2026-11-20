package clictx

import (
	"context"
	"errors"
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
// Only the drafts.* call itself changes session. What "draft list" asks
// next (conversations.info, for the names of the channels) is still asked
// as the workspace: a draft for a channel of another workspace of the
// organisation may be listed with its channel ID and no name.
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
	// The first reason found is the one given: it is about the first
	// organisation stored, which is the likeliest to be the right one.
	decline := func(format string, args ...any) {
		if why == "" {
			why = refused + fmt.Sprintf(format, args...)
		}
	}
	for _, w := range creds.Workspaces {
		if w.URL == workspaceURL || !IsOrgURL(w.URL) {
			continue
		}
		if !w.Auth.IsBrowser() {
			// Not the generic advice: --workspace <org> would run the
			// command with that token, which the commands that need a
			// browser session refuse.
			trace.Logf("organisation %s is stored with a standard token, not a browser session: not used", w.URL)
			decline("The organisation %[1]s is stored with a standard token (xoxb-/xoxp-), and only its browser session may be allowed this method: to use that session instead, run `%[2]s auth remove %[1]s`, then %[3]s", w.URL, appinfo.Name, reimport)
			continue
		}
		orgAuth, err := c.Store.Resolve(w)
		if err != nil {
			trace.Logf("organisation %s: its stored secrets cannot be read", w.URL)
			decline("The session stored for the organisation %s may be allowed it, but its secrets cannot be read (keychain locked?): unlock the keychain, or %s", w.URL, reimport)
			continue
		}
		if enterpriseID == "" {
			// Asked once, and only when there is an organisation to compare
			// with.
			who, err := slackapi.New(a, workspaceURL, c.ClientOptions...).API(ctx, "auth.test", nil)
			if err != nil {
				trace.Logf("workspace %s: auth.test failed (%v)", workspaceURL, err)
				if slackapi.IsTransient(err) {
					decline("The session stored for the organisation %s may be allowed it, but whether it is the organisation of this workspace could not be checked just now (%s): run the command again", w.URL, checkFailure(workspaceURL, err))
				}
				return nil, why
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
			switch {
			case slackapi.IsAuthError(err):
				decline("The session stored for the organisation %s may be allowed it, but Slack rejected it (%s): %s", w.URL, slackapi.ErrorCode(err), reimport)
			case slackapi.IsTransient(err):
				decline("The session stored for the organisation %s may be allowed it, but whether it is the organisation of this workspace could not be checked just now (%s): run the command again", w.URL, checkFailure(w.URL, err))
			}
			continue
		}
		// The organisation's own session names it as its enterprise, or
		// as its team.
		orgEnterprise, orgTeam, orgUser := jsonx.Str(who["enterprise_id"]), jsonx.Str(who["team_id"]), jsonx.Str(who["user_id"])
		sameOrg := orgEnterprise == enterpriseID || orgTeam == enterpriseID
		sameUser := orgUser == userID
		if sameOrg && sameUser {
			return org, ""
		}
		var differs []string
		if !sameOrg {
			if orgEnterprise == "" {
				orgEnterprise = orgTeam
			}
			differs = append(differs, fmt.Sprintf("is another organisation (%s, when this workspace belongs to %s)", orNone(orgEnterprise), enterpriseID))
		}
		if !sameUser {
			differs = append(differs, fmt.Sprintf("is signed in as another person (%s, when this workspace is signed in as %s)", orNone(orgUser), userID))
		}
		mismatch := strings.Join(differs, " and ")
		trace.Logf("organisation %s %s: not used for %s", w.URL, mismatch, workspaceURL)
		decline("The session of its organisation may be allowed it, but the one that is stored (%s) %s: %s", w.URL, mismatch, reimport)
	}
	return nil, why
}

// checkFailure says, in a few words, why the auth.test of workspaceURL gave
// no answer: the full error carries advice of its own, which would read as
// advice about the command.
func checkFailure(workspaceURL string, err error) string {
	cause := "no answer"
	var limited *slackapi.RateLimitError
	switch {
	case errors.As(err, &limited):
		cause = "rate limited"
	case slackapi.ErrorCode(err) != "":
		cause = slackapi.ErrorCode(err)
	}
	return fmt.Sprintf("auth.test of %s: %s", workspaceURL, cause)
}

// orNone is s, or "none" when Slack gave no value.
func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}
