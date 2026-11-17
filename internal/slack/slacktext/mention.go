package slacktext

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

var (
	// bareUserRE matches what may be the user ID of a bare mention: see
	// [isBareUserID].
	bareUserRE      = regexp.MustCompile(`^@([UW][A-Z0-9]{8,})\b`)
	bareBroadcastRE = regexp.MustCompile(`^@(here|channel|everyone)\b`)

	// bareURLRE matches a URL written as is, not as a `<url>` token.
	bareURLRE = regexp.MustCompile(`^https?://[^\s<>]+`)
)

// bareMention is a mention written without its Slack token syntax.
type bareMention struct {
	userID    string // of "@U05BRPTKL6A", or
	broadcast string // "here", "channel" or "everyone"
}

// bareMentionAt parses the bare mention (`@U05BRPTKL6A`, `@here`,
// `@channel`, `@everyone`) whose "@" is text[i], and returns the index
// after it. text must be the whole text, not a piece of it: what precedes
// the "@" decides. It is the one rule for the plain `text` of a message
// ([FormatOutboundText]) and for its blocks ([ParseInlineElements]).
//
// A mention notifies people, @here and @channel many of them, so only what
// was clearly meant as one is accepted: the "@" must start the text or
// follow whitespace, an opening bracket or quote, or one of `,;:*_~`. That
// leaves alone e-mail addresses (bob@here.com), paths and URLs
// (medium.com/@channel/post), handles on other sites (@channel.io,
// @here-and-now) and an "@" glued to the end of a code span or of a `<…>`
// token ("`code`@here", "<@U123>@here").
func bareMentionAt(text string, i int) (m bareMention, end int, ok bool) {
	if prev, size := utf8.DecodeLastRuneInString(text[:i]); size > 0 && !opensMention(prev) {
		return bareMention{}, 0, false
	}
	rest := text[i:]
	if match := bareBroadcastRE.FindStringSubmatch(rest); match != nil {
		m.broadcast = match[1]
		end = i + len(match[0])
	} else if match := bareUserRE.FindStringSubmatch(rest); match != nil && isBareUserID(match[1]) {
		m.userID = match[1]
		end = i + len(match[0])
	} else {
		return bareMention{}, 0, false
	}
	// "@here." ends a sentence, "@here.com" is a domain.
	if after := text[end:]; len(after) >= 2 && strings.IndexByte(".-/@", after[0]) >= 0 {
		if next, _ := utf8.DecodeRuneInString(after[1:]); isAlnum(next) {
			return bareMention{}, 0, false
		}
	}
	return m, end, true
}

// isBareUserID reports whether id, "U" or "W" followed by at least 8
// upper-case letters and digits, is taken for a user ID when written as a
// bare mention. An ID with a digit is. One without is only at the two
// lengths Slack user IDs have, 9 and 11 characters: IDs are base 36 and
// some have no digit (UKTRBHMPQ), but so do words in capitals (@UPDATE,
// @USERNAME, @UNBOXTHERAPY).
func isBareUserID(id string) bool {
	return strings.ContainsAny(id, "0123456789") || len(id) == 9 || len(id) == 11
}

// opensMention reports whether a bare mention may follow r. Neither a
// backtick nor ">" is in the list: they end a code span and a token.
func opensMention(r rune) bool {
	return unicode.IsSpace(r) ||
		unicode.Is(unicode.Ps, r) || unicode.Is(unicode.Pi, r) ||
		strings.ContainsRune(`"',;:*_~`, r)
}

// bareURLAt returns the bare http(s) URL starting at text[i], without the
// punctuation that follows it (see [trimBareURL]). There is none in the
// middle of a word, nor when no host follows the scheme: "prefix it with
// https://." has no URL, and a link to "https://" is one Slack refuses.
func bareURLAt(text string, i int) (url string, ok bool) {
	if i > 0 && isWordByte(text[i-1]) {
		return "", false
	}
	url = trimBareURL(bareURLRE.FindString(text[i:]))
	_, host, _ := strings.Cut(url, "://")
	if first, _ := utf8.DecodeRuneInString(host); !isAlnum(first) && first != '[' {
		return "", false
	}
	return url, true
}

// trimBareURL removes from a bare URL the punctuation that ends the
// sentence, or closes the parenthesis around the URL, rather than being
// part of it.
func trimBareURL(url string) string {
	for url != "" {
		last := url[len(url)-1]
		switch {
		case strings.IndexByte(".,;:!?'\"*~", last) >= 0:
		case last == ')' && strings.Count(url, ")") > strings.Count(url, "("):
		default:
			return url
		}
		url = url[:len(url)-1]
	}
	return url
}
