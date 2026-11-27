// Package root assembles the command tree and runs it.
package root

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/maelvls/slackcli/internal/appinfo"
	"github.com/maelvls/slackcli/internal/audit"
	"github.com/maelvls/slackcli/internal/clictx"
	"github.com/maelvls/slackcli/internal/cmd/auditcmd"
	"github.com/maelvls/slackcli/internal/cmd/authcmd"
	"github.com/maelvls/slackcli/internal/cmd/canvascmd"
	"github.com/maelvls/slackcli/internal/cmd/channelcmd"
	"github.com/maelvls/slackcli/internal/cmd/cmdutil"
	"github.com/maelvls/slackcli/internal/cmd/inboxcmd"
	"github.com/maelvls/slackcli/internal/cmd/latercmd"
	"github.com/maelvls/slackcli/internal/cmd/messagecmd"
	"github.com/maelvls/slackcli/internal/cmd/searchcmd"
	"github.com/maelvls/slackcli/internal/cmd/unreadscmd"
	"github.com/maelvls/slackcli/internal/cmd/usercmd"
	"github.com/maelvls/slackcli/internal/cmd/watchcmd"
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

// interruptGrace is the same for a command that was interrupted (Ctrl-C,
// SIGTERM). It is longer: a command that waits on a human has no timeout
// behind it, and closes its editor when interrupted.
const interruptGrace = 5 * time.Second

// Run the root's PersistentPreRunE even if a sub-command defines its own.
// It is cobra's global, hence set once, here.
func init() { cobra.EnableTraverseRunHooks = true }

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
		inboxcmd.New(rt),
		watchcmd.New(rt),
		usercmd.New(rt),
		channelcmd.New(rt),
		workflowcmd.New(rt),
		auditcmd.New(rt),
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

Commands print JSON on stdout, with their warnings and hints in it
("warnings", "hints"), and errors on stderr; see the help of each command
for what it prints.

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
	return runner{newContext: clictx.New, commands: commands, grace: timeoutGrace, interruptGrace: interruptGrace}.run(args, stdout, stderr)
}

// runner is Run with its dependencies spelled out, so that tests can run
// the real root (flags, watchdog, exit codes) over commands of their own.
type runner struct {
	newContext func() *clictx.Context
	commands   func(rt *clictx.Context) []*cobra.Command
	// grace is how long a command gets to return after a timeout, and
	// how long the messages of a timeout may take to be written.
	grace time.Duration
	// interruptGrace is how long a command gets to return after a signal
	// (0 means grace).
	interruptGrace time.Duration
	// elapsed is the clock of the watchdog (see stopwatch); nil means the
	// real one. Tests replace it to put the machine to sleep.
	elapsed func() (awake, wall time.Duration)
	// goroutines, when set, counts every goroutine the run starts (the
	// command, the watchdog, the writes to stderr that are not waited
	// for). A process exits with what it leaves running; a test waits on
	// it instead, for nothing it started to outlive it and write to the
	// trace or the audit log of the next one.
	goroutines *sync.WaitGroup
}

// spawn runs f on a goroutine of its own, counted in r.goroutines.
func (r runner) spawn(f func()) {
	if r.goroutines == nil {
		go f()
		return
	}
	r.goroutines.Go(f)
}

// run runs the command and, when the live trace is on, ends it with the
// exit code and what the time went to.
func (r runner) run(args []string, stdout, stderr io.Writer) int {
	code := r.execute(args, stdout, stderr)
	// Not waited for longer than the grace period: a stderr that is a
	// full pipe nobody reads must not keep the process from exiting.
	r.within(r.grace, func() {
		if summary := trace.Summary(); summary != "" {
			trace.Logf("exit code %d; %s", code, summary)
		} else {
			trace.Logf("exit code %d", code)
		}
	})
	return code
}

// within runs f and returns once it is done or d has passed, whichever
// comes first; f is left running in the second case.
func (r runner) within(d time.Duration, f func()) {
	done := make(chan struct{})
	r.spawn(func() {
		defer close(done)
		f()
	})
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-done:
	case <-t.C:
	}
}

func (r runner) execute(args []string, stdout, stderr io.Writer) int {
	trace.Reset(stderr)
	audit.Reset()

	// The watchdog bounds every command that does not wait on a human, so
	// that an agent is never left hanging on a stuck network call. It is
	// the first thing started, and everything else (setting up the
	// credentials store, building the commands, parsing the command line,
	// running the command) happens on another goroutine, so that nothing
	// can escape it; a command that waits on a human stops it
	// (clictx.NoTimeout, Context.DisableTimeout).
	//
	// Nor does this goroutine wait on stderr for long when a command
	// times out: a pipe nobody reads, once full, blocks whoever writes to
	// it (see within).
	timeout := CommandTimeout()
	deadline := time.Now().Add(timeout)
	elapsed := r.elapsed
	if elapsed == nil {
		elapsed = stopwatch()
	}
	var (
		once     sync.Once
		stopped  = make(chan struct{})
		timedOut = make(chan expiry, 1)
	)
	stopWatchdog := func() { once.Do(func() { close(stopped) }) }
	r.spawn(func() { watch(timeout, elapsed, stopped, timedOut) })

	signalCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// cancel is how a timeout tells the command to stop.
	ctx, cancel := context.WithCancel(signalCtx)
	defer cancel()

	// Optional lookups that Slack rate limited do not fail a command: they
	// report here, and the user is told the result is incomplete (the
	// command's context is handed the collector below).
	ctx, incomplete := enrich.Collect(ctx)
	// What may last (one more try of a call that timed out) can ask how
	// long the command has left.
	ctx = slackapi.WithCommandDeadline(ctx, func() (time.Time, bool) {
		select {
		case <-stopped:
			return time.Time{}, false
		default:
			return deadline, true
		}
	})

	// ran is the command cobra selected, once its command line was
	// accepted.
	var ran atomic.Pointer[cobra.Command]
	// timedOutFlag is set once the command timeout has struck.
	var timedOutFlag atomic.Bool
	done := make(chan error, 1)
	// The context of the command is set up by the goroutine below, under
	// the watchdog like the rest: it is handed over here once it is ready,
	// for its warnings and hints to be written when the command is over.
	ready := make(chan *clictx.Context, 1)
	var (
		rtMu sync.Mutex
		rt   *clictx.Context
	)
	// flushNotes writes on stderr the warnings and hints that no result
	// carried (clictx.Context.FlushNotes). It does nothing when the time
	// was up, or the signal came, before there was a context to record
	// one. It may be called from several goroutines (see within): one
	// that a blocked stderr holds does not hold the next.
	flushNotes := func() {
		rtMu.Lock()
		if rt == nil {
			select {
			case rt = <-ready:
			default:
			}
		}
		c := rt
		rtMu.Unlock()
		if c != nil {
			c.FlushNotes()
		}
	}
	r.spawn(func() {
		rt := r.newContext()
		rt.Stdout, rt.Stderr = stdout, stderr
		rt.SetWatchdogCanceller(stopWatchdog)
		rt.Incomplete = incomplete
		ready <- rt

		cmd := newRoot(rt, r.commands(rt))
		cmd.SetArgs(args)
		cmd.SetOut(stdout)
		cmd.SetErr(stderr)

		warnAboutEnvironment(rt)

		cmd.PersistentPreRunE = func(c *cobra.Command, _ []string) error {
			// cobra checks these itself, later; doing it here words the
			// failure like the other usage errors.
			if err := cmdutil.CheckFlags(c); err != nil {
				return err
			}
			// A set-up that only got here once the command was timed out
			// or interrupted must not start it: this process is about to
			// exit, and has said that the command did not run to its end.
			if timedOutFlag.Load() {
				return errTimedOut
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			ran.Store(c)
			// The audit log names the command on each of its lines, with
			// only what is safe to keep of its arguments.
			audit.SetCommand(commandName(c), args, flagLookup(c))
			// The trace names the command, not its arguments: they may
			// hold a message or a secret.
			if clictx.HasNoTimeout(c) {
				stopWatchdog()
				trace.Logf("%s %s, command %q: no command timeout (it may wait on a human), API timeout %s",
					appinfo.Name, appinfo.BareVersion(), commandName(c), slackapi.APITimeout())
			} else {
				trace.Logf("%s %s, command %q: command timeout %s, API timeout %s",
					appinfo.Name, appinfo.BareVersion(), commandName(c), timeout, slackapi.APITimeout())
			}
			return nil
		}
		done <- cmd.ExecuteContext(ctx)
	})

	// interrupted says so, the way that fits the command.
	interrupted := func() int {
		// As for a timeout: what a writing command was doing may have
		// reached Slack all the same.
		if c := ran.Load(); c != nil && clictx.ChangesSlack(commandName(c)) {
			fmt.Fprintln(stderr, "interrupted. What the command was sending or changing may still have gone through: check before retrying.")
		} else {
			fmt.Fprintln(stderr, "interrupted")
		}
		return 130
	}

	err, how, late := wait(done, timedOut, signalCtx.Done())
	switch how {
	case finished:
		stopWatchdog()
	case interruptedBefore:
		// The command sees the cancellation and returns, most of the
		// time. One that does not (a set-up or a call that ignores its
		// context) is not waited for: Ctrl-C must end the process, and
		// with exit code 130, also when there is no timeout behind it.
		stopWatchdog()
		grace := time.NewTimer(cmp.Or(r.interruptGrace, r.grace))
		defer grace.Stop()
		select {
		case err = <-done:
		case <-grace.C:
			// What the command had to say so far, then that it was
			// interrupted; neither holds the exit when stderr blocks.
			r.within(r.grace, func() {
				flushNotes()
				interrupted()
			})
			return 130
		}
	case expired:
		// The command is still running: what it was doing may or may not
		// have reached Slack, and saying "timed out" alone invites a
		// retry that posts twice.
		//
		// From here on nothing new is started: a set-up that ends now
		// does not get to run its command (see the pre-run hook). The
		// command itself is only told to stop once the messages are
		// written, so that what it writes on its way out comes after
		// them; but neither that nor the exit waits on a stderr that
		// blocks.
		timedOutFlag.Store(true)
		r.within(r.grace, func() {
			fmt.Fprintf(stderr, "%s command timed out after %s. If it was sending or changing something, that may still have gone through: check before retrying. Set %s (milliseconds) to adjust.\n",
				appinfo.Name, timeout, appinfo.EnvName("COMMAND_TIMEOUT_MS"))
			if late.asleep(timeout) {
				// The limit was reached on the wall clock only (see watch).
				fmt.Fprintf(stderr, "The clock moved %s forward while the command only ran for %s: the machine was asleep (or its clock was set forward), and the command was not left to resume.\n",
					late.wall.Round(100*time.Millisecond), late.awake.Round(time.Millisecond))
			}
			// Where the time went, so that a transcript is enough to tell
			// a slow Slack from a rate limit or a locked keychain.
			if summary := trace.Summary(); summary != "" && trace.Enabled() {
				fmt.Fprintf(stderr, "Before the timeout, %s.\n", summary)
			} else if summary != "" {
				fmt.Fprintf(stderr, "Before the timeout, %s. Set %s=1 to trace every call as it is made.\n", summary, appinfo.EnvName(trace.EnvSuffix))
			}
			// The warnings and hints no result carried.
			flushNotes()
		})
		// Let the command see the cancellation and return, so that its
		// deferred cleanups run; do not wait for one that ignores it.
		cancel()
		grace := time.NewTimer(r.grace)
		defer grace.Stop()
		select {
		case <-done:
		case <-grace.C:
		}
		// What the command said while it was cleaning up; like the
		// messages above, not waited for when stderr blocks.
		r.within(r.grace, flushNotes)
		return ExitTimeout
	}

	// The warnings and hints are in the JSON the command printed. Those
	// it did not print (it failed, was interrupted, or prints text) go to
	// stderr.
	flushNotes()
	if err != nil {
		if errors.Is(err, context.Canceled) && signalCtx.Err() != nil {
			return interrupted()
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
	return ExitOK
}

// errTimedOut is what a command that was not started because the timeout
// had struck fails with. Nobody sees it: the run ends with ExitTimeout.
var errTimedOut = errors.New("the command timed out before it started")

// ending is how the wait for a command ended.
type ending int

const (
	// finished: the command returned.
	finished ending = iota
	// expired: the command timeout struck first.
	expired
	// interruptedBefore: a signal came first.
	interruptedBefore
)

// wait waits for the command to return (done), for its timeout or for a
// signal, and says which came first, with the error of the command or
// what the watchdog saw.
//
// A command that has returned has returned, whatever else is true by
// then: when the timeout or a signal is ready at the same time (a command
// that ends on the tick the watchdog strikes on, typically right after
// the machine woke up), it is the command's own result that counts.
// Reporting a write that went through as timed out would invite sending
// it again.
func wait(done <-chan error, timedOut <-chan expiry, signalled <-chan struct{}) (error, ending, expiry) {
	select {
	case err := <-done:
		return err, finished, expiry{}
	case <-signalled:
		select {
		case err := <-done:
			return err, finished, expiry{}
		default:
			return nil, interruptedBefore, expiry{}
		}
	case late := <-timedOut:
		select {
		case err := <-done:
			return err, finished, expiry{}
		default:
			return nil, expired, late
		}
	}
}

// expiry is what the two clocks of the watchdog showed when the time of a
// command was up.
type expiry struct {
	// awake is the time the process saw go by; wall is how far the clock
	// on the wall moved.
	awake, wall time.Duration
}

// asleep reports whether the limit was only reached on the wall clock:
// the machine slept, or its clock was set forward.
func (e expiry) asleep(timeout time.Duration) bool { return e.awake < timeout }

// stopwatch returns the clock of the watchdog: the time since this call,
// as the process saw it go by (the monotonic clock) and as the clock on
// the wall shows it.
func stopwatch() func() (awake, wall time.Duration) {
	start := time.Now()
	wallStart := start.Round(0) // without the monotonic reading
	return func() (time.Duration, time.Duration) {
		now := time.Now()
		return now.Sub(start), now.Round(0).Sub(wallStart)
	}
}

// watch sends on timedOut (which must have room for it) once timeout has
// passed on either clock of elapsed, unless stopped is closed first.
//
// A timer alone would not do. Timers, and with them every HTTP timeout,
// follow the monotonic clock, which stands still while the machine sleeps,
// on macOS (mach_absolute_time) as on Linux (CLOCK_MONOTONIC does not
// count the time suspended): a command caught by the lid closing would
// have its "30 seconds" last the whole night. The wall clock does
// not stop, so whichever clock says the time is up first wins; they are
// read every time the ticker gets to fire, which it does soon after the
// machine wakes. (A wall clock set forward by more than the timeout in
// the middle of a command ends it early; that is the lesser evil, and the
// message says what happened.)
func watch(timeout time.Duration, elapsed func() (awake, wall time.Duration), stopped <-chan struct{}, timedOut chan<- expiry) {
	tick := time.NewTicker(min(max(timeout/20, time.Millisecond), 250*time.Millisecond))
	defer tick.Stop()
	for {
		select {
		case <-stopped:
			return
		case <-tick.C:
			if awake, wall := elapsed(); awake >= timeout || wall >= timeout {
				timedOut <- expiry{awake: awake, wall: wall}
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
func warnAboutEnvironment(rt *clictx.Context) {
	const ignored = "%v. The variable is ignored and the default applies."
	for _, v := range durationVariables {
		if err := appinfo.CheckEnvDurationMS(v.suffix, v.allowZero); err != nil {
			rt.Warnf(ignored, err)
		}
	}
	// The one duration variable that is not tool-specific.
	if err := slackapi.CheckLegacyAPITimeout(); err != nil {
		rt.Warnf(ignored, err)
	}
}

// ErrorMessage renders an error for the user on a single line.
func ErrorMessage(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// flagLookup tells the audit log which arguments of c's command line are
// its flags (its own and those it inherits), and which of them take a
// value: only their names are kept.
func flagLookup(c *cobra.Command) audit.FlagLookup {
	return func(name string, short bool) (bool, bool) {
		for _, fs := range []*pflag.FlagSet{c.Flags(), c.InheritedFlags()} {
			var f *pflag.Flag
			if short {
				f = fs.ShorthandLookup(name)
			} else {
				f = fs.Lookup(name)
			}
			if f != nil {
				return true, f.NoOptDefVal == ""
			}
		}
		return false, false
	}
}
