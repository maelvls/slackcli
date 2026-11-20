// Package root assembles the command tree and runs it.
package root

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/maelvls/slackcli/internal/appinfo"
	"github.com/maelvls/slackcli/internal/clictx"
	"github.com/maelvls/slackcli/internal/cmd/authcmd"
	"github.com/maelvls/slackcli/internal/cmd/canvascmd"
	"github.com/maelvls/slackcli/internal/cmd/channelcmd"
	"github.com/maelvls/slackcli/internal/cmd/cmdutil"
	"github.com/maelvls/slackcli/internal/cmd/latercmd"
	"github.com/maelvls/slackcli/internal/cmd/messagecmd"
	"github.com/maelvls/slackcli/internal/cmd/searchcmd"
	"github.com/maelvls/slackcli/internal/cmd/unreadscmd"
	"github.com/maelvls/slackcli/internal/cmd/usercmd"
	"github.com/maelvls/slackcli/internal/cmd/workflowcmd"
	"github.com/maelvls/slackcli/internal/slack/enrich"
	"github.com/maelvls/slackcli/internal/slackapi"
	"github.com/maelvls/slackcli/internal/trace"
)

// Exit codes.
const (
	ExitOK      = 0
	ExitError   = 1
	ExitTimeout = 124
)

const defaultCommandTimeout = 30 * time.Second

// CommandTimeout is the global per-command time limit
// (SLACKCLI_COMMAND_TIMEOUT_MS; default 30s).
func CommandTimeout() time.Duration {
	return appinfo.EnvDurationMS("COMMAND_TIMEOUT_MS", defaultCommandTimeout, false)
}

// timeoutGrace is how long a command that timed out is given to notice
// that its context was cancelled and return, so that its deferred cleanups
// run (temporary copies of cookie databases, partial downloads…).
const timeoutGrace = 2 * time.Second

// New builds the root command.
func New(rt *clictx.Context) *cobra.Command {
	return newRoot(rt, commands(rt))
}

// commands returns the top-level commands.
func commands(rt *clictx.Context) []*cobra.Command {
	return []*cobra.Command{
		authcmd.New(rt),
		messagecmd.New(rt),
		canvascmd.New(rt),
		searchcmd.New(rt),
		latercmd.New(rt),
		unreadscmd.New(rt),
		usercmd.New(rt),
		channelcmd.New(rt),
		workflowcmd.New(rt),
	}
}

// newRoot builds the root command (global flags, version, help) over the
// given top-level commands.
func newRoot(rt *clictx.Context, subcommands []*cobra.Command) *cobra.Command {
	var version, shortVersion bool
	cmd := &cobra.Command{
		Use:   appinfo.Name,
		Short: appinfo.Description,
		Long: fmt.Sprintf(`%s.

Commands print JSON on stdout and hints and warnings on stderr; see the help
of each command for what it prints.

%s
It fails closed: --safe-mode=false does not turn off a safe mode that the
environment turned on, and any value of %s other than
empty, 0, false, no or off turns it on.`,
			appinfo.Description, clictx.SafeModeHelp(), appinfo.EnvName("SAFE_MODE")),
		SilenceUsage:  true,
		SilenceErrors: true,
		// The version is printed here rather than by cobra (cmd.Version),
		// which would do it before looking at the arguments: "--version
		// extra" is a mistake, not a request for the version.
		RunE: func(cmd *cobra.Command, _ []string) error {
			if version || shortVersion {
				// Without the "v" of a version tag, like the User-Agent.
				_, err := fmt.Fprintln(cmd.OutOrStdout(), appinfo.BareVersion())
				return err
			}
			return cmd.Help()
		},
	}
	cmd.CompletionOptions.HiddenDefaultCmd = true
	// -V is the predecessor's shorthand; -v stays as a hidden alias.
	cmd.Flags().BoolVarP(&version, "version", "V", false, "Print the version of "+appinfo.Name)
	cmd.Flags().BoolVarP(&shortVersion, "version-short", "v", false, "Print the version (alias of -V)")
	_ = cmd.Flags().MarkHidden("version-short")
	// One line: this flag is listed in the help of every command.
	cmd.PersistentFlags().BoolVar(&rt.SafeModeFlag, "safe-mode", false,
		fmt.Sprintf(`Keep a human in the loop: messages are sent from a review editor, other writes are refused (also %s=1; see "%s --help")`,
			appinfo.EnvName("SAFE_MODE"), appinfo.Name))

	cmd.AddCommand(subcommands...)
	cmd.SetHelpCommand(newHelp(cmd))
	// cobra adds "help" and "completion" when the command is executed:
	// add them now, so that they are checked like the others.
	cmd.InitDefaultHelpCmd()
	cmd.InitDefaultCompletionCmd()
	cmd.SetFlagErrorFunc(cmdutil.FlagError)
	checkUsage(cmd)
	return cmd
}

// checkUsage makes cmd and every command below it report a command line
// they cannot accept as a *cmdutil.UsageError, which Run words for the
// user and follows with a pointer to that command's help.
//
// It also closes a gap in cobra, which only validates the arguments of a
// command that can run: a group of commands without a RunE of its own
// ("auth", "channel") would answer "auth status" with its help and exit
// code 0. Such a group gets one: its help without arguments, an "unknown
// command" error otherwise. Doing it here covers every group, present and
// future.
func checkUsage(cmd *cobra.Command) {
	for _, sub := range cmd.Commands() {
		checkUsage(sub)
	}
	if cmd.HasSubCommands() && !cmd.Runnable() {
		cmd.Args = cobra.NoArgs
		cmd.RunE = func(c *cobra.Command, _ []string) error { return c.Help() }
	}
	if !cmd.HasParent() {
		cmd.Args = cobra.NoArgs
	}
	validate := cmd.Args
	cmd.Args = func(c *cobra.Command, args []string) error { return cmdutil.CheckArgs(c, validate, args) }
}

// newHelp returns the "help" command. Unlike cobra's, it fails when asked
// about a command that does not exist, instead of printing the root's help
// and succeeding.
func newHelp(root *cobra.Command) *cobra.Command {
	return &cobra.Command{
		Use:   "help [command]",
		Short: "Help about any command",
		Long: fmt.Sprintf(`Print the help of a command: "%[1]s help message send" is
"%[1]s message send --help".`, appinfo.Name),
		// The mistake is about the commands of the root: that is whose
		// help the error points at.
		Args: func(_ *cobra.Command, args []string) error {
			return cmdutil.CheckArgs(root, func(*cobra.Command, []string) error {
				if target, rest, err := root.Find(args); err != nil || target == nil || len(rest) > 0 {
					return fmt.Errorf("unknown help topic %q: it is not a command of %s", strings.Join(args, " "), appinfo.Name)
				}
				return nil
			}, args)
		},
		RunE: func(_ *cobra.Command, args []string) error {
			target, _, err := root.Find(args)
			if err != nil {
				return err
			}
			target.InitDefaultHelpFlag()
			return target.Help()
		},
	}
}

// Run executes the CLI with the given arguments (without the program name)
// and returns the process exit code.
func Run(args []string, stdout, stderr io.Writer) int {
	return runner{newContext: clictx.New, commands: commands, grace: timeoutGrace}.run(args, stdout, stderr)
}

// runner is Run with its dependencies spelled out, so that tests can run
// the real root (flags, watchdog, exit codes) over commands of their own.
type runner struct {
	newContext func() *clictx.Context
	commands   func(rt *clictx.Context) []*cobra.Command
	// grace is how long a command gets to return after a timeout.
	grace time.Duration
}

func (r runner) run(args []string, stdout, stderr io.Writer) int {
	rt := r.newContext()
	rt.Stdout, rt.Stderr = stdout, stderr
	trace.Reset(stderr)

	signalCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// cancel is how a timeout tells the command to stop.
	ctx, cancel := context.WithCancel(signalCtx)
	defer cancel()

	// Run the root's PersistentPreRunE even if a sub-command defines its own.
	cobra.EnableTraverseRunHooks = true

	cmd := newRoot(rt, r.commands(rt))
	cmd.SetArgs(args)
	cmd.SetOut(stdout)
	cmd.SetErr(stderr)

	warnAboutEnvironment(stderr)

	// The watchdog bounds every command that does not wait on a human, so
	// that an agent is never left hanging on a stuck network call. It
	// starts here, before the command line is even parsed, so that nothing
	// a command does can escape it; a command that waits on a human stops
	// it (clictx.NoTimeout, Context.DisableTimeout).
	timeout := CommandTimeout()
	var (
		once     sync.Once
		stopped  = make(chan struct{})
		timedOut = make(chan struct{})
	)
	stopWatchdog := func() { once.Do(func() { close(stopped) }) }
	rt.SetWatchdogCanceller(stopWatchdog)
	go watch(timeout, stopped, timedOut)

	// ran is the command cobra selected, once its command line was
	// accepted.
	var ran *cobra.Command
	cmd.PersistentPreRunE = func(c *cobra.Command, _ []string) error {
		// cobra checks these itself, later; doing it here words the
		// failure like the other usage errors.
		if err := cmdutil.CheckFlags(c); err != nil {
			return err
		}
		ran = c
		if clictx.HasNoTimeout(c) {
			stopWatchdog()
		}
		return nil
	}

	// Optional lookups that Slack rate limited do not fail a command: they
	// report here, and the user is told the result is incomplete.
	ctx, incomplete := enrich.Collect(ctx)

	done := make(chan error, 1)
	go func() { done <- cmd.ExecuteContext(ctx) }()

	var err error
	select {
	case err = <-done:
		stopWatchdog()
	case <-timedOut:
		// The command is still running: what it was doing may or may not
		// have reached Slack, and saying "timed out" alone invites a
		// retry that posts twice.
		fmt.Fprintf(stderr, "%s command timed out after %s. If it was sending or changing something, that may still have gone through: check before retrying. Set %s (milliseconds) to adjust.\n",
			appinfo.Name, timeout, appinfo.EnvName("COMMAND_TIMEOUT_MS"))
		// Where the time went, so that a transcript is enough to tell a
		// slow Slack from a rate limit or a locked keychain.
		if summary := trace.Summary(); summary != "" {
			fmt.Fprintf(stderr, "Before the timeout, %s. Set %s=1 to trace every call as it is made.\n", summary, appinfo.EnvName(trace.EnvSuffix))
		}
		// Let the command see the cancellation and return, so that its
		// deferred cleanups run; do not wait for one that ignores it.
		cancel()
		grace := time.NewTimer(r.grace)
		defer grace.Stop()
		select {
		case <-done:
		case <-grace.C:
		}
		return ExitTimeout
	}

	if err != nil {
		if errors.Is(err, context.Canceled) && signalCtx.Err() != nil {
			// As for a timeout: what a writing command was doing may
			// have reached Slack all the same.
			if ran != nil && clictx.ChangesSlack(commandName(ran)) {
				fmt.Fprintln(stderr, "interrupted. What the command was sending or changing may still have gone through: check before retrying.")
			} else {
				fmt.Fprintln(stderr, "interrupted")
			}
			return 130
		}
		var usage *cmdutil.UsageError
		if errors.As(err, &usage) {
			// The command line was rejected: say how, then where the
			// usage of the command that rejected it is.
			fmt.Fprintln(stderr, usage.Message())
			fmt.Fprintf(stderr, "Run \"%s --help\" for usage.\n", usage.Cmd.CommandPath())
			return ExitError
		}
		fmt.Fprintln(stderr, ErrorMessage(err))
		return ExitError
	}
	for _, w := range incomplete.List() {
		fmt.Fprintln(stderr, "Warning: "+w)
	}
	return ExitOK
}

// watch closes timedOut once timeout has passed, unless stopped is closed
// first.
//
// It looks at two clocks. A timer alone reads the monotonic clock, which
// on macOS stands still while the machine sleeps: a command started just
// before the lid was closed would then run for "30 seconds" that last the
// whole night, as would every HTTP timeout inside it. The wall clock does
// not stop, so whichever clock says the time is up first wins. (A wall
// clock set forward by more than the timeout in the middle of a command
// ends it early; that is the lesser evil.)
func watch(timeout time.Duration, stopped <-chan struct{}, timedOut chan<- struct{}) {
	start := time.Now()
	wallStart := start.Round(0) // without the monotonic reading
	tick := time.NewTicker(min(max(timeout/20, time.Millisecond), 250*time.Millisecond))
	defer tick.Stop()
	for {
		select {
		case <-stopped:
			return
		case <-tick.C:
			if time.Since(start) >= timeout || time.Now().Round(0).Sub(wallStart) >= timeout {
				close(timedOut)
				return
			}
		}
	}
}

// commandName returns the path of c below the root ("message send").
func commandName(c *cobra.Command) string {
	return strings.TrimPrefix(c.CommandPath(), c.Root().Name()+" ")
}

// durationVariables are the environment variables read as a duration in
// milliseconds (appinfo.EnvDurationMS), by suffix, and whether 0 is a
// value they accept. The packages that read them fall back to their
// default on a value they cannot use; Run warns about it.
var durationVariables = []struct {
	suffix    string
	allowZero bool
}{
	{"COMMAND_TIMEOUT_MS", false},
	{"API_TIMEOUT_MS", false},
	{"RATE_LIMIT_MAX_WAIT_MS", true},
	{"KEYCHAIN_TIMEOUT_MS", false},
}

// warnAboutEnvironment reports, one line each, the duration variables
// whose value is not usable. A warning rather than an error: the default
// that applies instead is a sound one, and a stray variable must not keep
// "--help" or "auth whoami" from running; but whoever set it, to a value
// that does nothing, has to be told.
func warnAboutEnvironment(stderr io.Writer) {
	const ignored = "Warning: %v. The variable is ignored and the default applies.\n"
	for _, v := range durationVariables {
		if err := appinfo.CheckEnvDurationMS(v.suffix, v.allowZero); err != nil {
			fmt.Fprintf(stderr, ignored, err)
		}
	}
	// The one duration variable that is not tool-specific.
	if err := slackapi.CheckLegacyAPITimeout(); err != nil {
		fmt.Fprintf(stderr, ignored, err)
	}
}

// ErrorMessage renders an error for the user on a single line.
func ErrorMessage(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
