package users

import (
	"cmp"
	"regexp"

	"github.com/maelvls/slackcli/internal/slack/messages"
)

// Label is the name to call a user by: the full name first, since it is
// the least ambiguous (display names are often a first name or a nickname
// that several people share), then the display name, then the handle.
//
// Every command that prints messages names their authors with it, so that
// whoever reads the output (often a model drafting a reply) never has to
// match a user ID to a person by hand: a wrong guess is a reply that
// greets the wrong person.
func Label(u CompactUser) string {
	return cmp.Or(u.RealName, u.DisplayName, u.Name)
}

// contentMentionRe matches a mention of a user as rendered to Markdown
// when Slack gave no label: "@U0123ABCD".
var contentMentionRe = regexp.MustCompile(`@([UW][A-Z0-9]{8,})\b`)

// MentionedInContent returns the IDs of the users that rendered content
// mentions by ID ("@U0123ABCD").
func MentionedInContent(content string) []string {
	var ids []string
	for _, m := range contentMentionRe.FindAllStringSubmatch(content, -1) {
		ids = append(ids, m[1])
	}
	return ids
}

// NameContent replaces the mentions by ID ("@U0123ABCD") of rendered
// content with the names of the users ("@Ada Lovelace"). Users missing
// from byID keep their ID.
func NameContent(content string, byID map[string]CompactUser) string {
	if len(byID) == 0 {
		return content
	}
	return contentMentionRe.ReplaceAllStringFunc(content, func(m string) string {
		if name := Label(byID[m[1:]]); name != "" {
			return "@" + name
		}
		return m
	})
}

// NameMessages names the authors of msgs and the users their content
// mentions, from byID.
func NameMessages(msgs []messages.CompactMessage, byID map[string]CompactUser) {
	for i := range msgs {
		msgs[i].Content = NameContent(msgs[i].Content, byID)
		if a := msgs[i].Author; a != nil {
			if name := Label(byID[a.UserID]); name != "" {
				a.Name = name
			}
		}
	}
}
