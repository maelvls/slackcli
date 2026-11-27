package users

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/maelvls/slackcli/internal/jsonx"
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
// member whose email is the address is accepted. The result is:
//
//   - the user (as users.info gives it) when exactly one candidate has the
//     address;
//   - amb when several have it, when users.info hides the email of the
//     candidates so that none can be confirmed, or when there are too many
//     candidates to check (more than maxPeopleByEmailChecks): none is
//     picked;
//   - nothing at all when no candidate has the address;
//   - err when a call failed: the search (see peopleSearchRefused for the
//     failures that leave users.lookupByEmail to try) or users.info.
func lookupByPeopleSearch(ctx context.Context, client *slackapi.Client, email string) (user map[string]any, amb *AmbiguousError, err error) {
	resp, err := client.API(ctx, "search.modules.people", map[string]any{
		"module": "people",
		"query":  email,
		"count":  peopleByEmailCount,
		"page":   1,
	})
	if err != nil {
		return nil, nil, err
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
	if len(candidates) > maxPeopleByEmailChecks {
		amb = &AmbiguousError{Input: email, Reason: fmt.Sprintf(
			"%s finds %d members for that email address, too many to check which one has it", peopleSearchName, len(candidates))}
		for _, c := range candidates {
			amb.Matches = append(amb.Matches, ToCompactUser(c))
		}
		return nil, amb, nil
	}

	var confirmed, hidden []map[string]any
	for _, c := range candidates {
		id := jsonx.Str(c["id"])
		resp, err := client.API(ctx, "users.info", map[string]any{"user": id})
		if err != nil {
			return nil, nil, fmt.Errorf("looking up user %s (%s, found by %s): %w", id, email, peopleSearchName, err)
		}
		u := jsonx.Map(resp["user"])
		if jsonx.Str(u["id"]) != id {
			return nil, nil, fmt.Errorf("looking up user %s (%s, found by %s): users.info returned no user", id, email, peopleSearchName)
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
		return confirmed[0], nil, nil
	case len(confirmed) > 1:
		amb = &AmbiguousError{Input: email, Reason: fmt.Sprintf("%d members have that email address", len(confirmed))}
		for _, u := range confirmed {
			amb.Matches = append(amb.Matches, ToCompactUser(u))
		}
		return nil, amb, nil
	case len(hidden) > 0:
		amb = &AmbiguousError{Input: email, Reason: fmt.Sprintf(
			"%s finds %d member(s) for that email address, but Slack does not show their email to you (users.info), so whether they have it cannot be checked",
			peopleSearchName, len(hidden))}
		for _, u := range hidden {
			amb.Matches = append(amb.Matches, ToCompactUser(u))
		}
		return nil, amb, nil
	}
	return nil, nil, nil
}

// userEmail is the email address of a Slack user object, "" when hidden.
func userEmail(u map[string]any) string {
	return strings.TrimSpace(jsonx.Str(jsonx.Map(u["profile"])["email"]))
}

// roomForAnotherPage reports whether a directory scan that only looks for
// deactivated members (see want.bounded) may ask for the page after the
// pages it has read (the last one took took): no more than
// deactivatedScanPages in all, and only when the command has the time for
// another page and a little more.
func roomForAnotherPage(ctx context.Context, pages int, took time.Duration) bool {
	if pages >= deactivatedScanPages {
		return false
	}
	left, limited := slackapi.TimeLeft(ctx)
	return !limited || left > 2*took+time.Second
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
