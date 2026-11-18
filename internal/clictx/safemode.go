package clictx

import (
	"fmt"
	"os"
	"strings"

	"github.com/maelvls/slackcli/internal/appinfo"
)

// SafeModeReviewed is the one command safe mode lets through, with a human
// in the loop: instead of posting, it opens the compose editor.
const SafeModeReviewed = "message send"

// safeModeBlocked is everything safe mode refuses outright, because there
// is no way for a human to review it first: the command, and what an
// agent could otherwise do with it. Together with safeModeNotCovered it is
// the single source of the help texts (SafeModeHelp), of the refusals
// (CheckSafeMode) and of what the docs must list (see the tests).
var safeModeBlocked = []struct{ command, action string }{
	{"message edit", "edit messages"},
	{"message delete", "delete messages"},
	{"message scheduled cancel", "delete pending scheduled messages"},
	{"canvas create", "create canvases"},
	{"canvas edit", "edit canvases"},
	{"workflow run", "start workflows (which may post messages or act on other systems)"},
	{"channel new", "create channels"},
	{"channel invite", "add people to channels or send Slack Connect invitations to external e-mail addresses"},
}

// safeModeNotCovered are the commands that change something in Slack and
// that safe mode deliberately lets through: they only touch the user's
// own, low-impact state (a reaction, a draft that is not posted, a read
// marker, the saved-for-later list, an open DM). "*" stands for every
// subcommand.
var safeModeNotCovered = []string{"message react", "message draft *", "channel mark", "later *", "user dm-open"}

// SafeModeBlocked lists the commands safe mode refuses (CheckSafeMode).
func SafeModeBlocked() []string {
	out := make([]string, len(safeModeBlocked))
	for i, b := range safeModeBlocked {
		out[i] = b.command
	}
	return out
}

// SafeModeNotCovered lists the writing commands safe mode lets through
// unreviewed; "*" stands for every subcommand.
func SafeModeNotCovered() []string { return append([]string(nil), safeModeNotCovered...) }

// SafeModeHelp says, for help texts, what safe mode does and what it does
// not cover, in lines of at most 78 columns.
func SafeModeHelp() string {
	const indent = "               "
	return fmt.Sprintf(`Safe mode (--safe-mode or %s=1) keeps a human in the loop:
  reviewed     %s: opens the compose editor, where a human reviews
               and sends the message, instead of posting
  refused      %s
  not covered  %s
               ("*" stands for every subcommand)`,
		appinfo.EnvName("SAFE_MODE"), SafeModeReviewed, wrapItems(SafeModeBlocked(), indent), wrapItems(safeModeNotCovered, indent))
}

// wrapItems renders items as a comma-separated list that continues on
// lines starting with indent, none longer than 78 columns. The first line
// is taken to start after a label as wide as indent.
func wrapItems(items []string, indent string) string {
	const width = 78
	var b strings.Builder
	column := len(indent)
	for i, item := range items {
		if i < len(items)-1 {
			item += ","
		}
		switch {
		case i == 0:
		case column+1+len(item) > width:
			b.WriteString("\n" + indent)
			column = len(indent)
		default:
			b.WriteString(" ")
			column++
		}
		b.WriteString(item)
		column += len(item)
	}
	return b.String()
}

// CheckSafeMode refuses command (e.g. "canvas edit") when safe mode is on;
// it returns nil when safe mode is off. command must be one of those safe
// mode blocks: an unknown one is refused all the same.
func (c *Context) CheckSafeMode(command string) error {
	if !c.SafeMode() {
		return nil
	}
	action := "do this"
	for _, b := range safeModeBlocked {
		if b.command == command {
			action = b.action
		}
	}
	return fmt.Errorf(`safe mode is active (turned on by %s): %q is blocked so that an agent cannot %s without human review. Ask the user to do it, or to disable safe mode`,
		c.SafeModeSource(), command, action)
}

// ChangesSlack reports whether command (its path below the root, e.g.
// "message send" or "later remind") posts, edits or deletes something in
// Slack: the commands safe mode reviews, refuses or deliberately lets
// through, and the compose editor. After such a command was cut short,
// nobody knows whether Slack got the change.
func ChangesSlack(command string) bool {
	writers := append([]string{SafeModeReviewed, "message compose"}, SafeModeBlocked()...)
	for _, w := range append(writers, safeModeNotCovered...) {
		// "later *" and "message react" stand for their subcommands too.
		w = strings.TrimSuffix(w, " *")
		if command == w || strings.HasPrefix(command, w+" ") {
			return true
		}
	}
	return false
}

// IsCI reports whether the CI environment variable says this is an
// automated run, where no browser can be opened for a human: it is set to
// anything but "", "0" or "false".
//
// Whatever it answers, safe mode holds: in CI, commands that need the
// compose editor fail under safe mode; outside CI the editor opens and
// nothing is sent until a human presses Send.
func IsCI() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("CI"))) {
	case "", "0", "false":
		return false
	}
	return true
}
