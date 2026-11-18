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
// `>` are escaped as entities, and the mentions people (and models) write
// as `@U05BRPTKL6A`, `@here`, `@channel` and `@everyone` are turned into the
// `<@U05BRPTKL6A>` and `<!here>` tokens that actually notify.
//
// Well-formed Slack tokens already in the text (`<@U123>`, `<#C123|name>`,
// `<!subteam^S123>`, `<!date^...>`, `<https://example.com|label>`) are left
// alone. Nothing inside a code span, a code block or a URL becomes a
// mention, nor does an "@" in the middle of a word, an e-mail address or a
// path: see [bareMentionAt] for what counts as one.
func FormatOutboundText(text string) string {
	var b strings.Builder
	offset := 0 // of the segment in text
	splitCode(text, func(seg string, kind codeKind) {
		start := offset
		offset += len(seg)
		if kind != notCode {
			b.WriteString(entityEscaper.Replace(seg))
			return
		}
		last := 0
		for _, loc := range outboundTokenRE.FindAllStringIndex(seg, -1) {
			writeWithMentions(&b, text, start+last, start+loc[0])
			b.WriteString(seg[loc[0]:loc[1]])
			last = loc[1]
		}
		writeWithMentions(&b, text, start+last, offset)
	})
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
