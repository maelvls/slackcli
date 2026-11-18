package clictx

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// The commands an agent could misuse to reach other people are refused,
// and the help of safe mode names both what is covered and what is not.
func TestSafeModeBlocksOutreachAndCancellation(t *testing.T) {
	c := newContext(t)
	c.SafeModeFlag = true
	for command, action := range map[string]string{
		"channel invite":           "Slack Connect invitations",
		"channel new":              "create channels",
		"message scheduled cancel": "delete pending scheduled messages",
	} {
		err := c.CheckSafeMode(command)
		if err == nil || !strings.Contains(err.Error(), action) {
			t.Errorf("%q: err = %v, want a refusal mentioning %q", command, err, action)
		}
	}

	help := SafeModeHelp()
	// The lists are wrapped: compare them with the line breaks undone.
	flat := strings.Join(strings.Fields(help), " ")
	wantNotCovered := []string{"message react", "message draft *", "channel mark", "later *", "user dm-open"}
	if got := SafeModeNotCovered(); strings.Join(got, "|") != strings.Join(wantNotCovered, "|") {
		t.Errorf("SafeModeNotCovered() = %q, want %q", got, wantNotCovered)
	}
	for _, want := range []string{
		"(--safe-mode or SLACKCLI_SAFE_MODE=1)",
		"reviewed message send: opens the compose editor",
		"refused " + strings.Join(SafeModeBlocked(), ", ") + " not covered",
		"not covered " + strings.Join(wantNotCovered, ", ") + ` ("*" stands for every subcommand)`,
	} {
		if !strings.Contains(flat, want) {
			t.Errorf("the help does not contain %q:\n%s", want, help)
		}
	}
	for _, line := range strings.Split(help, "\n") {
		if len(line) > 78 {
			t.Errorf("line longer than 78 columns: %q", line)
		}
	}
}

func TestWrapItems(t *testing.T) {
	items := []string{strings.Repeat("a", 30), strings.Repeat("b", 30), strings.Repeat("c", 30)}
	want := strings.Repeat("a", 30) + ", " + strings.Repeat("b", 30) + ",\n    " + strings.Repeat("c", 30)
	if got := wrapItems(items, "    "); got != want {
		t.Errorf("wrapItems = %q, want %q", got, want)
	}
	if got := wrapItems(nil, "    "); got != "" {
		t.Errorf("wrapItems(nil) = %q", got)
	}
}

// docList matches a list of `backticked` commands introduced by a phrase.
func docList(phrase string) *regexp.Regexp {
	return regexp.MustCompile("(?i)" + phrase + ": ((?:`[^`]+`(?:, | and )?)+)")
}

var backticked = regexp.MustCompile("`([^`]+)`")

// The README and the skill list exactly the commands safe mode refuses
// and exactly those it does not cover.
func TestSafeModeDocsMatchTheCode(t *testing.T) {
	for _, path := range []string{"../../README.md", "../../skills/slackcli/SKILL.md"} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for phrase, want := range map[string][]string{
			"refused in safe mode":     SafeModeBlocked(),
			"not covered by safe mode": SafeModeNotCovered(),
		} {
			m := docList(phrase).FindStringSubmatch(string(raw))
			if m == nil {
				t.Errorf("%s: no list introduced by %q", path, phrase+":")
				continue
			}
			var got []string
			for _, item := range backticked.FindAllStringSubmatch(m[1], -1) {
				got = append(got, item[1])
			}
			if strings.Join(got, "|") != strings.Join(want, "|") {
				t.Errorf("%s: %s:\n  %q\nwant\n  %q", path, phrase, got, want)
			}
		}
	}
}
