package users

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/maelvls/slackcli/internal/appinfo"
	"github.com/maelvls/slackcli/internal/jsonx"
	"github.com/maelvls/slackcli/internal/slack/enrich"
	"github.com/maelvls/slackcli/internal/slackapi"
	"github.com/maelvls/slackcli/internal/slackid"
)

// scanPageSize is the users.list page size for directory scans. users.list
// is rate limited per call (about 20 a minute), so the largest page Slack
// accepts scans a big workspace five times faster than the recommended
// 200.
const scanPageSize = 1000

var emailRe = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)

// NotFoundError says that no workspace member matches Input.
type NotFoundError struct {
	Input string
}

func (e *NotFoundError) Error() string {
	return fmt.Sprintf(`could not resolve user %q: no member of this workspace has that handle, email or display name. `+
		`Pass a user ID (U…), an email address or a @handle; "%s user list" shows them.`, e.Input, appinfo.Name)
}

// AmbiguousError says that Input is the display name of several members.
type AmbiguousError struct {
	Input string
	// Matches are the candidates.
	Matches []CompactUser
	// Reason says what the candidates have in common; "" means that
	// they have the name Input.
	Reason string
}

func (e *AmbiguousError) Error() string {
	var who []string
	for _, u := range e.Matches {
		who = append(who, fmt.Sprintf("%s (@%s)", u.ID, u.Name))
	}
	reason := e.Reason
	if reason == "" {
		reason = fmt.Sprintf("%d members have that name", len(e.Matches))
	}
	return fmt.Sprintf("user %q is ambiguous: %s: %s. Pass one of these user IDs instead.",
		e.Input, reason, strings.Join(who, ", "))
}

// ResolveUserID turns a user reference into a user ID. It accepts, in
// order of cost:
//
//   - a user ID (U…/W…): returned as-is, without any API call;
//   - an email address: one users.lookupByEmail call; with browser
//     credentials (xoxc), which may not call it, one search.modules.people
//     call (Slack's people search) and one users.info call (see
//     lookupByPeopleSearch);
//   - a handle, with or without "@": one search.messages "from:@handle"
//     call, which finds anybody who posted a message the caller can see
//     (user and browser tokens only), confirmed by one users.info call:
//     the author of the message must really have that handle;
//   - failing that, a handle or the exact display name or real name of a
//     single member: a scan of the user directory (users.list), which is
//     slow in large workspaces and gets rate limited in huge ones.
//
// The directory scan is also the last resort for an email address that
// users.lookupByEmail does not know (or may not look up): Slack only looks
// active members up by email, a deactivated one is found by the scan.
// After a people search that found nobody, that scan stops after a few
// pages (see deactivatedScanPages): a larger directory cannot be scanned
// within the command timeout.
//
// Matching is case-insensitive, and active members win over deactivated
// ones. The error is a *NotFoundError when nobody matches and an
// *AmbiguousError when a display name designates several members.
func ResolveUserID(ctx context.Context, client *slackapi.Client, input string) (string, error) {
	found, err := lookupAll(ctx, client, []string{input})
	if err != nil {
		return "", err
	}
	return found[0].id, nil
}

// Resolution is the outcome of resolving one input of ResolveUserIDs.
type Resolution struct {
	// Input is the user reference, as given.
	Input string
	// ID is the user ID; "" when Err is set.
	ID string
	// Err says why Input designates no single member: a *NotFoundError,
	// an *AmbiguousError, or a plain error for a blank input.
	Err error
}

// ResolveUserIDs resolves several user references (see ResolveUserID) and
// returns one Resolution per input, in order. The inputs that need the
// user directory share a single scan of it.
//
// An input that designates nobody (or several members) is reported in its
// Resolution and does not fail the others. The error is for failures that
// leave the outcome unknown: dead credentials, cancellation, a directory
// that cannot be scanned.
func ResolveUserIDs(ctx context.Context, client *slackapi.Client, inputs []string) ([]Resolution, error) {
	found, err := lookup(ctx, client, inputs)
	if err != nil {
		return nil, err
	}
	out := make([]Resolution, len(inputs))
	for i, f := range found {
		out[i] = Resolution{Input: inputs[i], ID: f.id, Err: f.err}
	}
	return out, nil
}

// found is the outcome for one input: the user's ID and, when the lookup
// came across it, the raw Slack user object; or err when the input
// designates no single member.
type found struct {
	id  string
	raw map[string]any
	err error
}

// want is one user being searched for in the directory.
type want struct {
	index  int    // position in the lookup inputs
	input  string // as given, for errors
	handle string // lower-cased, without "@"
	email  string // lower-cased; "" unless the input is an email address
	// deactivated is set when users.lookupByEmail knows no member with
	// that email: only a deactivated member can still have it, so the
	// scan may stop at the first one found.
	deactivated bool
	// emailErr is why users.lookupByEmail could not be used for this
	// email (a refusal: missing scope, token type), nil otherwise.
	emailErr error
	// notFoundBy names what found no active member with the email, when
	// deactivated is set: users.lookupByEmail or the people search.
	notFoundBy string
	// bounded is set when the email was looked up with the people search
	// (browser credentials) and nobody has it: only a deactivated member
	// is left to look for, and the scan for one stops once the directory
	// turns out too large for it (see roomForAnotherPage).
	bounded bool

	active   map[string]any   // handle or email match
	inactive map[string]any   // same, but a deactivated member
	named    []map[string]any // active members with that display or real name
}

// lookupAll is lookup for callers that need every input resolved: it
// fails on the first that is not.
func lookupAll(ctx context.Context, client *slackapi.Client, inputs []string) ([]found, error) {
	out, err := lookup(ctx, client, inputs)
	if err != nil {
		return nil, err
	}
	for _, f := range out {
		if f.err != nil {
			return nil, f.err
		}
	}
	return out, nil
}

// lookup resolves every input: out[i] is the outcome for inputs[i]. All
// the inputs that need the user directory share a single scan of it. The
// error is for operational failures only; an input matching nobody is
// reported in its found.err.
func lookup(ctx context.Context, client *slackapi.Client, inputs []string) ([]found, error) {
	out := make([]found, len(inputs))
	var wants []*want
	// searched remembers the outcome per handle, so that a handle given
	// twice costs one search.
	searched := map[string]map[string]any{}
	for i, input := range inputs {
		trimmed := strings.TrimSpace(input)
		if trimmed == "" {
			out[i].err = errors.New("user is empty: pass a user ID (U…), an email address or a @handle")
			continue
		}
		if slackid.IsUserID(trimmed) {
			out[i].id = trimmed
			continue
		}
		w := &want{index: i, input: input, handle: strings.ToLower(strings.TrimPrefix(trimmed, "@"))}
		if emailRe.MatchString(trimmed) {
			w.email = strings.ToLower(trimmed)
			viaPeople := false
			if client.Auth().IsBrowser() {
				user, amb, err := lookupByPeopleSearch(ctx, client, trimmed)
				viaPeople = true
				switch {
				case user != nil:
					out[i] = found{id: jsonx.Str(user["id"]), raw: user}
					continue
				case amb != nil:
					out[i].err = amb
					continue
				case err == nil:
					// No active member has that address. A deactivated
					// one may, if the directory is small enough to look.
					w.deactivated, w.bounded, w.notFoundBy = true, true, peopleSearchName
				case enrich.MustAbort(ctx, err):
					return nil, fmt.Errorf("looking up user %s: %w", trimmed, err)
				case slackapi.IsTransient(err):
					// As for users.lookupByEmail below: Slack did not
					// answer, which says nothing about the user.
					return nil, &emailLookupError{email: trimmed, method: "search.modules.people", tries: 1, err: err}
				case peopleSearchRefused(err):
					// The people search is not served here: try
					// users.lookupByEmail, as for the other tokens.
					viaPeople = false
				default:
					return nil, fmt.Errorf("looking up user %s by email: %w", trimmed, err)
				}
			}
			if viaPeople {
				wants = append(wants, w)
				continue
			}
			user, tries, err := lookupByEmail(ctx, client, trimmed)
			switch {
			case user != nil:
				out[i] = found{id: jsonx.Str(user["id"]), raw: user}
				continue
			case slackapi.IsErrorCode(err, "users_not_found"):
				// No active member has that address. A deactivated one
				// may: the directory is the only place to find them.
				w.deactivated, w.notFoundBy = true, "users.lookupByEmail"
			case enrich.MustAbort(ctx, err):
				return nil, fmt.Errorf("looking up user %s: %w", trimmed, err)
			case slackapi.IsTransient(err):
				// Slack did not answer, even when asked again. That says
				// nothing about the user, and the directory scan is not
				// the answer to it: in a large workspace it runs for
				// half a minute and then fails for another reason.
				return nil, &emailLookupError{email: trimmed, method: "users.lookupByEmail", tries: tries, err: err}
			case slackapi.IsErrorCode(err, emailLookupRefusals...):
				// users.lookupByEmail is not for these credentials: look
				// for the email in the directory instead.
				w.emailErr = err
			default:
				// Anything else (a proxy or a captive portal answering
				// in Slack's place, a redirect, an HTTP 4xx, an error
				// code that is about the request) says nothing about the
				// user either, and the directory would be asked through
				// the same broken path: the failure is the answer.
				return nil, fmt.Errorf("looking up user %s by email: %w", trimmed, err)
			}
		}
		if w.handle == "" {
			out[i].err = &NotFoundError{Input: input}
			continue
		}
		if w.email == "" && canSearch(client, w.handle) {
			user, done := searched[w.handle]
			if !done {
				var err error
				user, err = searchByHandle(ctx, client, w.handle)
				if err != nil && (ctx.Err() != nil || slackapi.IsAuthError(err)) {
					return nil, fmt.Errorf("looking up user @%s: %w", w.handle, err)
				}
				// Any other failure (missing search:read scope, rate limit
				// on the search tier, users.info refused …) leaves the
				// directory to look at. So does finding nothing: somebody
				// who never posted is invisible to search.
				searched[w.handle] = user
			}
			if user != nil {
				out[i] = found{id: jsonx.Str(user["id"]), raw: user}
				continue
			}
		}
		wants = append(wants, w)
	}

	if len(wants) > 0 {
		if err := scanDirectory(ctx, client, wants); err != nil {
			return nil, err
		}
	}
	for _, w := range wants {
		switch {
		case w.active != nil:
			out[w.index] = found{id: jsonx.Str(w.active["id"]), raw: w.active}
		case len(w.named) == 1:
			out[w.index] = found{id: jsonx.Str(w.named[0]["id"]), raw: w.named[0]}
		case len(w.named) > 1:
			amb := &AmbiguousError{Input: w.input}
			for _, m := range w.named {
				amb.Matches = append(amb.Matches, ToCompactUser(m))
			}
			out[w.index].err = amb
		case w.inactive != nil:
			out[w.index] = found{id: jsonx.Str(w.inactive["id"]), raw: w.inactive}
		default:
			out[w.index].err = &NotFoundError{Input: w.input}
		}
	}
	return out, nil
}

const (
	// emailLookupAttempts is how many times users.lookupByEmail is called
	// before a failure that says nothing about the user (see
	// slackapi.IsTransient) is given up on.
	emailLookupAttempts = 3
	// maxEmailLookupWait is the longest pause Slack may ask for (the
	// Retry-After of a rate limit) that is still waited out: beyond it,
	// failing with that delay in the message is of more use.
	maxEmailLookupWait = 10 * time.Second
	emailLookupBackoff = 500 * time.Millisecond
)

// emailLookupRefusals are the error codes with which users.lookupByEmail
// says that it is not for the credentials at hand, whoever is looked up:
// the address is then looked for in the directory (users.list), which
// asks for less.
//
//   - missing_scope: the token lacks users:read.email;
//   - not_allowed_token_type: the method does not take this kind of token.
//     It is what browser credentials (xoxc) get, so this is the usual path
//     of an imported session;
//   - no_permission, access_denied: the workspace or its administrators
//     keep the method, or email addresses, from this user or app.
//
// Other codes are about the request or about Slack, and are errors.
var emailLookupRefusals = []string{"missing_scope", "not_allowed_token_type", "no_permission", "access_denied"}

// lookupByEmail returns the active member with that email address, or the
// error of users.lookupByEmail ("users_not_found" when there is none), and
// how many times the method was called.
//
// The lookup is one cheap call that either names the user or says there
// is none, so a failure that does neither is not taken for an answer: the
// call is made again, after the pause Slack asks for when it is rate
// limiting, a short one otherwise. The client's own policy of failing fast
// on a rate limit suits the lookups a command can do without; this one it
// cannot.
//
// A call that timed out is made once more only, and only when the command
// has the time for it: a try may last the whole API timeout, and one that
// the command timeout would cut short ends the command with exit code 124
// instead of an error that says what happened. With the default timeouts
// (20 s per call, 30 s per command) a timeout is therefore not retried.
func lookupByEmail(ctx context.Context, client *slackapi.Client, email string) (user map[string]any, tries int, err error) {
	// The retries are these, and only these: with
	// SLACKCLI_RATE_LIMIT_MAX_WAIT_MS set, the client would make up to
	// four calls of its own for each one made here.
	ctx = slackapi.WithoutRateLimitRetry(ctx)
	for tries = 1; ; tries++ {
		resp, err := client.API(ctx, "users.lookupByEmail", map[string]any{"email": email})
		if err == nil {
			if user := jsonx.Map(resp["user"]); jsonx.Str(user["id"]) != "" {
				return user, tries, nil
			}
			return nil, tries, nil
		}
		if ctx.Err() != nil || !slackapi.IsTransient(err) || tries >= emailLookupAttempts {
			return nil, tries, err
		}
		wait := time.Duration(tries) * emailLookupBackoff
		var (
			limited *slackapi.RateLimitError
			timeout *slackapi.TimeoutError
		)
		switch {
		case errors.As(err, &limited):
			if wait = limited.RetryAfter; wait > maxEmailLookupWait {
				return nil, tries, err
			}
		case errors.As(err, &timeout):
			if tries >= 2 {
				return nil, tries, err
			}
			if left, limited := slackapi.TimeLeft(ctx); limited && left < wait+slackapi.APITimeout() {
				return nil, tries, err
			}
		}
		if sleepErr := client.Sleep(ctx, wait); sleepErr != nil {
			return nil, tries, sleepErr
		}
	}
}

// emailLookupError says that users.lookupByEmail gave no answer about an
// email address. It wraps the cause (errors.As still finds a
// *slackapi.RateLimitError) and words it for somebody who passed an email:
// the advice of the cause, or of a failed directory scan, to pass "the
// email address instead" would be of no use.
type emailLookupError struct {
	email string
	// method is the Slack method that was asked.
	method string
	// tries is how many times the method was called.
	tries int
	err   error
}

func (e *emailLookupError) Error() string {
	cause := e.err.Error()
	var limited *slackapi.RateLimitError
	if errors.As(e.err, &limited) {
		cause = limited.Summary()
	}
	after := "after 1 try"
	if e.tries > 1 {
		after = fmt.Sprintf("after %d tries", e.tries)
	}
	return fmt.Sprintf("could not look up user %q by email: %s gave no answer %s (%s). "+
		"This is temporary and says nothing about whether the user exists: run the command again in a moment, or pass the user ID (U…) if you know it.",
		e.email, e.method, after, strings.TrimSuffix(cause, "."))
}

func (e *emailLookupError) Unwrap() error { return e.err }

// canSearch reports whether the search.messages shortcut is worth trying:
// bot tokens cannot call search.*, and something with spaces or quotes is
// a display name, not a handle, and cannot be embedded in a query.
func canSearch(client *slackapi.Client, handle string) bool {
	if strings.HasPrefix(client.Auth().BearerToken(), "xoxb-") {
		return false
	}
	return !strings.ContainsAny(handle, " \t\n\"")
}

// searchResults is how many matches searchByHandle asks for. Search is
// fuzzy about "from:": a few results give the exact handle a chance to
// show up behind a near miss.
const searchResults = 5

// searchByHandle finds a user through the messages they posted: Slack has
// no handle→ID lookup, and the user directory of a large workspace cannot
// be paged through. It returns the user (as users.info gives it), or nil
// when no message is from that handle.
//
// The ID it returns ends up being DMed, invited or searched for, so a
// match is not taken at its word. Integrations post under any `username`
// they like: their messages (those with a bot_id or a subtype) are
// ignored. And the author of the first remaining match is confirmed with
// one users.info call, whose `name` must be the handle.
func searchByHandle(ctx context.Context, client *slackapi.Client, handle string) (map[string]any, error) {
	resp, err := client.API(ctx, "search.messages", map[string]any{
		"query":    "from:@" + handle,
		"count":    searchResults,
		"sort":     "timestamp",
		"sort_dir": "desc",
	})
	if err != nil {
		return nil, err
	}
	candidate := ""
	for _, m := range jsonx.Maps(jsonx.Get(resp, "messages", "matches")) {
		if jsonx.Str(m["bot_id"]) != "" || jsonx.Str(m["subtype"]) != "" {
			continue
		}
		// "from:" also matches display names and similar handles.
		if id := jsonx.Str(m["user"]); slackid.IsUserID(id) && strings.EqualFold(jsonx.Str(m["username"]), handle) {
			candidate = id
			break
		}
	}
	if candidate == "" {
		return nil, nil
	}
	resp, err = client.API(ctx, "users.info", map[string]any{"user": candidate})
	if err != nil {
		return nil, err
	}
	user := jsonx.Map(resp["user"])
	if jsonx.Str(user["id"]) != candidate || !strings.EqualFold(jsonx.Str(user["name"]), handle) {
		return nil, nil
	}
	return user, nil
}

// scanDirectory pages through users.list filling in wants, and stops as
// soon as each of them has an active handle (or email) match, or, for an
// email that no active member has, a deactivated one.
func scanDirectory(ctx context.Context, client *slackapi.Client, wants []*want) error {
	cursor := ""
	for pages := 1; ; pages++ {
		params := map[string]any{"limit": scanPageSize}
		if cursor != "" {
			params["cursor"] = cursor
		}
		start := time.Now()
		resp, err := client.API(ctx, "users.list", params)
		if err != nil {
			return scanError(err, wants)
		}
		took := time.Since(start)
		for _, m := range jsonx.Maps(resp["members"]) {
			if jsonx.Str(m["id"]) == "" {
				continue
			}
			for _, w := range wants {
				w.consider(m)
			}
		}
		done, bounded := true, true
		for _, w := range wants {
			settled := w.active != nil || w.deactivated && w.inactive != nil
			done = done && settled
			bounded = bounded && (settled || w.bounded)
		}
		next := nextCursor(resp)
		if done || next == "" || next == cursor {
			return nil
		}
		if bounded && !roomForAnotherPage(ctx, pages, took) {
			return boundedScanError(wants, pages)
		}
		cursor = next
	}
}

// scanError explains a failed directory scan. Being rate limited is the
// normal outcome in a workspace with tens of thousands of members, where
// retrying does not help: the message says what to pass instead.
func scanError(err error, wants []*want) error {
	var rl *slackapi.RateLimitError
	if !errors.As(err, &rl) {
		return fmt.Errorf("searching the user directory: %w", err)
	}
	var missing []string
	onlyEmails, onlyRefused := true, true
	var refusal error
	var notFoundBy string
	for _, w := range wants {
		// Settled, as in scanDirectory: an active match, or the deactivated
		// member an email no active member has was looked for.
		if w.active != nil || w.deactivated && w.inactive != nil {
			continue
		}
		missing = append(missing, strconv.Quote(w.input))
		onlyEmails = onlyEmails && w.deactivated
		onlyRefused = onlyRefused && w.emailErr != nil
		refusal = cmp.Or(refusal, w.emailErr)
		notFoundBy = cmp.Or(notFoundBy, w.notFoundBy)
	}
	wait := fmt.Sprintf("Setting %s to a large value makes the scan wait and carry on, which can take very long (%s)",
		appinfo.EnvName("RATE_LIMIT_MAX_WAIT_MS"), rl.Summary())
	if onlyRefused && len(missing) > 0 {
		// Every input is an email address that users.lookupByEmail would
		// not look up: "pass the email address instead" is no advice.
		//
		// The way out is a token that may: never a browser session,
		// which is what users.lookupByEmail answers
		// not_allowed_token_type to, and most likely what is in use.
		token := "a token that has the users:read.email scope (xoxp- or xoxb-, in SLACK_TOKEN or saved with `" + appinfo.Name + " auth add`)"
		if slackapi.IsErrorCode(refusal, "not_allowed_token_type") {
			token += "; a browser session, as imported from Slack Desktop or a browser, cannot use this method"
		}
		return &scanLimitError{err: err, msg: fmt.Sprintf("could not resolve user %s: these credentials may not look a user up by email (users.lookupByEmail: %s), "+
			"and the other way, a scan of the user directory, cannot finish in a workspace this large (users.list was rate limited). "+
			"Pass the user ID (U…) instead, or use %s. %s",
			strings.Join(missing, ", "), refusalCode(refusal), token, wait)}
	}
	if onlyEmails && len(missing) > 0 {
		return &scanLimitError{err: err, msg: fmt.Sprintf("could not resolve user %s: no active member of this workspace has that email address (%s), "+
			"and looking for a deactivated one takes a scan of the user directory, which this workspace is too large for (users.list was rate limited). "+
			"Pass the user ID (U…) instead. %s",
			strings.Join(missing, ", "), notFoundBy, wait)}
	}
	instead := "Pass the user ID (U…) or the email address instead"
	if refusal != nil {
		// Some of the inputs are emails that could not be looked up as
		// such: an email is no way out for those.
		instead = fmt.Sprintf("Pass the user ID (U…) instead (an email address is no help here: users.lookupByEmail answered %s)", refusalCode(refusal))
	}
	return &scanLimitError{err: err, msg: fmt.Sprintf("could not resolve user %s: this workspace is too large to scan its user directory (users.list was rate limited). "+
		"%s: a handle is only found without a scan when it is the exact @handle of somebody who posted a message you can see. %s",
		strings.Join(missing, ", "), instead, wait)}
}

// refusalCode names why users.lookupByEmail refused: Slack's error code,
// or the error itself when it has none.
func refusalCode(err error) string {
	if code := slackapi.ErrorCode(err); code != "" {
		return code
	}
	if err == nil {
		return "no user"
	}
	return err.Error()
}

// scanLimitError is the explanation of a rate-limited directory scan. It
// wraps the rate limit (errors.As still finds the *slackapi.RateLimitError)
// without repeating its message, which gives the same advice about the
// environment variable as the explanation does.
type scanLimitError struct {
	msg string
	err error
}

func (e *scanLimitError) Error() string { return e.msg }
func (e *scanLimitError) Unwrap() error { return e.err }

func (w *want) consider(m map[string]any) {
	profile := jsonx.Map(m["profile"])
	deleted := jsonx.Bool(m["deleted"])

	exact := strings.ToLower(jsonx.Str(m["name"])) == w.handle ||
		(w.email != "" && strings.ToLower(jsonx.Str(profile["email"])) == w.email)
	switch {
	case exact && !deleted:
		if w.active == nil {
			w.active = m
		}
		return
	case exact:
		if w.inactive == nil {
			w.inactive = m
		}
		return
	case deleted:
		return
	}
	for _, name := range []string{jsonx.Str(profile["display_name"]), jsonx.Str(profile["real_name"]), jsonx.Str(m["real_name"])} {
		if name != "" && strings.ToLower(name) == w.handle {
			w.named = append(w.named, m)
			return
		}
	}
}
