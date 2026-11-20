package users

import (
	"context"
	"errors"
	"fmt"
	"html"
	"slices"
	"strings"
	"unicode"

	"github.com/maelvls/slackcli/internal/jsonx"
	"github.com/maelvls/slackcli/internal/slack/enrich"
	"github.com/maelvls/slackcli/internal/slackapi"
)

const (
	defaultSearchLimit = 20
	// MaxSearchLimit is the most users one search returns. It is also the
	// largest page search.modules.people serves: asked for more, Slack
	// silently answers with 20.
	MaxSearchLimit = 100
)

// SearchResult is what `user search` prints.
type SearchResult struct {
	// Users are the candidates, best match first.
	Users []CompactUser `json:"users"`
	// Total is how many members match, when that is known: it is larger
	// than len(Users) when the limit cut the list.
	Total int `json:"total,omitempty"`
	// Warnings say what is missing from Users, for the caller to show on
	// stderr.
	Warnings []string `json:"-"`
}

// SearchUsers finds the active members a few words designate: part of a
// name, a handle, a title. Unlike ResolveUserID it does not need an exact
// name, and it returns every candidate (at most limit: 0 means 20, values
// are clamped to 1..100).
//
// It asks Slack's people search (search.modules.people, what the Slack
// client searches people with), which answers in one call whatever the
// size of the workspace and decides what matches and in which order.
// Nothing is filtered here: Slack's search was seen to leave bots out, and
// whether it lists deactivated members is not known.
//
// Where Slack refuses that method (a bot token, see peopleSearchRefused)
// the user directory is scanned instead (users.list), keeping the active
// members, bots left out, in whose handle, names, email or title every
// word of the query is found, ignoring case and accents: slow in large
// workspaces, and cut short by the rate limit in huge ones (see
// SearchResult.Warnings). Any other failure of the people search fails
// the search: a timeout or a Slack outage says nothing about whether the
// method is available, and a scan started then would hide the real error
// behind a slow, possibly partial answer.
//
// A query that is an email address is first looked up as such
// (users.lookupByEmail), with the tokens that may: Slack answers
// not_allowed_token_type to browser credentials.
func SearchUsers(ctx context.Context, client *slackapi.Client, query string, limit int) (SearchResult, error) {
	query = strings.TrimSpace(query)
	terms := searchTerms(query)
	if len(terms) == 0 {
		return SearchResult{}, errors.New("the query is empty: pass part of a name, a handle or a title")
	}
	if limit <= 0 {
		limit = defaultSearchLimit
	}
	limit = min(limit, MaxSearchLimit)

	if emailRe.MatchString(query) && !client.Auth().IsBrowser() {
		user, err := lookupByEmail(ctx, client, query)
		switch {
		case user != nil:
			return SearchResult{Users: []CompactUser{ToCompactUser(user)}, Total: 1}, nil
		case err != nil && !slackapi.IsErrorCode(err, "users_not_found") && enrich.MustAbortRequired(ctx, err):
			return SearchResult{}, fmt.Errorf("looking up user %s: %w", query, err)
		}
		// Not the address of an active member, or users.lookupByEmail is
		// not available: it may still be found as text.
	}

	// Bot tokens cannot call search.*.
	why := "bot tokens cannot use Slack's people search"
	if !strings.HasPrefix(client.Auth().BearerToken(), "xoxb-") {
		result, err := searchPeople(ctx, client, query, limit)
		if err == nil {
			return result, nil
		}
		if !peopleSearchRefused(err) {
			return SearchResult{}, fmt.Errorf("searching users: %w", err)
		}
		why = fmt.Sprintf("Slack refused its people search: %s", slackapi.ErrorCode(err))
	}
	return searchDirectory(ctx, client, terms, limit, why)
}

// peopleSearchRefused reports whether err is Slack answering that
// search.modules.people is not for this token or this workspace, which is
// the one case where the directory is scanned instead. These codes are
// about the method and the credentials, not about the query or the moment:
// asking again would get the same answer.
//
//   - unknown_method, method_deprecated: the method is internal to Slack
//     and may not be served everywhere, or forever;
//   - not_allowed_token_type, missing_scope, no_permission: this kind of
//     token may not call it (an app token without search:read …);
//   - enterprise_is_restricted, feature_not_enabled: the workspace or its
//     plan does not offer it.
//
// Everything else fails the search: a timeout, a network error, an HTTP
// 5xx, internal_error or fatal_error, a rate limit, dead credentials.
func peopleSearchRefused(err error) bool {
	return slackapi.IsErrorCode(err, "unknown_method", "method_deprecated", "not_allowed_token_type", "missing_scope",
		"no_permission", "enterprise_is_restricted", "feature_not_enabled")
}

// searchPeople asks Slack's people search, page after page until limit
// users are found.
func searchPeople(ctx context.Context, client *slackapi.Client, query string, limit int) (SearchResult, error) {
	result := SearchResult{Users: []CompactUser{}}
	seen := map[string]bool{}
	for page := 1; ; page++ {
		resp, err := client.API(ctx, "search.modules.people", map[string]any{
			"module": "people",
			"query":  query,
			"count":  limit,
			"page":   page,
		})
		if err != nil {
			return SearchResult{}, err
		}
		items := jsonx.Maps(resp["items"])
		for _, item := range items {
			id := jsonx.Str(item["id"])
			if id == "" || seen[id] || len(result.Users) == limit {
				continue
			}
			seen[id] = true
			result.Users = append(result.Users, ToCompactUser(peopleUser(item)))
		}
		total, _ := jsonx.Int(jsonx.Get(resp, "pagination", "total_count"))
		pages, _ := jsonx.Int(jsonx.Get(resp, "pagination", "page_count"))
		result.Total = max(total, len(result.Users))
		if len(result.Users) >= limit || len(items) == 0 || page >= pages {
			return result, nil
		}
	}
}

// peopleUser gives an item of search.modules.people the shape of a
// users.info user: there the handle is "username", and the rest is in the
// profile (an item has no tz, is_bot or deleted). Unlike users.info, the
// search escapes the texts of the profile as HTML ("R&amp;D").
func peopleUser(item map[string]any) map[string]any {
	profile := map[string]any{}
	for key, v := range jsonx.Map(item["profile"]) {
		profile[key] = v
	}
	for _, key := range []string{"real_name", "display_name", "title", "status_text"} {
		if s, ok := profile[key].(string); ok {
			profile[key] = html.UnescapeString(s)
		}
	}
	return map[string]any{"id": item["id"], "name": item["username"], "profile": profile}
}

// searchDirectory scans the whole user directory for the active members
// that match every term. A rate limit after the first page ends the scan
// with what was found and a warning. why says why the people search was
// not used, for the error and the warning.
func searchDirectory(ctx context.Context, client *slackapi.Client, terms []string, limit int, why string) (SearchResult, error) {
	type match struct {
		user  CompactUser
		score int
	}
	var matches []match
	result := SearchResult{Users: []CompactUser{}}
	// The query as one name, whatever the spaces between its words.
	whole := strings.Join(terms, " ")
	cursor := ""
	for pages := 0; ; pages++ {
		params := map[string]any{"limit": scanPageSize}
		if cursor != "" {
			params["cursor"] = cursor
		}
		resp, err := client.API(ctx, "users.list", params)
		if err != nil {
			if pages > 0 && enrich.IsRateLimit(err) {
				result.Warnings = append(result.Warnings, fmt.Sprintf(
					"the user directory was scanned (%s) and the scan stopped early, the rest of the members were not looked at: %v", why, err))
				break
			}
			return SearchResult{}, fmt.Errorf("searching the user directory (%s): %w", why, err)
		}
		for _, m := range jsonx.Maps(resp["members"]) {
			if jsonx.Str(m["id"]) == "" || jsonx.Bool(m["deleted"]) || jsonx.Bool(m["is_bot"]) {
				continue
			}
			user := ToCompactUser(m)
			// Always false here, and the people search has neither.
			user.IsBot, user.Deleted = nil, nil
			if score, ok := searchScore(user, whole, terms); ok {
				matches = append(matches, match{user, score})
			}
		}
		next := nextCursor(resp)
		if next == "" || next == cursor {
			// Only a complete scan knows how many members match.
			result.Total = len(matches)
			break
		}
		cursor = next
	}
	slices.SortStableFunc(matches, func(a, b match) int { return a.score - b.score })
	for _, m := range matches[:min(limit, len(matches))] {
		result.Users = append(result.Users, m.user)
	}
	return result, nil
}

// searchScore reports whether every term is found in the handle, the
// names, the email or the title of the user, and how good the match is:
// 0 when the whole query is the handle or a name, 1 when every term starts
// a word of the handle or of a name, 2 otherwise.
func searchScore(u CompactUser, whole string, terms []string) (score int, ok bool) {
	names := []string{foldForSearch(u.Name), foldForSearch(u.RealName), foldForSearch(u.DisplayName)}
	fields := append([]string{foldForSearch(u.Email), foldForSearch(u.Title)}, names...)
	for _, term := range terms {
		if !slices.ContainsFunc(fields, func(f string) bool { return strings.Contains(f, term) }) {
			return 0, false
		}
	}
	if slices.Contains(names, whole) {
		return 0, true
	}
	for _, term := range terms {
		if !slices.ContainsFunc(names, func(name string) bool { return startsWord(name, term) }) {
			return 2, true
		}
	}
	return 1, true
}

// startsWord reports whether term is found in s at the start of a word.
func startsWord(s, term string) bool {
	for from := 0; ; {
		at := strings.Index(s[from:], term)
		if at < 0 {
			return false
		}
		at += from
		if at == 0 {
			return true
		}
		if prev := []rune(s[:at]); !unicode.IsLetter(prev[len(prev)-1]) && !unicode.IsDigit(prev[len(prev)-1]) {
			return true
		}
		from = at + 1
	}
}

// searchTerms splits a query into the words to look for, folded; the "@"
// of a handle is dropped.
func searchTerms(query string) []string {
	var terms []string
	for _, word := range strings.Fields(query) {
		if term := foldForSearch(strings.TrimPrefix(word, "@")); term != "" {
			terms = append(terms, term)
		}
	}
	return terms
}

// accentFolds maps the accented Latin letters of names to the letter they
// are typed as without the accent.
var accentFolds = func() map[rune]string {
	folds := map[rune]string{'ß': "ss", 'æ': "ae", 'œ': "oe", 'ø': "o", 'ł': "l", 'đ': "d"}
	for base, accented := range map[string]string{
		"a": "àáâãäåāăą", "c": "çćčĉċ", "d": "ď", "e": "èéêëēĕėęě", "g": "ĝğġģ", "h": "ĥ",
		"i": "ìíîïĩīĭįı", "j": "ĵ", "k": "ķ", "l": "ĺļľ", "n": "ñńņň", "o": "òóôõöōŏő",
		"r": "ŕŗř", "s": "śŝşšș", "t": "ţťț", "u": "ùúûüũūŭůűų", "w": "ŵ", "y": "ýÿŷ", "z": "źżž",
	} {
		for _, r := range accented {
			folds[r] = base
		}
	}
	return folds
}()

// foldForSearch lower-cases s and removes the accents of its Latin
// letters, so that "mael" finds "Maël".
func foldForSearch(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		switch folded, ok := accentFolds[r]; {
		case ok:
			b.WriteString(folded)
		case unicode.Is(unicode.Mn, r):
			// A combining accent, when the name is not precomposed.
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
