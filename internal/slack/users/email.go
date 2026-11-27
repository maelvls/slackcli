package users

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/maelvls/slackcli/internal/jsonx"
	"github.com/maelvls/slackcli/internal/slack/enrich"
	"github.com/maelvls/slackcli/internal/slackapi"
)

const (
	// peopleByEmailCount is how many matches the people search is asked
	// for when it looks an email address up: one is expected, the others
	// are for the error when it is not.
	peopleByEmailCount = 10
	// maxPeopleByEmailChecks is how many of the members the people search
	// finds for an email address are checked with users.info: more than
	// that is not a search for one address.
	maxPeopleByEmailChecks = 5

	// deactivatedScanPages is how many users.list pages (of scanPageSize
	// members) are looked through for a deactivated member with an email
	// address that Slack's people search found no active member with. A
	// larger directory cannot be scanned within the command timeout in
	// the best of cases, and the users.list rate limit cuts its scan
	// short in the others: the scan stops and says so.
	deactivatedScanPages = 5
)

// peopleSearchName names Slack's people search in errors.
const peopleSearchName = "Slack's people search (search.modules.people)"

// lookupByPeopleSearch finds the member with an email address through
// Slack's people search, for browser credentials (xoxc): Slack answers
// not_allowed_token_type to their users.lookupByEmail, and the other way,
// a scan of the user directory, cannot finish in a large workspace. The
// people search answers in one call whatever the size of the workspace.
//
// Its items have no email (nor tz), and the search is fuzzy, so no item is
// taken at its word: each candidate (an item without an email, or with
// that one) is fetched with users.info, which has the email, and only a
// member whose email is the address is accepted. Only the first
// maxPeopleByEmailChecks candidates, in the order of the search, are
// checked. The result is:
//
//   - the user (as users.info gives it) when exactly one checked candidate
//     has the address;
//   - unresolved, an error for this address alone, when several have it,
//     when users.info hides the email of the candidates (or fails for
//     them) so that none can be confirmed, or when none of the checked
//     candidates has it but others were left unchecked: none is picked
//     (but see the next case);
//   - with allowUnverified, the user and unverified=true when the search
//     found one member and nobody else, and users.info shows that member,
//     not a bot, but hides its email: the search is then the only
//     evidence (see GetUserOptions.AllowUnverifiedEmail);
//   - nothing at all when no candidate has the address;
//   - err when a call failed in a way that says nothing about the address:
//     the search (see peopleSearchRefused for the failures that leave
//     users.lookupByEmail to try), or users.info not answering (an
//     *emailLookupError naming it).
func lookupByPeopleSearch(ctx context.Context, client *slackapi.Client, email string, allowUnverified bool) (user map[string]any, unverified bool, unresolved error, err error) {
	resp, err := client.API(ctx, "search.modules.people", map[string]any{
		"module": "people",
		"query":  email,
		"count":  peopleByEmailCount,
		"page":   1,
	})
	if err != nil {
		return nil, false, nil, err
	}
	var candidates []map[string]any
	seen := map[string]bool{}
	for _, item := range jsonx.Maps(resp["items"]) {
		id := jsonx.Str(item["id"])
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		u := peopleUser(item)
		// An item with another address is somebody else, found by the
		// fuzzy match of the search.
		if got := userEmail(u); got == "" || strings.EqualFold(got, email) {
			candidates = append(candidates, u)
		}
	}
	unchecked := 0
	if len(candidates) > maxPeopleByEmailChecks {
		unchecked = len(candidates) - maxPeopleByEmailChecks
		candidates = candidates[:maxPeopleByEmailChecks]
	}

	var confirmed, hidden []map[string]any
	var failedID string
	var failed error
	for _, c := range candidates {
		id := jsonx.Str(c["id"])
		resp, err := client.API(ctx, "users.info", map[string]any{"user": id})
		switch {
		case enrich.MustAbort(ctx, err):
			return nil, false, nil, fmt.Errorf("looking up user %s (%s, found by %s): %w", id, email, peopleSearchName, err)
		case slackapi.IsTransient(err):
			// Slack did not answer: that says nothing about the
			// address, and the next candidate would fare no better.
			return nil, false, nil, &emailLookupError{email: email, method: "users.info", tries: 1, err: err}
		case err != nil:
			// This candidate cannot be checked (a member of another
			// workspace, a refusal): it does not decide for the others.
			if failed == nil {
				failedID, failed = id, err
			}
			continue
		}
		u := jsonx.Map(resp["user"])
		if jsonx.Str(u["id"]) != id {
			if failed == nil {
				failedID, failed = id, errors.New("users.info returned no user")
			}
			continue
		}
		switch got := userEmail(u); {
		case strings.EqualFold(got, email):
			confirmed = append(confirmed, u)
		case got == "":
			hidden = append(hidden, u)
		}
		// Another address: the search matched something else (a name,
		// a title), and that member does not have this one.
	}
	switch {
	case len(confirmed) == 1:
		return confirmed[0], false, nil, nil
	case len(confirmed) > 1:
		amb := &AmbiguousError{Input: email, Reason: fmt.Sprintf("%d members have that email address", len(confirmed))}
		for _, u := range confirmed {
			amb.Matches = append(amb.Matches, ToCompactUser(u))
		}
		return nil, false, amb, nil
	case allowUnverified && len(seen) == 1 && len(hidden) == 1 && !jsonx.Bool(hidden[0]["is_bot"]):
		// The search found this member and nobody else, and users.info
		// knows it but hides its email, as Slack does for many members
		// of a large workspace: the search is the only evidence. A
		// caller that only reads takes it, marked as unverified. A bot
		// has no email to hide: its being found says nothing.
		return hidden[0], true, nil, nil
	case len(hidden) > 0:
		amb := &AmbiguousError{Input: email, Reason: fmt.Sprintf(
			"%s finds %d member(s) for that email address, but Slack does not show their email to you (users.info), so whether they have it cannot be checked",
			peopleSearchName, len(hidden))}
		for _, u := range hidden {
			amb.Matches = append(amb.Matches, ToCompactUser(u))
		}
		return nil, false, amb, nil
	case unchecked > 0:
		return nil, false, fmt.Errorf("could not resolve user %q: %s finds %d members for that email address, too many to check: "+
			"none of the first %d has it (users.info), the other %d were not checked. "+
			"Pass the user ID (U…) instead, or check the address",
			email, peopleSearchName, len(candidates)+unchecked, len(candidates), unchecked), nil
	case failed != nil:
		// %v, not %w: the code of users.info (missing_scope, …) must not
		// pass for a refusal of the people search.
		return nil, false, fmt.Errorf("could not resolve user %q: %s finds member %s for that email address, but users.info cannot show it (%v), "+
			"so whether it has the address cannot be checked. Pass the user ID (U…) instead",
			email, peopleSearchName, failedID, failed), nil
	}
	return nil, false, nil, nil
}

// userEmail is the email address of a Slack user object, "" when hidden.
func userEmail(u map[string]any) string {
	return strings.TrimSpace(jsonx.Str(jsonx.Map(u["profile"])["email"]))
}

// roomForAnotherPage reports whether a directory scan that only looks for
// deactivated members (see want.bounded) may ask for the page after the
// pages it has read (the last one took took): no more than
// deactivatedScanPages in all, and only when the command has the time for
// another page and a little more. A command without a timeout scans the
// whole directory.
func roomForAnotherPage(ctx context.Context, pages int, took time.Duration) bool {
	left, limited := slackapi.TimeLeft(ctx)
	if !limited {
		// No command timeout (COMMAND_TIMEOUT_MS=0): the scan may take
		// as long as it takes, as for the other tokens.
		return true
	}
	return pages < deactivatedScanPages && left > 2*took+time.Second
}

// boundedScanError says that a directory scan for deactivated members was
// stopped before its end, after pages pages: see roomForAnotherPage.
func boundedScanError(wants []*want, pages int) error {
	var missing []string
	for _, w := range wants {
		if w.active == nil && w.inactive == nil {
			missing = append(missing, fmt.Sprintf("%q", w.input))
		}
	}
	return fmt.Errorf("could not resolve user %s: no active member of this workspace has that email address (%s found none). "+
		"A deactivated member could still have it, but the user directory has more than %d members, "+
		"too many to look through within the command timeout (users.list is rate limited). "+
		"Pass the user ID (U…) instead, or check the address.",
		strings.Join(missing, ", "), peopleSearchName, pages*scanPageSize)
}
