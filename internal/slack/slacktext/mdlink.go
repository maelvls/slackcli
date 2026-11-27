package slacktext

import "strings"

// outboundLink is a CommonMark inline link, `[label](https://...)`, in the
// text of an outgoing message. Slack has no such syntax: left alone, it is
// posted as literal brackets and parentheses. It is sent as a real link
// instead: `<url|label>` in mrkdwn ([FormatOutboundText]) and a link
// element in rich text ([ParseInlineElements]).
type outboundLink struct {
	label string // without the styles below
	url   string
	style inlineStyle
	// markers are the formatting markers that wrapped the whole label, from
	// the outermost: "*_" for `[*_label_*](...)`.
	markers  string
	labelEnd int // index of the closing bracket
	end      int // index after the closing parenthesis
}

// markdownLinkAt parses a Markdown link whose opening bracket is text[i].
//
//   - The label may hold balanced brackets ("[[PR] fix](...)") but no line
//     break, and must not be blank.
//   - The target must follow the closing bracket immediately and be an
//     http(s) URL: "[x](#anchor)", "[x](mailto:...)" and "[x](./file)" are
//     left as they are written. It may hold balanced parentheses, as
//     Wikipedia URLs do ("https://en.wikipedia.org/wiki/Go_(game)"), but no
//     whitespace: a target with a title ("(url "title")") is not a link.
//   - "<url>" targets are accepted, the CommonMark way to write a URL with
//     unbalanced parentheses.
//
// When the whole label is one `*bold*`, `_italic_`, `~strike~` or code span
// (or several nested), the link takes that style. Formatting covering only
// part of the label is kept as literal text: a link has a single style.
func markdownLinkAt(text string, i int) (outboundLink, bool) {
	if i >= len(text) || text[i] != '[' {
		return outboundLink{}, false
	}
	depth := 0
	closeBracket := -1
	for k := i; k < len(text) && closeBracket < 0; k++ {
		switch text[k] {
		case '\n':
			return outboundLink{}, false
		case '[':
			depth++
		case ']':
			depth--
			if depth == 0 {
				closeBracket = k
			}
		}
	}
	if closeBracket < 0 || closeBracket+1 >= len(text) || text[closeBracket+1] != '(' {
		return outboundLink{}, false
	}
	label := text[i+1 : closeBracket]
	if strings.TrimSpace(label) == "" {
		return outboundLink{}, false
	}

	url, end, ok := markdownLinkTarget(text, closeBracket+2)
	if !ok || !manualLinkRE.MatchString(url) || strings.HasPrefix(strings.ToLower(url), "mailto:") {
		return outboundLink{}, false
	}
	if _, host, _ := strings.Cut(url, "://"); host == "" {
		return outboundLink{}, false
	}

	link := outboundLink{url: url, labelEnd: closeBracket, end: end}
	link.label, link.style, link.markers = peelLabelStyle(label)
	return link, true
}

// markdownLinkTarget parses the target of a Markdown link starting at
// text[start], just after its opening parenthesis. It returns the URL and
// the index after the closing parenthesis.
func markdownLinkTarget(text string, start int) (url string, end int, ok bool) {
	if start < len(text) && text[start] == '<' {
		closeAngle := strings.IndexAny(text[start+1:], "<>\n")
		if closeAngle < 0 || text[start+1+closeAngle] != '>' {
			return "", 0, false
		}
		after := start + 1 + closeAngle + 1
		if after >= len(text) || text[after] != ')' {
			return "", 0, false
		}
		return text[start+1 : start+1+closeAngle], after + 1, true
	}
	depth := 0
	for k := start; k < len(text); k++ {
		switch c := text[k]; {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '<' || c == '>':
			return "", 0, false
		case c == '(':
			depth++
		case c == ')':
			if depth == 0 {
				if k == start {
					return "", 0, false
				}
				return text[start:k], k + 1, true
			}
			depth--
		}
	}
	return "", 0, false
}

// peelLabelStyle removes the formatting markers that wrap a whole link
// label, "*bold*" or "_*both*_", and returns the styles they stand for.
func peelLabelStyle(label string) (plain string, style inlineStyle, markers string) {
	for len(label) > 2 {
		m := label[0]
		if strings.IndexByte("*_~`", m) < 0 || label[len(label)-1] != m {
			break
		}
		inner := label[1 : len(label)-1]
		if strings.TrimSpace(inner) != inner || inner == "" || strings.IndexByte(inner, m) >= 0 {
			break
		}
		switch m {
		case '*':
			style.bold = true
		case '_':
			style.italic = true
		case '~':
			style.strike = true
		case '`':
			style.code = true
		}
		markers += string(m)
		label = inner
	}
	return label, style, markers
}

// mrkdwnLinkEscaper makes a URL safe inside a `<url|label>` token: a `|`
// would end the URL and a `>` the token.
var mrkdwnLinkEscaper = strings.NewReplacer("|", "%7C", "<", "%3C", ">", "%3E")

// mrkdwn returns the link as a mrkdwn `<url|label>` token, wrapped in the
// markers of its style: `*<url|label>*`. A code style cannot apply to a
// link in mrkdwn and is dropped there.
func (l outboundLink) mrkdwn() string {
	token := "<" + mrkdwnLinkEscaper.Replace(l.url) + "|" + entityEscaper.Replace(l.label) + ">"
	var open, closing strings.Builder
	for k := 0; k < len(l.markers); k++ {
		if l.markers[k] == '`' {
			continue
		}
		open.WriteByte(l.markers[k])
	}
	o := open.String()
	for k := len(o) - 1; k >= 0; k-- {
		closing.WriteByte(o[k])
	}
	return o + token + closing.String()
}

// element returns the link as a rich text link element.
func (l outboundLink) element(outer inlineStyle) map[string]any {
	style := l.style
	style.bold = style.bold || outer.bold
	style.italic = style.italic || outer.italic
	style.strike = style.strike || outer.strike
	style.code = style.code || outer.code
	return style.apply(map[string]any{"type": "link", "url": l.url, "text": l.label})
}
