package slacktext

import "testing"

func TestFormatOutboundText(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{"bare user ID", "@U05BRPTKL6A heads up", "<@U05BRPTKL6A> heads up"},
		{"bare W ID and old 9-character ID", "cc @W1234567A and (@U024BE7LH)", "cc <@W1234567A> and (<@U024BE7LH>)"},
		{"formatted mention", "hi <@U123456A>!", "hi <@U123456A>!"},
		{"formatted mention with label", "hi <@U123456A|nick>!", "hi <@U123456A|nick>!"},
		{"formatted usergroup with label", "ping <!subteam^S12345678|@team>", "ping <!subteam^S12345678|@team>"},
		{"formatted usergroup", "ping <!subteam^S12345678>", "ping <!subteam^S12345678>"},
		{"broadcast", "@here ping", "<!here> ping"},
		{"broadcasts", "cc @channel and @everyone", "cc <!channel> and <!everyone>"},
		{"escapes", "a < b && c > d", "a &lt; b &amp;&amp; c &gt; d"},
		{"labelled link", "see <https://example.com|link>", "see <https://example.com|link>"},
		{"mailto link", "mail <mailto:bob@example.com|Bob>", "mail <mailto:bob@example.com|Bob>"},
		{"link with ampersand", "see <https://a.test/?x=1&y=2>", "see <https://a.test/?x=1&y=2>"},
		{"email-like text", "mail me at user@Udomain.com", "mail me at user@Udomain.com"},
		{"empty", "", ""},
		{
			"CI dump with a mention and a URL",
			`@U05BRPTKL6A heads up: CI "Install dependencies" is failing: https://github.com/x/y/actions/runs/1 & it needs <fix>`,
			`<@U05BRPTKL6A> heads up: CI "Install dependencies" is failing: https://github.com/x/y/actions/runs/1 &amp; it needs &lt;fix&gt;`,
		},

		// Fixed in the port.
		{"formatted channel and broadcast", "in <#C12345678|general> <!here>", "in <#C12345678|general> <!here>"},
		{"date token", "at <!date^1392734382^{date}|Feb 18>", "at <!date^1392734382^{date}|Feb 18>"},
		{"no mention glued to a token", "<@U123456A>@here <@U123456A> @here", "<@U123456A>@here <@U123456A> <!here>"},
		{"no mention glued to code", "`code`@here ```x```@channel", "`code`@here ```x```@channel"},
		{"user IDs without a digit", "@UKTRBHMPQ and @WKTRBHMPQXZ", "<@UKTRBHMPQ> and <@WKTRBHMPQXZ>"},
		{"scheme without a host hides no mention", "use https:// @here", "use https:// <!here>"},
		{"short ID is not a mention", "@U123 and @hereafter", "@U123 and @hereafter"},
		{"no mention in a code span", "run `notify @here && @U05BRPTKL6A` now @here", "run `notify @here &amp;&amp; @U05BRPTKL6A` now <!here>"},
		{"no mention in a code block", "```\n@channel <- literal\n```\n@channel", "```\n@channel &lt;- literal\n```\n<!channel>"},
		{"unterminated backtick is text", "it`s @here", "it`s <!here>"},
		{"NUL bytes are harmless", "a\x000\x00 & b", "a\x000\x00 &amp; b"},
		{"Markdown link", "see [the docs](https://example.com/docs).", "see <https://example.com/docs|the docs>."},
		{"several Markdown links", "[a](https://a.test) & [b](http://b.test/?x=1&y=2)", "<https://a.test|a> &amp; <http://b.test/?x=1&y=2|b>"},
		{"Markdown link in a code span", "run `[a](https://a.test)` now", "run `[a](https://a.test)` now"},
		{"Markdown link in a code block", "```\n[a](https://a.test)\n```", "```\n[a](https://a.test)\n```"},
		{"code span across a Markdown link", "[a `b](https://a.test) c`", "[a `b](https://a.test) c`"},
		{"Markdown link to a URL with parentheses", "[Go](https://en.wikipedia.org/wiki/Go_(game))", "<https://en.wikipedia.org/wiki/Go_(game)|Go>"},
		{"Markdown link with an angle-bracket target", "[x](<https://a.test/a)b>)", "<https://a.test/a)b|x>"},
		{"Markdown link with brackets and entities in its label", "[[PR] a < b & c](https://a.test/p|q)", "<https://a.test/p%7Cq|[PR] a &lt; b &amp; c>"},
		{"Markdown link with a styled label", "[*bold*](https://a.test) [`code`](https://b.test)", "*<https://a.test|bold>* <https://b.test|code>"},
		{"Markdown link with a code span in its label", "[the `x` func](https://a.test)", "<https://a.test|the `x` func>"},
		{"Slack link is left alone", "<https://a.test|[a](b)>", "<https://a.test|[a](b)>"},
		{"Markdown links to non-http targets stay text", "[a](#top) [b](mailto:x@y.z) [c](./f.md) [d](https://a.test \"t\")", "[a](#top) [b](mailto:x@y.z) [c](./f.md) [d](https://a.test \"t\")"},
		{"mention next to a Markdown link", "@here [a](https://a.test/@channel) @channel", "<!here> <https://a.test/@channel|a> <!channel>"},
		{"multibyte text", "héllo @here — «ça» > là", "héllo <!here> — «ça» &gt; là"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertEqual(t, FormatOutboundText(tt.in), tt.want)
		})
	}
}

// Text that only looks like a mention must not notify anyone, nor break
// the URL it is part of.
func TestFormatOutboundTextLeavesNonMentions(t *testing.T) {
	for _, text := range []string{
		"see https://medium.com/@channel/post-1",
		"watch https://www.youtube.com/@UNBOXTHERAPY now",
		"https://example.com/@here",
		"https://example.com/u/@U05BRPTKL6A/profile",
		"http://example.com/?to=@everyone",
		"(https://x.test/@here)",
		"the @WEBHOOKS and @USERNAME fields",
		"@UPDATE @WARNING @UNDERSTAND",
		"@UNBOXTHERAPY and @WORKSPACES",
		"@BABCDEFG1 is a bot ID, not a user",
		"mail bob@here.com or x@channel.io",
		"alice@U05BRPTKL6A.example",
		"follow @here.com and @channel.io",
		"twitter.com/@everyone",
		"a/@here b.@channel c@@everyone",
		"@here-and-now",
		"@U123 and @hereafter",
	} {
		assertEqual(t, FormatOutboundText(text), text)
	}

	tests := []struct {
		name, in, want string
	}{
		{"mention next to a URL", "@here see https://medium.com/@channel/post-1 @channel", "<!here> see https://medium.com/@channel/post-1 <!channel>"},
		{"end of sentence", "ping @here. Thanks @U05BRPTKL6A!", "ping <!here>. Thanks <@U05BRPTKL6A>!"},
		{"after opening punctuation", `(@here) [@channel] "@everyone" cc:@U05BRPTKL6A, *@here*`, `(<!here>) [<!channel>] "<!everyone>" cc:<@U05BRPTKL6A>, *<!here>*`},
		{"on a new line", "hello\n@channel\n\t@U05BRPTKL6A", "hello\n<!channel>\n\t<@U05BRPTKL6A>"},
		{"after a quote marker", "> @here", "&gt; <!here>"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertEqual(t, FormatOutboundText(tt.in), tt.want)
		})
	}
}
