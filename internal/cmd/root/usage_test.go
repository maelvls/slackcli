package root

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/maelvls/slackcli/internal/appinfo"
	"github.com/maelvls/slackcli/internal/auth"
	"github.com/maelvls/slackcli/internal/clictx"
	"github.com/maelvls/slackcli/internal/slackapi"
)

// groups returns the path (below the root) of every command of the real
// tree that has subcommands, the root itself included (as "").
func groups(t *testing.T) []string {
	t.Helper()
	var paths []string
	var walk func(cmd *cobra.Command)
	walk = func(cmd *cobra.Command) {
		if cmd.HasSubCommands() {
			paths = append(paths, strings.TrimPrefix(strings.TrimPrefix(cmd.CommandPath(), appinfo.Name), " "))
		}
		for _, sub := range cmd.Commands() {
			walk(sub)
		}
	}
	walk(New(&clictx.Context{Store: &auth.Store{Path: filepath.Join(t.TempDir(), "credentials.json")}}))
	return paths
}

// Every group of commands, at any depth, refuses a subcommand it does not
// have: exit code 1 and an error naming the group and its help, never the
// help on stdout with exit code 0. A new group is covered without doing
// anything.
func TestEveryGroupRejectsAnUnknownSubcommand(t *testing.T) {
	paths := groups(t)
	for _, want := range []string{"", "auth", "canvas", "channel", "completion", "later", "message", "message draft", "message react", "message scheduled", "search", "user", "workflow"} {
		if !slices.Contains(paths, want) {
			t.Fatalf("the groups %q do not include %q", paths, want)
		}
	}
	for _, path := range paths {
		full := strings.TrimSpace(appinfo.Name + " " + path)
		got := runWith(t, time.Second, commands, append(strings.Fields(path), "bogus", "x")...)
		if got.code != ExitError || got.stdout != "" {
			t.Errorf("%s bogus x: exit code %d, stdout %q", full, got.code, got.stdout)
		}
		for _, want := range []string{`unknown command "bogus" for "` + full + `"`, `"` + full + ` --help"`} {
			if !strings.Contains(got.stderr, want) {
				t.Errorf("%s bogus x: stderr %q does not contain %q", full, got.stderr, want)
			}
		}

		// Without arguments a group prints its help, successfully; these
		// two run their "list" instead.
		if path == "later" || path == "message scheduled" {
			continue
		}
		got = runWith(t, time.Second, commands, strings.Fields(path)...)
		if got.code != ExitOK || got.stderr != "" || !strings.Contains(got.stdout, "Usage:") || !strings.Contains(got.stdout, "Available Commands:") {
			t.Errorf("%s: %+v", full, got)
		}
	}
}

// The six groups that used to print their help for anything.
func TestGroupsWithoutACommandOfTheirOwn(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"channel", "#general"}, `unknown command "#general" for "slackcli channel": its commands are invite, list, mark, new`},
		{[]string{"user", "U0AAAAAAAAA"}, `unknown command "U0AAAAAAAAA" for "slackcli user": its commands are dm-open, get, list`},
		{[]string{"canvas", "F0123ABCDEF"}, `unknown command "F0123ABCDEF" for "slackcli canvas": its commands are create, edit, get`},
		{[]string{"auth", "status"}, `unknown command "status" for "slackcli auth": its commands are add, `},
		{[]string{"search", "bogus", "x"}, `unknown command "bogus" for "slackcli search": its commands are all, files, messages`},
		{[]string{"workflow", "bogus", "x"}, `unknown command "bogus" for "slackcli workflow": its commands are get, list, preview, run`},
		{[]string{"auth", "whoam"}, `unknown command "whoam" for "slackcli auth" (did you mean whoami?)`},
		{[]string{"mesage"}, `unknown command "mesage" for "slackcli" (did you mean message?)`},
	} {
		got := runWith(t, time.Second, commands, tc.args...)
		group := appinfo.Name
		if len(tc.args) > 1 {
			group += " " + tc.args[0]
		}
		if got.code != ExitError || got.stdout != "" || !strings.HasPrefix(got.stderr, tc.want) ||
			!strings.HasSuffix(got.stderr, "\nRun \""+group+" --help\" for usage.\n") {
			t.Errorf("%v: %+v", tc.args, got)
		}
	}
}

func TestHelpCommand(t *testing.T) {
	got := runWith(t, time.Second, commands, "help", "message", "send")
	if got.code != ExitOK || got.stderr != "" || !strings.Contains(got.stdout, appinfo.Name+" message send <target> [text] [flags]") || !strings.Contains(got.stdout, "--help") {
		t.Errorf("help message send: %+v", got)
	}
	got = runWith(t, time.Second, commands, "help")
	if got.code != ExitOK || !strings.Contains(got.stdout, "Available Commands:") {
		t.Errorf("help: %+v", got)
	}
	for _, args := range [][]string{{"help", "bogus"}, {"help", "message", "bogus"}} {
		got = runWith(t, time.Second, commands, args...)
		topic := strings.Join(args[1:], " ")
		want := `unknown help topic "` + topic + `": it is not a command of slackcli` + "\n" + `Run "slackcli --help" for usage.` + "\n"
		if got.code != ExitError || got.stdout != "" || got.stderr != want {
			t.Errorf("%v: %+v", args, got)
		}
	}
}

func TestVersionWithAnArgumentIsAnError(t *testing.T) {
	for _, flag := range []string{"--version", "-V", "-v"} {
		got := runWith(t, time.Second, commands, flag, "extra")
		if got.code != ExitError || got.stdout != "" || !strings.Contains(got.stderr, `unknown command "extra" for "`+appinfo.Name+`"`) {
			t.Errorf("%s extra: %+v", flag, got)
		}
	}
}

// A command line that is rejected is explained in the terms of the
// command, and followed by a pointer to the help of that command, whether
// cobra or the command itself did the checking.
func TestUsageErrorsOfTheRealCommands(t *testing.T) {
	const url = "https://acme.slack.com/archives/C1/p1"
	for _, tc := range []struct {
		args    []string
		want    string
		command string
	}{
		{[]string{"message", "get"}, `"slackcli message get" takes <target>, got no arguments`, "message get"},
		{[]string{"message", "send", "C12345678", "hello", "world"},
			`"slackcli message send" takes <target> [text], got 3 arguments ("C12345678", "hello", "world"): quote the text so that the shell passes it as one argument`, "message send"},
		{[]string{"message", "edit", url}, `"slackcli message edit" takes <target> <text>, got 1 argument ("` + url + `")`, "message edit"},
		{[]string{"message", "compose"}, `"slackcli message compose" takes <target> [text], got no arguments`, "message compose"},
		{[]string{"message", "draft", "create", "C12345678"}, `"slackcli message draft create" takes <target> <text>, got 1 argument ("C12345678")`, "message draft create"},
		{[]string{"message", "draft", "update", "Dr1"}, `"slackcli message draft update" takes <draft-id> <text>, got 1 argument ("Dr1")`, "message draft update"},
		{[]string{"auth", "whoami", "extra"}, `"slackcli auth whoami" takes no arguments, got 1 argument ("extra")`, "auth whoami"},
		{[]string{"message", "scheduled", "cancel", "Q1"}, "--channel is required", "message scheduled cancel"},
		{[]string{"channel", "invite"}, "--channel and --users are required", "channel invite"},
		{[]string{"message", "draft", "update", "Dr1", "x", "--broadcast", "--no-broadcast"}, "--broadcast and --no-broadcast cannot be used together", "message draft update"},
		{[]string{"message", "get", url, "--max-body-chars", "abc"}, `--max-body-chars must be an integer, got "abc"`, "message get"},
		{[]string{"message", "list", "C12345678", "--limit", "abc"}, `--limit must be an integer, got "abc"`, "message list"},
		{[]string{"message", "list", "C12345678", "--safe-mode=maybe"}, `--safe-mode must be true or false, got "maybe" (the flag alone means true)`, "message list"},
		{[]string{"message", "send", "C12345678", "-1"},
			`unknown shorthand flag: '1' in -1. If -1 is the text, put it after "--" so that it is not read as a flag: slackcli message send <target> -- "-1"`, "message send"},
		{[]string{"message", "list", "--nope"}, "unknown flag: --nope", "message list"},
	} {
		got := runWith(t, time.Second, commands, tc.args...)
		want := tc.want + "\nRun \"" + appinfo.Name + " " + tc.command + " --help\" for usage.\n"
		if got.code != ExitError || got.stdout != "" || got.stderr != want {
			t.Errorf("%v:\n got %q\nwant %q (code %d, stdout %q)", tc.args, got.stderr, want, got.code, got.stdout)
		}
	}
}

// The global flag is listed in the help of every command: its usage is
// one line, and the detail is in the root's help.
func TestSafeModeHelpIsInOnePlace(t *testing.T) {
	root := New(&clictx.Context{Store: &auth.Store{Path: filepath.Join(t.TempDir(), "credentials.json")}})
	usage := root.PersistentFlags().Lookup("safe-mode").Usage
	if len(usage) > 160 || !strings.Contains(usage, "SLACKCLI_SAFE_MODE=1") || !strings.Contains(usage, `"`+appinfo.Name+` --help"`) {
		t.Errorf("--safe-mode usage (%d characters) = %q", len(usage), usage)
	}
	help := runWith(t, time.Second, commands, "--help").stdout
	for _, want := range []string{clictx.SafeModeHelp(), "It fails closed", "--safe-mode=false does not turn off"} {
		if !strings.Contains(help, want) {
			t.Errorf("the root help does not contain %q:\n%s", want, help)
		}
	}
	sub := runWith(t, time.Second, commands, "channel", "list", "--help").stdout
	if !strings.Contains(sub, "--safe-mode") || strings.Contains(sub, "message scheduled cancel") {
		t.Errorf("the help of a subcommand should name the flag without the detail:\n%s", sub)
	}
}

// The help of every command is wrapped for an 80-column terminal, and its
// examples can be copied: they start with the name of the binary.
func TestHelpIsWrappedAndExamplesAreRunnable(t *testing.T) {
	var walk func(cmd *cobra.Command)
	walk = func(cmd *cobra.Command) {
		for _, line := range strings.Split(cmd.Long, "\n") {
			// An indented line is a command to copy: it is not wrapped.
			if len([]rune(line)) > 80 && !strings.HasPrefix(line, "  ") {
				t.Errorf("%s: help line of %d columns: %q", cmd.CommandPath(), len([]rune(line)), line)
			}
		}
		for _, line := range strings.Split(cmd.Example, "\n") {
			if line = strings.TrimSpace(line); line != "" && !strings.Contains(line, appinfo.Name+" ") {
				t.Errorf("%s: example %q does not call %s", cmd.CommandPath(), line, appinfo.Name)
			}
		}
		for _, sub := range cmd.Commands() {
			// "completion" is cobra's own.
			if sub.Name() != "completion" {
				walk(sub)
			}
		}
	}
	walk(New(&clictx.Context{Store: &auth.Store{Path: filepath.Join(t.TempDir(), "credentials.json")}}))
}

// A duration variable with a value that cannot be used is reported, once,
// and the command runs with the default.
func TestInvalidDurationVariablesAreReported(t *testing.T) {
	ok := func(rt *clictx.Context) []*cobra.Command {
		return []*cobra.Command{{Use: "ok", RunE: func(*cobra.Command, []string) error { return rt.Print(map[string]any{"ok": true}) }}}
	}
	clear := func() {
		for _, v := range durationVariables {
			t.Setenv(appinfo.EnvName(v.suffix), "")
			t.Setenv(appinfo.LegacyEnvPrefix+v.suffix, "")
		}
		t.Setenv(slackapi.LegacyAPITimeoutEnv, "")
	}
	const ignored = ". The variable is ignored and the default applies.\n"
	for _, tc := range []struct{ name, value, want string }{
		{"SLACKCLI_COMMAND_TIMEOUT_MS", "0", `Warning: SLACKCLI_COMMAND_TIMEOUT_MS="0" is not valid: expected a positive number of milliseconds, e.g. 30000` + ignored},
		{"SLACKCLI_COMMAND_TIMEOUT_MS", "-1", `Warning: SLACKCLI_COMMAND_TIMEOUT_MS="-1" is not valid: expected a positive number of milliseconds, e.g. 30000` + ignored},
		{"SLACKCLI_API_TIMEOUT_MS", "abc", `Warning: SLACKCLI_API_TIMEOUT_MS="abc" is not valid: expected a positive number of milliseconds, e.g. 30000` + ignored},
		{"SLACKCLI_RATE_LIMIT_MAX_WAIT_MS", "abc", `Warning: SLACKCLI_RATE_LIMIT_MAX_WAIT_MS="abc" is not valid: expected a number of milliseconds, 0 or more, e.g. 30000` + ignored},
		{"SLACKCLI_KEYCHAIN_TIMEOUT_MS", "3s", `Warning: SLACKCLI_KEYCHAIN_TIMEOUT_MS="3s" is not valid: expected a positive number of milliseconds, e.g. 30000` + ignored},
		{"AGENT_SLACK_API_TIMEOUT_MS", "abc", `Warning: AGENT_SLACK_API_TIMEOUT_MS="abc" is not valid: expected a positive number of milliseconds, e.g. 30000` + ignored},
		{"SLACK_API_TIMEOUT_MS", "abc", `Warning: SLACK_API_TIMEOUT_MS="abc" is not valid: expected a positive number of milliseconds, e.g. 30000` + ignored},
		{"SLACK_API_TIMEOUT_MS", "0", `Warning: SLACK_API_TIMEOUT_MS="0" is not valid: expected a positive number of milliseconds, e.g. 30000` + ignored},
		{"SLACK_API_TIMEOUT_MS", "1500", ""},
		{"SLACKCLI_RATE_LIMIT_MAX_WAIT_MS", "0", ""},
		{"SLACKCLI_COMMAND_TIMEOUT_MS", "5000", ""},
	} {
		clear()
		t.Setenv(tc.name, tc.value)
		got := runWith(t, time.Second, ok, "ok")
		if got.code != ExitOK || got.stderr != tc.want || !strings.Contains(got.stdout, `"ok": true`) {
			t.Errorf("%s=%s: %+v", tc.name, tc.value, got)
		}
	}
	// An unusable legacy variable that is not read, because the current
	// one is set, is not worth a warning.
	clear()
	t.Setenv("SLACK_API_TIMEOUT_MS", "abc")
	t.Setenv("SLACKCLI_API_TIMEOUT_MS", "5000")
	if got := runWith(t, time.Second, ok, "ok"); got.code != ExitOK || got.stderr != "" {
		t.Errorf("SLACK_API_TIMEOUT_MS=abc next to a valid SLACKCLI_API_TIMEOUT_MS: %+v", got)
	}
	clear()
}

// Every variable read with appinfo.EnvDurationMS is one Run checks.
func TestDurationVariablesAreAllChecked(t *testing.T) {
	call := regexp.MustCompile(`appinfo\.EnvDurationMS\("([A-Z_]+)", [^,]+, (true|false)\)`)
	found := 0
	err := filepath.WalkDir("../..", func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, m := range call.FindAllStringSubmatch(string(src), -1) {
			found++
			if !slices.Contains(durationVariables, struct {
				suffix    string
				allowZero bool
			}{m[1], m[2] == "true"}) {
				t.Errorf("%s reads %s (allowZero=%s), which durationVariables does not list that way", path, m[1], m[2])
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if found != len(durationVariables) {
		t.Errorf("found %d calls of appinfo.EnvDurationMS, durationVariables lists %d", found, len(durationVariables))
	}
}
