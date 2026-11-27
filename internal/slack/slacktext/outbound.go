package slacktext

import (
	"regexp"
	"strings"
)

// outboundTokenRE matches the Slack tokens a user may already have
// written, whose `<`, `>` and `&` must not be escaped.
var outboundTokenRE = regexp.MustCompile(`<(?:@[UWB][A-Z0-9]+(?:\|[^>]*)?|#[CG][A-Z0-9]+(?:\|[^>]*)?|!subteam\^[A-Z0-9]+(?:\|[^>]*)?|!date\^[^>]+|![a-zA-Z]+(?:\|[^>]*)?|(?:https?://|mailto:)[^>]+)>`)

// FormatOutboundText prepares user-authored text for the `text` parameter
// of chat.postMessage and chat.update, which is mrkdwn: literal `&`, `<` and
// `>` are escaped as entities, the mentions people (and models) write as
// `@U05BRPTKL6A`, `@here`, `@channel` and `@everyone` are turned into the
// `<@U05BRPTKL6A>` and `<!here>` tokens that actually notify, and Markdown
// links, `[label](https://example.com)`, become `<https://example.com|label>`
// (see [markdownLinkAt]), which Slack shows as a link.
//
// Well-formed Slack tokens already in the text (`<@U123>`, `<#C123|name>`,
// `<!subteam^S123>`, `<!date^...>`, `<https://example.com|label>`) are left
// alone. Nothing inside a code span, a code block or a URL becomes a
// mention or a link, nor does an "@" in the middle of a word, an e-mail
// address or a path: see [bareMentionAt] for what counts as one.
func FormatOutboundText(text string) string {
	type segment struct {
		start, end int
		kind       codeKind
	}
	var segs []segment
	offset := 0
	splitCode(text, func(seg string, kind codeKind) {
		segs = append(segs, segment{offset, offset + len(seg), kind})
		offset += len(seg)
	})
	// linkFits reports whether a Markdown link leaves the code around it
	// whole: code may only be found inside its label ("[`func`](url)").
	linkFits := func(start int, link outboundLink) bool {
		for _, s := range segs {
			if s.kind != notCode && s.start < link.end && s.end > start &&
				(s.start <= start || s.end > link.labelEnd) {
				return false
			}
		}
		return true
	}

	var b strings.Builder
	pos := 0
	for _, s := range segs {
		if pos >= s.end {
			continue // covered by a link
		}
		if s.kind != notCode {
			b.WriteString(entityEscaper.Replace(text[pos:s.end]))
			pos = s.end
			continue
		}
		last := pos
		for i := pos; i < s.end; i++ {
			switch text[i] {
			case '<':
				loc := outboundTokenRE.FindStringIndex(text[i:s.end])
				if loc == nil || loc[0] != 0 {
					continue
				}
				writeWithMentions(&b, text, last, i)
				b.WriteString(text[i : i+loc[1]])
				last = i + loc[1]
				i = last - 1
			case '[':
				link, ok := markdownLinkAt(text, i)
				if !ok || !linkFits(i, link) {
					continue
				}
				writeWithMentions(&b, text, last, i)
				b.WriteString(link.mrkdwn())
				last = link.end
				if last >= s.end {
					break
				}
				i = last - 1
			case 'h':
				// A bare URL is never the start of a token or a link:
				// "https://x.test/[a](https://y.test)" stays one URL.
				if url, ok := bareURLAt(text, i); ok {
					i += len(url) - 1
				}
			}
			if last >= s.end {
				break
			}
		}
		if last < s.end {
			writeWithMentions(&b, text, last, s.end)
		}
		pos = max(last, s.end)
	}
	return b.String()
}

// writeWithMentions writes text[start:end], a run of literal text between
// tokens and code, entity-escaped and with its bare mentions turned into
// tokens. It is given the whole text because what precedes an "@", code and
// tokens included, decides whether it is a mention.
func writeWithMentions(b *strings.Builder, text string, start, end int) {
	pending := start
	for i := start; i < end; i++ {
		switch text[i] {
		case 'h':
			// "https://medium.com/@channel/post" must stay a working link.
			if url, ok := bareURLAt(text, i); ok {
				i += len(url) - 1
			}
		case '@':
			m, after, ok := bareMentionAt(text, i)
			if !ok {
				continue
			}
			b.WriteString(entityEscaper.Replace(text[pending:i]))
			if m.userID != "" {
				b.WriteString("<@" + m.userID + ">")
			} else {
				b.WriteString("<!" + m.broadcast + ">")
			}
			pending = after
			i = after - 1
		}
	}
	b.WriteString(entityEscaper.Replace(text[pending:end]))
}
