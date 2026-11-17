package cmdutil

import (
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

// usageMessage runs "tool <cmd> <args>" with the usage checks wired the
// way the root wires them, and returns the Message of the *UsageError it
// fails with.
func usageMessage(t *testing.T, cmd *cobra.Command, args ...string) string {
	t.Helper()
	if !TakesText(cmd) {
		validate := cmd.Args
		cmd.Args = func(c *cobra.Command, a []string) error { return CheckArgs(c, validate, a) }
	}
	if cmd.RunE == nil {
		cmd.RunE = func(*cobra.Command, []string) error { return nil }
	}
	cmd.PreRunE = func(c *cobra.Command, _ []string) error { return CheckFlags(c) }
	tool := &cobra.Command{Use: "tool", SilenceUsage: true, SilenceErrors: true}
	tool.SetFlagErrorFunc(FlagError)
	tool.SetOut(io.Discard)
	tool.SetErr(io.Discard)
	tool.AddCommand(cmd)
	tool.SetArgs(append([]string{cmd.Name()}, args...))
	err := tool.Execute()
	var usage *UsageError
	if !errors.As(err, &usage) {
		t.Fatalf("tool %s %q: err = %v, want a *UsageError", cmd.Name(), args, err)
	}
	if usage.Cmd != cmd {
		t.Errorf("tool %s %q: the error is attributed to %q", cmd.Name(), args, usage.Cmd.CommandPath())
	}
	return usage.Message()
}

// textCommand is a command taking free text, like "message send".
func textCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:  "send <target> [text] [flags]",
		Args: cobra.RangeArgs(1, 2),
		RunE: func(*cobra.Command, []string) error { return nil },
	}
	cmd.Flags().String("thread-ts", "", "")
	return AllowDashText(cmd)
}

// Usage errors are worded for the reader. Cobra's argument validators and
// flag groups only produce strings, which UsageError.Message recognises:
// this test runs cobra itself, so it fails if cobra changes its wording.
func TestUsageMessages(t *testing.T) {
	flags := func(cmd *cobra.Command) *cobra.Command {
		f := cmd.Flags()
		f.Int("limit", 10, "")
		f.Bool("force", false, "")
		f.Float64("ratio", 1, "")
		f.Duration("wait", time.Second, "")
		f.Int8("tiny", 0, "")
		f.String("a", "", "")
		f.String("b", "", "")
		f.String("c", "", "")
		return cmd
	}
	plain := func(use string, args cobra.PositionalArgs) *cobra.Command {
		return flags(&cobra.Command{Use: use, Args: args})
	}
	required := func(names ...string) *cobra.Command {
		cmd := plain("run", cobra.NoArgs)
		for _, name := range names {
			if err := cmd.MarkFlagRequired(name); err != nil {
				t.Fatal(err)
			}
		}
		return cmd
	}
	exclusive, together, oneOf := plain("run", cobra.NoArgs), plain("run", cobra.NoArgs), plain("run", cobra.NoArgs)
	exclusive.MarkFlagsMutuallyExclusive("a", "b", "c")
	together.MarkFlagsRequiredTogether("a", "b", "c")
	oneOf.MarkFlagsOneRequired("a", "b")

	for _, tc := range []struct {
		name string
		cmd  *cobra.Command
		args []string
		want string
	}{
		// Arity: the arguments are named as in the usage line.
		{"exact, none given", plain("get <target>", cobra.ExactArgs(1)), nil, `"tool get" takes <target>, got no arguments`},
		{"exact, too many", plain("get <target>", cobra.ExactArgs(1)), []string{"a", "b"}, `"tool get" takes <target>, got 2 arguments ("a", "b")`},
		{"minimum", plain("add <file>...", cobra.MinimumNArgs(1)), nil, `"tool add" takes <file>..., got no arguments`},
		{"maximum", plain("show [id]", cobra.MaximumNArgs(1)), []string{"a", "b"}, `"tool show" takes [id], got 2 arguments ("a", "b")`},
		{"range", plain("put <a> [b]", cobra.RangeArgs(1, 2)), nil, `"tool put" takes <a> [b], got no arguments`},
		{"no arguments", plain("whoami", cobra.NoArgs), []string{"extra"}, `"tool whoami" takes no arguments, got 1 argument ("extra")`},
		{"text, too many", textCommand(), []string{"C1", "hello", "world"},
			`"tool send" takes <target> [text], got 3 arguments ("C1", "hello", "world"): quote the text so that the shell passes it as one argument`},
		{"text, too few", textCommand(), nil, `"tool send" takes <target> [text], got no arguments`},
		{"long arguments are cut", plain("get <target>", cobra.ExactArgs(1)), []string{"a", strings.Repeat("x", 50)},
			`got 2 arguments ("a", "` + strings.Repeat("x", 40) + `…")`},
		{"a validator's own error is kept", plain("get <target>", func(*cobra.Command, []string) error { return errors.New("the target is odd") }), nil, "the target is odd"},

		// Flag values: no trace of strconv.
		{"integer", plain("list", cobra.NoArgs), []string{"--limit", "abc"}, `--limit must be an integer, got "abc"`},
		{"integer, fractional", plain("list", cobra.NoArgs), []string{"--limit=1.5"}, `--limit must be an integer, got "1.5"`},
		{"integer, empty", plain("list", cobra.NoArgs), []string{"--limit", ""}, `--limit must be an integer, got ""`},
		{"integer, out of range", plain("list", cobra.NoArgs), []string{"--tiny", "1000"}, `--tiny value "1000" is out of range`},
		{"boolean", plain("list", cobra.NoArgs), []string{"--force=maybe"}, `--force must be true or false, got "maybe" (the flag alone means true)`},
		{"number", plain("list", cobra.NoArgs), []string{"--ratio", "half"}, `--ratio must be a number, got "half"`},
		{"other types", plain("list", cobra.NoArgs), []string{"--wait", "soon"}, `invalid --wait value "soon": time: invalid duration "soon"`},
		{"unknown flag", plain("list", cobra.NoArgs), []string{"--nope"}, "unknown flag: --nope"},
		{"missing value", plain("list", cobra.NoArgs), []string{"--limit"}, "flag needs an argument: --limit"},

		// Text that looks like a flag.
		{"text, dash number", textCommand(), []string{"C1", "-1"},
			`unknown shorthand flag: '1' in -1. If -1 is the text, put it after "--" so that it is not read as a flag: tool send <target> -- "-1"`},
		{"text, dash word", textCommand(), []string{"C1", "--nope"},
			`unknown flag: --nope. If --nope is the text, put it after "--" so that it is not read as a flag: tool send <target> -- "--nope"`},
		{"text, missing value", textCommand(), []string{"C1", "x", "--thread-ts"}, "flag needs an argument: --thread-ts"},

		// Required flags and flag groups.
		{"one required flag", required("a"), nil, "--a is required"},
		{"two required flags", required("a", "b"), nil, "--a and --b are required"},
		{"three required flags, one given", required("a", "b", "c"), []string{"--b", "x"}, "--a and --c are required"},
		{"mutually exclusive", exclusive, []string{"--a", "x", "--c", "y"}, "--a and --c cannot be used together"},
		{"required together", together, []string{"--b", "x"}, "--a, --b and --c must be used together (missing: --a and --c)"},
		{"one of", oneOf, nil, "one of --a and --b is required"},
	} {
		got := usageMessage(t, tc.cmd, tc.args...)
		if tc.name == "long arguments are cut" {
			if !strings.Contains(got, tc.want) {
				t.Errorf("%s: message = %q, want it to contain %q", tc.name, got, tc.want)
			}
			continue
		}
		if got != tc.want {
			t.Errorf("%s:\n got %q\nwant %q", tc.name, got, tc.want)
		}
		for _, artefact := range []string{"strconv", "arg(s)", "flags in the group", "required flag(s)"} {
			if strings.Contains(got, artefact) {
				t.Errorf("%s: message %q still has %q", tc.name, got, artefact)
			}
		}
	}
}

// Text of several words that starts with a dash needs no "--"; a single
// word that looks like a flag does.
func TestAllowDashTextRule(t *testing.T) {
	for arg, text := range map[string]bool{
		"- item":        true,
		"-1 for me":     true,
		"--> see below": true,
		"- a=b":         true,
		"-1":            false,
		"-h":            false,
		"--force":       false,
		"--a=b c":       false, // "--a" could be a flag with the value "b c"
		"plain":         false,
	} {
		if got := isDashText(arg); got != text {
			t.Errorf("isDashText(%q) = %v, want %v", arg, got, text)
		}
	}
	if !TakesText(textCommand()) || TakesText(&cobra.Command{Use: "get"}) {
		t.Error("TakesText is wrong")
	}
}

func TestCheckRange(t *testing.T) {
	if err := CheckRange("--max-messages", 5, 10, "x"); err != nil {
		t.Error(err)
	}
	for _, n := range []int{0, -1, 11} {
		err := CheckRange("--max-messages", n, 10, "use --counts-only to fetch no messages")
		if err == nil || !strings.Contains(err.Error(), "invalid --max-messages value") || !strings.Contains(err.Error(), "between 1 and 10 (use --counts-only") {
			t.Errorf("%d: err = %v", n, err)
		}
	}
}

// An argument given to a group of commands is an unknown command, reported
// with the names that come close and those that exist.
func TestUsageMessageUnknownSubcommand(t *testing.T) {
	group := &cobra.Command{Use: "auth", Args: cobra.NoArgs}
	run := func(*cobra.Command, []string) error { return nil }
	group.AddCommand(
		&cobra.Command{Use: "whoami", RunE: run},
		&cobra.Command{Use: "test", RunE: run},
		&cobra.Command{Use: "secret", RunE: run, Hidden: true},
	)
	for arg, want := range map[string]string{
		"status": `unknown command "status" for "tool auth": its commands are test, whoami`,
		"whoam":  `unknown command "whoam" for "tool auth" (did you mean whoami?): its commands are test, whoami`,
	} {
		if got := usageMessage(t, group, arg); got != want {
			t.Errorf("%s:\n got %q\nwant %q", arg, got, want)
		}
	}
}
