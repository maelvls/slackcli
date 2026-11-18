package cmdutil

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// UsageError is a command line that a command cannot accept: the wrong
// number of arguments, an unknown or malformed flag, a required flag left
// out, flags that exclude each other.
//
// Err is the error as cobra or the command's own validator worded it, and
// it is what Error returns. Message words it for the person or agent who
// typed the command; the root prints that, followed by a pointer to the
// help of Cmd.
type UsageError struct {
	// Cmd is the command that rejected its command line.
	Cmd *cobra.Command
	Err error

	kind usageKind
	// args are the positional arguments, for usageArgs.
	args []string
}

type usageKind int

const (
	usageArgs usageKind = iota
	usageFlag
	usageRequiredFlags
	usageFlagGroups
)

func (e *UsageError) Error() string { return e.Err.Error() }

// Unwrap returns the underlying error.
func (e *UsageError) Unwrap() error { return e.Err }

// CheckArgs validates the positional arguments of cmd with validate (nil
// accepts anything) and returns a failure as a *UsageError; one that
// validate returns is kept as it is.
func CheckArgs(cmd *cobra.Command, validate cobra.PositionalArgs, args []string) error {
	if validate == nil {
		return nil
	}
	err := validate(cmd, args)
	var usage *UsageError
	if err == nil || errors.As(err, &usage) {
		return err
	}
	return &UsageError{Cmd: cmd, Err: err, kind: usageArgs, args: args}
}

// CheckFlags makes the checks cobra makes on the flags of cmd once they
// are parsed (required flags, flag groups) and returns a failure as a
// *UsageError.
func CheckFlags(cmd *cobra.Command) error {
	if err := cmd.ValidateRequiredFlags(); err != nil {
		return &UsageError{Cmd: cmd, Err: err, kind: usageRequiredFlags}
	}
	if err := cmd.ValidateFlagGroups(); err != nil {
		return &UsageError{Cmd: cmd, Err: err, kind: usageFlagGroups}
	}
	return nil
}

// FlagError turns a flag parsing error of cmd into a *UsageError. It is
// the root's cobra FlagErrorFunc.
func FlagError(cmd *cobra.Command, err error) error {
	var usage *UsageError
	if err == nil || errors.As(err, &usage) {
		return err
	}
	return &UsageError{Cmd: cmd, Err: err, kind: usageFlag}
}

// Message words the error for whoever typed the command: which arguments
// the command takes, which flag is wrong and what it accepts, without the
// wording of cobra and of the Go standard library.
func (e *UsageError) Message() string {
	switch e.kind {
	case usageArgs:
		raw := e.Err.Error()
		switch {
		case cobraArity.MatchString(raw):
			return arityMessage(e.Cmd, e.args)
		case len(e.args) > 0 && raw == fmt.Sprintf("unknown command %q for %q", e.args[0], e.Cmd.CommandPath()):
			// cobra.NoArgs: true of a group of commands, but a command
			// of its own simply takes no arguments.
			if e.Cmd.HasSubCommands() {
				return unknownCommandMessage(e.Cmd, e.args[0])
			}
			return arityMessage(e.Cmd, e.args)
		}
	case usageFlag:
		return flagMessage(e.Cmd, e.Err)
	case usageRequiredFlags:
		if missing := missingRequiredFlags(e.Cmd); len(missing) > 0 {
			return flagList(missing) + plural(len(missing), " is required", " are required")
		}
	case usageFlagGroups:
		return flagGroupMessage(e.Err.Error())
	}
	return e.Err.Error()
}

// The errors of cobra's argument validators (cobra.ExactArgs and the
// like) and of its flag groups are plain strings: they are recognised by
// their wording, here and nowhere else. TestUsageMessages runs cobra
// itself, so that a change of wording in cobra fails it.
var (
	cobraArity             = regexp.MustCompile(`^(accepts|requires) .*arg\(s\), (only )?received \d+$`)
	cobraMutuallyExclusive = regexp.MustCompile(`^if any flags in the group \[.+\] are set none of the others can be; \[(.+)\] were all set$`)
	cobraRequiredTogether  = regexp.MustCompile(`^if any flags in the group \[(.+)\] are set they must all be set; missing \[(.+)\]$`)
	cobraOneRequired       = regexp.MustCompile(`^at least one of the flags in the group \[(.+)\] is required$`)
)

// unknownCommandMessage says that name is not a subcommand of the group
// cmd, which ones come close and which ones there are.
func unknownCommandMessage(cmd *cobra.Command, name string) string {
	msg := fmt.Sprintf("unknown command %q for %q", name, cmd.CommandPath())
	// SuggestionsFor needs a distance; cobra only defaults it on its own
	// path.
	if cmd.SuggestionsMinimumDistance <= 0 {
		cmd.SuggestionsMinimumDistance = 2
	}
	if suggestions := cmd.SuggestionsFor(name); len(suggestions) > 0 {
		msg += fmt.Sprintf(" (did you mean %s?)", strings.Join(suggestions, ", "))
	}
	var names []string
	for _, sub := range cmd.Commands() {
		if sub.IsAvailableCommand() {
			names = append(names, sub.Name())
		}
	}
	return msg + ": its commands are " + strings.Join(names, ", ")
}

// argsUsage returns the arguments of cmd as its usage line names them
// ("<target> [text]"), "" when it takes none.
func argsUsage(cmd *cobra.Command) []string {
	var out []string
	for _, field := range strings.Fields(cmd.Use)[1:] {
		if field != "[flags]" {
			out = append(out, field)
		}
	}
	return out
}

func arityMessage(cmd *cobra.Command, args []string) string {
	expected := argsUsage(cmd)
	takes := "no arguments"
	if len(expected) > 0 {
		takes = strings.Join(expected, " ")
	}
	got := "no arguments"
	if len(args) > 0 {
		shown := make([]string, len(args))
		for i, a := range args {
			shown[i] = strconv.Quote(abbreviate(a))
		}
		got = fmt.Sprintf("%d%s (%s)", len(args), plural(len(args), " argument", " arguments"), strings.Join(shown, ", "))
	}
	msg := fmt.Sprintf("%q takes %s, got %s", cmd.CommandPath(), takes, got)
	if TakesText(cmd) && len(args) > len(expected) {
		msg += ": quote the text so that the shell passes it as one argument"
	}
	return msg
}

// abbreviate shortens an argument quoted in an error (message text can be
// long).
func abbreviate(s string) string {
	const max = 40
	if r := []rune(s); len(r) > max {
		return string(r[:max]) + "…"
	}
	return s
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func flagMessage(cmd *cobra.Command, err error) string {
	var invalid *pflag.InvalidValueError
	var unknown *pflag.NotExistError
	switch {
	case errors.As(err, &invalid):
		name, value, cause := "--"+invalid.GetFlag().Name, invalid.GetValue(), invalid.Unwrap()
		// strconv's errors read `strconv.ParseInt: parsing "abc": invalid
		// syntax`: only their reason is of use.
		var num *strconv.NumError
		if errors.As(cause, &num) {
			cause = num.Err
		}
		if errors.Is(cause, strconv.ErrRange) {
			return fmt.Sprintf("%s value %q is out of range", name, value)
		}
		switch kind := invalid.GetFlag().Value.Type(); {
		case kind == "bool":
			return fmt.Sprintf("%s must be true or false, got %q (the flag alone means true)", name, value)
		case strings.HasPrefix(kind, "int") || strings.HasPrefix(kind, "uint"):
			return fmt.Sprintf("%s must be an integer, got %q", name, value)
		case strings.HasPrefix(kind, "float"):
			return fmt.Sprintf("%s must be a number, got %q", name, value)
		}
		return fmt.Sprintf("invalid %s value %q: %v", name, value, cause)
	case errors.As(err, &unknown) && TakesText(cmd):
		// "-1" or "--not really a flag": probably the text.
		given := "--" + unknown.GetSpecifiedName()
		if short := unknown.GetSpecifiedShortnames(); short != "" {
			given = "-" + short
		}
		example := cmd.CommandPath()
		if expected := argsUsage(cmd); len(expected) > 0 {
			example += " " + expected[0]
		}
		return fmt.Sprintf(`%v. If %s is the text, put it after "--" so that it is not read as a flag: %s -- %s`, err, given, example, strconv.Quote(given))
	}
	return err.Error()
}

// missingRequiredFlags lists the flags of cmd marked required
// (cmd.MarkFlagRequired) that were not given.
func missingRequiredFlags(cmd *cobra.Command) []string {
	var missing []string
	cmd.Flags().VisitAll(func(f *pflag.Flag) {
		if required := f.Annotations[cobra.BashCompOneRequiredFlag]; len(required) > 0 && required[0] == "true" && !f.Changed {
			missing = append(missing, f.Name)
		}
	})
	return missing
}

func flagGroupMessage(raw string) string {
	if m := cobraMutuallyExclusive.FindStringSubmatch(raw); m != nil {
		return flagList(strings.Fields(m[1])) + " cannot be used together"
	}
	if m := cobraRequiredTogether.FindStringSubmatch(raw); m != nil {
		return fmt.Sprintf("%s must be used together (missing: %s)", flagList(strings.Fields(m[1])), flagList(strings.Fields(m[2])))
	}
	if m := cobraOneRequired.FindStringSubmatch(raw); m != nil {
		return "one of " + flagList(strings.Fields(m[1])) + " is required"
	}
	return raw
}

// flagList renders flag names as "--a", "--a and --b" or "--a, --b and
// --c".
func flagList(names []string) string {
	flags := make([]string, len(names))
	for i, name := range names {
		flags[i] = "--" + name
	}
	if len(flags) < 2 {
		return strings.Join(flags, "")
	}
	last := len(flags) - 1
	return strings.Join(flags[:last], ", ") + " and " + flags[last]
}
