package slacktext

import (
	"strings"
	"testing"
)

// A message is sent as plain `text` (FormatOutboundText) or as blocks
// (ParseInlineElements) depending on whether it has a list: whether its
// "@here" notifies the channel must not depend on that.
func TestBareMentionsAreTheSameInTextAndBlocks(t *testing.T) {
	tests := []struct {
		in      string
		mention string // the bare mention under test
		want    bool   // whether it is a mention
	}{
		{"@here", "here", true},
		{"hi @here", "here", true},
		{"hi\n@channel", "channel", true},
		{"(@here)", "here", true},
		{`"@everyone"`, "everyone", true},
		{"cc:@U05BRPTKL6A, thanks", "U05BRPTKL6A", true},
		{"*@here*", "here", true},
		{"*bold*@here", "here", true},
		{":wave:@here", "here", true},
		{"> @here", "here", true},
		{"@UKTRBHMPQ", "UKTRBHMPQ", true},

		// Glued to code or to a token: not clearly meant as a mention.
		{"`code`@here", "here", false},
		{"`code` @here", "here", true},
		{"```code```@channel", "channel", false},
		{"<@U123456A>@here", "here", false},
		{"<@U123456A> @here", "here", true},
		{"<#C12345678>@channel", "channel", false},
		{"<https://example.com|x>@here", "here", false},
		{"<!subteam^S12345678>@U05BRPTKL6A", "U05BRPTKL6A", false},
		{"<fix>@here", "here", false},
		{"a >@here", "here", false},

		{"bob@here.com", "here", false},
		{"a/@here", "here", false},
		{"@here-and-now", "here", false},
		{"https://medium.com/@channel/post", "channel", false},
		{"see https://example.com/@here.", "here", false},
		{"xhttps://example.com/@here", "here", false},
		{"https://,@here", "here", true},
		{"@USERNAME", "USERNAME", false},
		{"@UNBOXTHERAPY", "UNBOXTHERAPY", false},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			text := FormatOutboundText(tt.in)
			inText := strings.Contains(text, "<!"+tt.mention+">") || strings.Contains(text, "<@"+tt.mention+">")
			if inText != tt.want {
				t.Errorf("FormatOutboundText(%q) = %q, want mention: %v", tt.in, text, tt.want)
			}

			inBlocks := false
			for _, el := range ParseInlineElements(tt.in) {
				el := el.(map[string]any)
				inBlocks = inBlocks ||
					el["type"] == "broadcast" && el["range"] == tt.mention ||
					el["type"] == "user" && el["user_id"] == tt.mention
			}
			if inBlocks != tt.want {
				t.Errorf("ParseInlineElements(%q) = %v, want mention: %v", tt.in, ParseInlineElements(tt.in), tt.want)
			}
		})
	}
}
