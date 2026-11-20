package users

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode"

	"github.com/maelvls/slackcli/internal/jsonx"
	"github.com/maelvls/slackcli/internal/slack/enrich"
	"github.com/maelvls/slackcli/internal/slackapi"
)

const (
	defaultSearchLimit = 20
	// MaxSearchLimit is the most users one search returns.
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
// size of the workspace and decides what matches (it tolerates typos).
// Where that is not available (a bot token, a workspace that refuses it)
// the user directory is scanned instead (users.list), keeping the members
// in whose handle, names, email or title every word of the query is found,
// ignoring case and accents: slow in large workspaces, and cut short by
// the rate limit in huge ones (see SearchResult.Warnings).
//
// A query that is an email address is first looked up as such
// (users.lookupByEmail).
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

	if emailRe.MatchString(query) {
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
	if !strings.HasPrefix(client.Auth().BearerToken(), "xoxb-") {
		result, err := searchPeople(ctx, client, query, limit)
		if err == nil {
			return result, nil
		}
		if enrich.MustAbortRequired(ctx, err) {
			return SearchResult{}, fmt.Errorf("searching users: %w", err)
		}
		// Anything else (a token without the scope, a workspace where
		// the method is not served) leaves the directory to look at.
	}
	return searchDirectory(ctx, client, query, terms, limit)
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
// profile.
func peopleUser(item map[string]any) map[string]any {
	user := map[string]any{"id": item["id"], "name": item["username"], "profile": item["profile"]}
	for _, key := range []string{"tz", "is_bot", "deleted"} {
		if v, ok := item[key]; ok {
			user[key] = v
		}
	}
	return user
}

// searchDirectory scans the whole user directory for the active members
// that match every term. A rate limit after the first page ends the scan
// with what was found and a warning.
func searchDirectory(ctx context.Context, client *slackapi.Client, query string, terms []string, limit int) (SearchResult, error) {
	type match struct {
		user  CompactUser
		score int
	}
	var matches []match
	result := SearchResult{Users: []CompactUser{}}
	whole := foldForSearch(strings.TrimPrefix(query, "@"))
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
					"the scan of the user directory stopped early, the rest of the members were not looked at: %v", err))
				break
			}
			return SearchResult{}, fmt.Errorf("searching the user directory: %w", err)
		}
		for _, m := range jsonx.Maps(resp["members"]) {
			if jsonx.Str(m["id"]) == "" || jsonx.Bool(m["deleted"]) || jsonx.Bool(m["is_bot"]) {
				continue
			}
			user := ToCompactUser(m)
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
