package root

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/maelvls/slackcli/internal/appinfo"
	"github.com/maelvls/slackcli/internal/audit"
	"github.com/maelvls/slackcli/internal/auth"
	"github.com/maelvls/slackcli/internal/clictx"
	"github.com/maelvls/slackcli/internal/cmd/cmdutil"
	"github.com/maelvls/slackcli/internal/slackapi"
	"github.com/maelvls/slackcli/internal/testenv"
	"github.com/maelvls/slackcli/internal/trace"
)

// result is what one run of the CLI produced.
type result struct {
	code           int
	stdout, stderr string
	rt             *clictx.Context
}

// runWith runs the real root (flags, watchdog, exit codes) over the given
// top-level commands, with an empty credentials store.
func runWith(t *testing.T, grace time.Duration, build func(rt *clictx.Context) []*cobra.Command, args ...string) result {
	t.Helper()
	return runRunner(t, runner{commands: build, grace: grace}, args...)
}

// runRunner is runWith for a runner the test set up itself (r.newContext
// is replaced).
func runRunner(t *testing.T, r runner, args ...string) result {
	t.Helper()
	for _, name := range []string{"SLACK_TOKEN", "SLACK_COOKIE_D", "SLACK_WORKSPACE_URL"} {
		t.Setenv(name, "")
	}
	// The real commands must not find the developer's configuration.
	testenv.Isolate(t)
	var stdout, stderr bytes.Buffer
	// Set by the goroutine of the command, which a run that timed out
	// does not wait for.
	var rt atomic.Pointer[clictx.Context]
	path := filepath.Join(t.TempDir(), "credentials.json")
	r.newContext = func() *clictx.Context {
		c := &clictx.Context{Store: &auth.Store{Path: path}}
		rt.Store(c)
		return c
	}
	code := r.run(args, &stdout, &stderr)
	return result{code: code, stdout: stdout.String(), stderr: stderr.String(), rt: rt.Load()}
}

// The tests expect an exact stderr: a live trace that the developer left
// on in their shell must not reach them.
func TestMain(m *testing.M) {
	os.Unsetenv(appinfo.EnvName(trace.EnvSuffix))
	os.Unsetenv(appinfo.LegacyEnvPrefix + trace.EnvSuffix)
	os.Exit(m.Run())
}

// one wraps a single test command.
func one(cmd *cobra.Command) func(*clictx.Context) []*cobra.Command {
	return func(*clictx.Context) []*cobra.Command { return []*cobra.Command{cmd} }
}

func TestRunSuccessAndErrors(t *testing.T) {
	build := func(rt *clictx.Context) []*cobra.Command {
		return []*cobra.Command{
			{Use: "ok", Args: cobra.NoArgs, RunE: func(*cobra.Command, []string) error {
				return rt.Print(map[string]any{"ok": true})
			}},
			{Use: "fail", RunE: func(*cobra.Command, []string) error {
				return fmt.Errorf("resolving the channel: %w", errors.New("channel_not_found"))
			}},
		}
	}

	got := runWith(t, time.Second, build, "ok")
	if got.code != ExitOK || got.stdout != "{\"ok\":true}\n" || got.stderr != "" {
		t.Errorf("ok: %+v", got)
	}

	// The error is printed once, on stderr, without usage.
	got = runWith(t, time.Second, build, "fail")
	if got.code != ExitError || got.stdout != "" || got.stderr != "resolving the channel: channel_not_found\n" {
		t.Errorf("fail: %+v", got)
	}

	// A command line cobra rejects points at --help.
	got = runWith(t, time.Second, build, "nope")
	if got.code != ExitError || !strings.Contains(got.stderr, `unknown command "nope"`) ||
		!strings.Contains(got.stderr, `Run "`+appinfo.Name+` --help" for usage.`) {
		t.Errorf("unknown command: %+v", got)
	}
	got = runWith(t, time.Second, build, "ok", "--nope")
	if got.code != ExitError || !strings.Contains(got.stderr, "unknown flag: --nope") || !strings.Contains(got.stderr, "--help") {
		t.Errorf("unknown flag: %+v", got)
	}
	// An error of the command itself is not followed by a pointer to help.
	if got = runWith(t, time.Second, build, "fail"); strings.Contains(got.stderr, "--help") {
		t.Errorf("fail: %+v", got)
	}
	got = runWith(t, time.Second, build, "ok", "extra")
	want := `"` + appinfo.Name + ` ok" takes no arguments, got 1 argument ("extra")` + "\n" + `Run "` + appinfo.Name + ` ok --help" for usage.` + "\n"
	if got.code != ExitError || got.stdout != "" || got.stderr != want {
		t.Errorf("extra argument: %+v", got)
	}

	// No arguments: help, successfully.
	got = runWith(t, time.Second, build)
	if got.code != ExitOK || !strings.Contains(got.stdout, "Available Commands") || !strings.Contains(got.stdout, "--safe-mode") {
		t.Errorf("no arguments: %+v", got)
	}
}

func TestRunTimeoutCancelsTheCommandAndLetsItCleanUp(t *testing.T) {
	t.Setenv(appinfo.EnvName("COMMAND_TIMEOUT_MS"), "40")
	var cancelled, cleaned atomic.Bool
	slow := &cobra.Command{Use: "slow", RunE: func(cmd *cobra.Command, _ []string) error {
		defer cleaned.Store(true) // a deferred cleanup, e.g. removing a temp file
		select {
		case <-cmd.Context().Done():
			cancelled.Store(true)
			return cmd.Context().Err()
		case <-time.After(10 * time.Second):
			return nil
		}
	}}
	start := time.Now()
	got := runWith(t, 5*time.Second, one(slow), "slow")
	if got.code != ExitTimeout {
		t.Fatalf("exit code = %d, want %d (%+v)", got.code, ExitTimeout, got)
	}
	if !cancelled.Load() || !cleaned.Load() {
		t.Errorf("context cancelled = %v, cleanup ran = %v", cancelled.Load(), cleaned.Load())
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("returned after %s: it should not wait for the whole grace period", elapsed)
	}
	for _, want := range []string{
		appinfo.Name + " command timed out after 40ms",
		"may still have gone through: check before retrying",
		"SLACKCLI_COMMAND_TIMEOUT_MS",
	} {
		if !strings.Contains(got.stderr, want) {
			t.Errorf("stderr %q does not contain %q", got.stderr, want)
		}
	}
	// The cancellation is reported as a timeout, not as an interruption.
	if strings.Contains(got.stderr, "interrupted") || strings.Count(got.stderr, "\n") != 1 {
		t.Errorf("stderr = %q", got.stderr)
	}
}

func TestRunTimeoutDoesNotWaitForACommandThatIgnoresCancellation(t *testing.T) {
	t.Setenv(appinfo.EnvName("COMMAND_TIMEOUT_MS"), "30")
	release := make(chan struct{})
	finished := make(chan struct{})
	stuck := &cobra.Command{Use: "stuck", RunE: func(*cobra.Command, []string) error {
		defer close(finished)
		<-release
		return nil
	}}
	start := time.Now()
	got := runWith(t, 50*time.Millisecond, one(stuck), "stuck")
	if got.code != ExitTimeout || time.Since(start) > 2*time.Second {
		t.Errorf("code = %d after %s", got.code, time.Since(start))
	}
	close(release)
	<-finished
}

// The watchdog is running before anything else is: what a command does
// while it is being set up (reading the credentials, building the command
// tree) is bounded like the command itself.
func TestRunTimeoutCoversTheSetUpOfTheCommand(t *testing.T) {
	t.Setenv(appinfo.EnvName("COMMAND_TIMEOUT_MS"), "30")
	release := make(chan struct{})
	defer close(release)
	start := time.Now()
	got := runWith(t, 50*time.Millisecond, func(*clictx.Context) []*cobra.Command {
		<-release // e.g. a keychain that does not answer
		return nil
	}, "anything")
	if got.code != ExitTimeout || time.Since(start) > 2*time.Second {
		t.Errorf("code = %d after %s (%q)", got.code, time.Since(start), got.stderr)
	}
}

// A set-up that ends once the timeout was reported must not start the
// command: one that ignores its context (removing saved credentials, say)
// would do its work after the user was told that it did not.
func TestRunTimeoutDoesNotStartACommandWhoseSetUpWasLate(t *testing.T) {
	t.Setenv(appinfo.EnvName("COMMAND_TIMEOUT_MS"), "30")
	var started atomic.Bool
	finished := make(chan struct{})
	got := runWith(t, 5*time.Second, func(*clictx.Context) []*cobra.Command {
		time.Sleep(300 * time.Millisecond) // well past the timeout
		return []*cobra.Command{{Use: "remove", RunE: func(*cobra.Command, []string) error {
			started.Store(true)
			return nil
		}, PersistentPostRun: func(*cobra.Command, []string) { close(finished) }}}
	}, "remove")
	if got.code != ExitTimeout || started.Load() {
		t.Errorf("code = %d, command started = %v (%q)", got.code, started.Load(), got.stderr)
	}
	select {
	case <-finished:
		t.Error("the command ran to its end")
	default:
	}
}

// Nor does a set-up that ends while the timeout is being reported, before
// the command is told to stop: that takes as long as stderr does.
func TestRunTimeoutDoesNotStartACommandWhileItIsReported(t *testing.T) {
	t.Setenv(appinfo.EnvName("COMMAND_TIMEOUT_MS"), "30")
	testenv.Isolate(t)
	var started atomic.Bool
	hookRan := make(chan struct{})
	stderr := blockedWriter{release: make(chan struct{})}
	defer close(stderr.release)
	r := runner{
		newContext: func() *clictx.Context {
			return &clictx.Context{Store: &auth.Store{Path: filepath.Join(t.TempDir(), "credentials.json")}}
		},
		commands: func(*clictx.Context) []*cobra.Command {
			// Past the timeout, and well within the time the message
			// of the timeout is given to be written (the grace period).
			time.Sleep(300 * time.Millisecond)
			defer close(hookRan)
			return []*cobra.Command{{Use: "remove", RunE: func(*cobra.Command, []string) error {
				started.Store(true)
				return nil
			}}}
		},
		grace: 2 * time.Second,
	}
	var stdout bytes.Buffer
	if code := r.run([]string{"remove"}, &stdout, stderr); code != ExitTimeout {
		t.Errorf("code = %d", code)
	}
	<-hookRan
	if started.Load() {
		t.Error("the command was started after the timeout")
	}
}

// A command that returned on the very tick its timeout, or a signal,
// struck on has returned: its own result is what counts, every time.
func TestWaitPrefersACommandThatReturned(t *testing.T) {
	result := errors.New("the result of the command")
	for range 500 {
		done, timedOut, signalled := make(chan error, 1), make(chan expiry, 1), make(chan struct{})
		done <- result
		timedOut <- expiry{awake: time.Second, wall: time.Hour}
		close(signalled)
		if err, how, late := wait(done, timedOut, signalled); err != result || how != finished || late != (expiry{}) {
			t.Fatalf("wait = %v, %v, %+v; want the command's result", err, how, late)
		}
	}

	late := expiry{awake: time.Minute, wall: time.Minute}
	timedOut := make(chan expiry, 1)
	timedOut <- late
	if err, how, got := wait(make(chan error), timedOut, nil); err != nil || how != expired || got != late {
		t.Errorf("timeout: wait = %v, %v, %+v", err, how, got)
	}
	signalled := make(chan struct{})
	close(signalled)
	if err, how, _ := wait(make(chan error), make(chan expiry), signalled); err != nil || how != interruptedBefore {
		t.Errorf("signal: wait = %v, %v", err, how)
	}
}

// blockedWriter is a stderr that is a full pipe nobody reads.
type blockedWriter struct{ release chan struct{} }

func (w blockedWriter) Write(p []byte) (int, error) {
	<-w.release
	return len(p), nil
}

// Neither the message of a timeout nor the trace keeps the process from
// exiting with 124 when stderr blocks.
func TestRunTimeoutDoesNotWaitOnABlockedStderr(t *testing.T) {
	for _, debug := range []string{"", "1"} {
		t.Setenv(appinfo.EnvName("COMMAND_TIMEOUT_MS"), "30")
		t.Setenv(appinfo.EnvName(trace.EnvSuffix), debug)
		testenv.Isolate(t)
		stderr := blockedWriter{release: make(chan struct{})}
		r := runner{
			newContext: func() *clictx.Context {
				return &clictx.Context{Store: &auth.Store{Path: filepath.Join(t.TempDir(), "credentials.json")}}
			},
			commands: one(&cobra.Command{Use: "slow", RunE: func(cmd *cobra.Command, _ []string) error {
				span := trace.Begin("api conversations.history", "")
				<-cmd.Context().Done()
				span.EndErr(cmd.Context().Err()) // a trace line, when the trace is on
				return cmd.Context().Err()
			}}),
			grace: 100 * time.Millisecond,
		}
		start := time.Now()
		var stdout bytes.Buffer
		code := r.run([]string{"slow"}, &stdout, stderr)
		if elapsed := time.Since(start); code != ExitTimeout || elapsed > 3*time.Second {
			t.Errorf("debug=%q: code = %d after %s", debug, code, elapsed)
		}
		close(stderr.release)
	}
}

// A command that times out says what it spent its time on and what it was
// still waiting for, without SLACKCLI_DEBUG.
func TestRunTimeoutSaysWhereTheTimeWent(t *testing.T) {
	t.Setenv(appinfo.EnvName("COMMAND_TIMEOUT_MS"), "40")
	slow := &cobra.Command{Use: "slow", RunE: func(cmd *cobra.Command, _ []string) error {
		trace.Begin("api conversations.info", "").End("HTTP 200")
		trace.Begin("api users.info", "").Fail("HTTP 429")
		trace.Begin("api users.info", "").End("HTTP 200")
		span := trace.Begin("api conversations.history", "(attempt 2)")
		<-cmd.Context().Done()
		span.EndErr(cmd.Context().Err())
		return cmd.Context().Err()
	}}
	got := runWith(t, time.Second, one(slow), "slow")
	if got.code != ExitTimeout {
		t.Fatalf("%+v", got)
	}
	lines := strings.Split(strings.TrimSuffix(got.stderr, "\n"), "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[0], appinfo.Name+" command timed out after 40ms.") {
		t.Fatalf("stderr = %q", got.stderr)
	}
	want := regexp.MustCompile(`^Before the timeout, time spent: api (conversations|users)\.info \S+( \(2 calls, 1 failed\))?, api (conversations|users)\.info \S+( \(2 calls, 1 failed\))?; ` +
		`still running: api conversations\.history \(attempt 2\) for \S+\. Set SLACKCLI_DEBUG=1 to trace every call as it is made\.$`)
	if !want.MatchString(lines[1]) || !strings.Contains(lines[1], "api users.info") || !strings.Contains(lines[1], "(2 calls, 1 failed)") {
		t.Errorf("summary = %q", lines[1])
	}
}

// Timers stand still while a Mac sleeps: the watchdog also reads the wall
// clock, and says so when that is what ended the command.
func TestRunTimeoutEndsACommandTheMachineSleptThrough(t *testing.T) {
	t.Setenv(appinfo.EnvName("COMMAND_TIMEOUT_MS"), "30000")
	var asleep atomic.Bool
	real := stopwatch()
	r := runner{
		grace: time.Second,
		commands: one(&cobra.Command{Use: "slow", RunE: func(cmd *cobra.Command, _ []string) error {
			asleep.Store(true) // the lid is closed, and opened 8 minutes later
			<-cmd.Context().Done()
			return cmd.Context().Err()
		}}),
		elapsed: func() (time.Duration, time.Duration) {
			awake, _ := real()
			if asleep.Load() {
				return awake, awake + 8*time.Minute
			}
			return awake, awake
		},
	}
	start := time.Now()
	got := runRunner(t, r, "slow")
	if got.code != ExitTimeout || time.Since(start) > 5*time.Second {
		t.Fatalf("code = %d after %s (%q)", got.code, time.Since(start), got.stderr)
	}
	for _, want := range []string{
		appinfo.Name + " command timed out after 30s.",
		"\nThe clock moved 8m0", "s forward while the command only ran for ", "the machine was asleep",
	} {
		if !strings.Contains(got.stderr, want) {
			t.Errorf("stderr %q does not contain %q", got.stderr, want)
		}
	}
}

func TestWatch(t *testing.T) {
	clock := func(awake, wall time.Duration) func() (time.Duration, time.Duration) {
		return func() (time.Duration, time.Duration) { return awake, wall }
	}
	for name, tc := range map[string]struct {
		awake, wall time.Duration
		expires     bool
		asleep      bool
	}{
		"within the limit":                        {awake: time.Millisecond, wall: time.Millisecond},
		"limit reached":                           {awake: time.Minute, wall: time.Minute, expires: true},
		"slept through the limit":                 {awake: time.Millisecond, wall: time.Hour, expires: true, asleep: true},
		"wall clock set backwards":                {awake: time.Millisecond, wall: -time.Hour},
		"wall clock set backwards, limit reached": {awake: time.Minute, wall: -time.Hour, expires: true},
	} {
		t.Run(name, func(t *testing.T) {
			const timeout = 20 * time.Millisecond // a tick every millisecond
			stopped, timedOut := make(chan struct{}), make(chan expiry, 1)
			exited := make(chan struct{})
			go func() {
				defer close(exited)
				watch(timeout, clock(tc.awake, tc.wall), stopped, timedOut)
			}()
			if !tc.expires {
				select {
				case late := <-timedOut:
					t.Fatalf("expired: %+v", late)
				case <-time.After(30 * time.Millisecond):
				}
				close(stopped)
				<-exited // stopping it ends it
				return
			}
			select {
			case late := <-timedOut:
				if late.awake != tc.awake || late.wall != tc.wall || late.asleep(timeout) != tc.asleep {
					t.Errorf("expiry = %+v, asleep = %v", late, late.asleep(timeout))
				}
			case <-time.After(5 * time.Second):
				t.Fatal("did not expire")
			}
			<-exited
		})
	}
}

// SLACKCLI_DEBUG=1 traces the run on stderr, and nothing of it is written
// without it.
func TestRunLiveTrace(t *testing.T) {
	work := one(&cobra.Command{Use: "work", RunE: func(*cobra.Command, []string) error {
		trace.Begin("api conversations.history", "").End("HTTP 200")
		return nil
	}})
	if got := runWith(t, time.Second, work, "work", "secret-argument"); got.code != ExitOK || got.stderr != "" {
		t.Errorf("without the variable: %+v", got)
	}

	t.Setenv(appinfo.EnvName(trace.EnvSuffix), "1")
	t.Setenv(appinfo.EnvName("COMMAND_TIMEOUT_MS"), "30000")
	t.Setenv(appinfo.EnvName("API_TIMEOUT_MS"), "20000")
	got := runWith(t, time.Second, work, "work", "secret-argument")
	lines := strings.Split(strings.TrimSuffix(got.stderr, "\n"), "\n")
	if got.code != ExitOK || got.stdout != "" || len(lines) != 3 {
		t.Fatalf("%+v", got)
	}
	for i, want := range []string{
		`^\[slackcli \+\S+\] slackcli \S+, command "work": command timeout 30s, API timeout 20s$`,
		`^\[slackcli \+\S+\] api conversations\.history: \S+, HTTP 200$`,
		`^\[slackcli \+\S+\] exit code 0; time spent: api conversations\.history \S+$`,
	} {
		if !regexp.MustCompile(want).MatchString(lines[i]) {
			t.Errorf("line %d = %q, want %s", i, lines[i], want)
		}
	}
	if strings.Contains(got.stderr, "secret-argument") {
		t.Errorf("the arguments are traced: %q", got.stderr)
	}

	// A failure is traced too, after the error itself.
	got = runWith(t, time.Second, work, "nope")
	if got.code != ExitError || !regexp.MustCompile(`\n\[slackcli \+\S+\] exit code 1\n$`).MatchString(got.stderr) {
		t.Errorf("unknown command: %+v", got)
	}
}

// The lines of the audit log name the command that made the calls, with
// only what is safe to keep of its arguments.
func TestRunNamesTheCommandInTheAuditLog(t *testing.T) {
	log := filepath.Join(t.TempDir(), "audit.jsonl")
	t.Setenv(appinfo.EnvName(audit.EnvSuffix), log)
	work := one(&cobra.Command{Use: "work", RunE: func(cmd *cobra.Command, _ []string) error {
		_, err := slackapi.NewFake(func(context.Context, string, map[string]any) (map[string]any, error) {
			return nil, nil
		}).API(cmd.Context(), "conversations.history", nil)
		return err
	}})
	if got := runWith(t, time.Second, work, "work", "C0123ABCD", "secret-argument"); got.code != ExitOK {
		t.Fatalf("%+v", got)
	}
	entries, skipped, err := audit.Read(log, time.Time{})
	if err != nil || skipped != 0 || len(entries) != 1 {
		t.Fatalf("entries = %+v, skipped %d, %v", entries, skipped, err)
	}
	e := entries[0]
	if e.Cmd != "work" || strings.Join(e.Args, " ") != "work C0123ABCD "+audit.Placeholder || e.Method != "conversations.history" {
		t.Errorf("entry = %+v", e)
	}
}

// Of the arguments that start with a dash, only the flags of the command
// are kept, by name: not the value of a flag, even one that looks like a
// flag, nor message text that starts with a dash, nor what follows "--".
func TestAuditLogKeepsOnlyTheFlagsOfTheCommand(t *testing.T) {
	log := filepath.Join(t.TempDir(), "audit.jsonl")
	t.Setenv(appinfo.EnvName(audit.EnvSuffix), log)
	work := &cobra.Command{Use: "work", Args: cobra.ArbitraryArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		_, err := slackapi.NewFake(func(context.Context, string, map[string]any) (map[string]any, error) {
			return nil, nil
		}).API(cmd.Context(), "chat.postMessage", nil)
		return err
	}}
	work.Flags().String("text", "", "")
	work.Flags().Bool("force", false, "")
	work.Flags().IntP("limit", "n", 0, "")
	cmdutil.AllowDashText(work)
	got := runWith(t, time.Second, one(work), "work", "C0123ABCD", "--text", "--confidential plan",
		"- first item", "--force", "-n", "5", "--", "--secret", "-1")
	if got.code != ExitOK {
		t.Fatalf("%+v", got)
	}
	entries, _, err := audit.Read(log, time.Time{})
	if err != nil || len(entries) != 1 {
		t.Fatalf("entries = %+v, %v", entries, err)
	}
	p := audit.Placeholder
	want := strings.Join([]string{"work", "C0123ABCD", "--text", p, p, "--force", "-n", p, "--", p, p}, " ")
	if args := strings.Join(entries[0].Args, " "); args != want {
		t.Errorf("args = %q, want %q", args, want)
	}
}

// A command can ask how long it has left, for as long as it has a limit.
func TestRunTellsTheCommandHowLongItHasLeft(t *testing.T) {
	t.Setenv(appinfo.EnvName("COMMAND_TIMEOUT_MS"), "30000")
	var left, after time.Duration
	var limited, limitedAfter bool
	got := runWith(t, time.Second, func(rt *clictx.Context) []*cobra.Command {
		return []*cobra.Command{{Use: "work", RunE: func(cmd *cobra.Command, _ []string) error {
			left, limited = slackapi.TimeLeft(cmd.Context())
			rt.DisableTimeout()
			after, limitedAfter = slackapi.TimeLeft(cmd.Context())
			return nil
		}}}
	}, "work")
	if got.code != ExitOK || !limited || left > 30*time.Second || left < 20*time.Second {
		t.Errorf("%+v: %s left, limited = %v", got, left, limited)
	}
	if limitedAfter || after != 0 {
		t.Errorf("without a timeout: %s left, limited = %v", after, limitedAfter)
	}
}

func TestRunNoTimeout(t *testing.T) {
	// Long enough for a loaded machine to get to the command before the
	// watchdog, which runs from the very start, strikes.
	t.Setenv(appinfo.EnvName("COMMAND_TIMEOUT_MS"), "200")
	wait := func(*cobra.Command, []string) error {
		time.Sleep(600 * time.Millisecond)
		return nil
	}

	// A command marked NoTimeout waits on a human: no watchdog.
	got := runWith(t, time.Second, one(clictx.NoTimeout(&cobra.Command{Use: "edit", RunE: wait})), "edit")
	if got.code != ExitOK || got.stderr != "" {
		t.Errorf("NoTimeout: %+v", got)
	}

	// A command can also decide so at run time.
	got = runWith(t, time.Second, func(rt *clictx.Context) []*cobra.Command {
		return []*cobra.Command{{Use: "send", RunE: func(cmd *cobra.Command, args []string) error {
			rt.DisableTimeout()
			return wait(cmd, args)
		}}}
	}, "send")
	if got.code != ExitOK || got.stderr != "" {
		t.Errorf("DisableTimeout: %+v", got)
	}

	// Without either, the same command times out.
	got = runWith(t, time.Second, one(&cobra.Command{Use: "send", RunE: wait}), "send")
	if got.code != ExitTimeout {
		t.Errorf("plain command: %+v", got)
	}
}

// --safe-mode turns safe mode on, and --safe-mode=false cannot turn off a
// safe mode that the environment turned on.
func TestRunSafeModeFlagCannotDisableTheEnvironment(t *testing.T) {
	for _, tc := range []struct {
		env, legacy string
		args        []string
		want        bool
	}{
		{"", "", []string{"probe"}, false},
		{"", "", []string{"probe", "--safe-mode"}, true},
		{"", "", []string{"--safe-mode", "probe"}, true},
		{"", "", []string{"probe", "--safe-mode=false"}, false},
		{"1", "", []string{"probe"}, true},
		{"1", "", []string{"probe", "--safe-mode=false"}, true},
		{"1", "", []string{"--safe-mode=false", "probe"}, true},
		{"enabled", "", []string{"probe", "--safe-mode=false"}, true},
		{"false", "1", []string{"probe", "--safe-mode=false"}, true},
		{"0", "", []string{"probe", "--safe-mode"}, true},
	} {
		t.Setenv("SLACKCLI_SAFE_MODE", tc.env)
		t.Setenv("AGENT_SLACK_SAFE_MODE", tc.legacy)
		got := runWith(t, time.Second, func(rt *clictx.Context) []*cobra.Command {
			return []*cobra.Command{{Use: "probe", RunE: func(*cobra.Command, []string) error {
				return rt.Print(map[string]any{"safe_mode": rt.SafeMode(), "blocked": rt.CheckSafeMode("message delete") != nil})
			}}}
		}, tc.args...)
		want := fmt.Sprintf("{\"blocked\":%[1]v,\"safe_mode\":%[1]v}\n", tc.want)
		if got.code != ExitOK || got.stdout != want {
			t.Errorf("SLACKCLI_SAFE_MODE=%q AGENT_SLACK_SAFE_MODE=%q %v: %+v, want safe mode %v", tc.env, tc.legacy, tc.args, got, tc.want)
		}
	}
}

// -V and --version print the bare version, as the predecessor did, even
// when the build is stamped with the "v" of a version tag.
func TestVersionFlags(t *testing.T) {
	old := appinfo.Version
	t.Cleanup(func() { appinfo.Version = old })
	appinfo.Version = "v1.2.3"

	for _, flag := range []string{"-V", "--version", "-v"} {
		got := runWith(t, time.Second, commands, flag)
		if got.code != ExitOK || got.stdout != "1.2.3\n" || got.stderr != "" {
			t.Errorf("%s: %+v", flag, got)
		}
	}
	help := runWith(t, time.Second, commands, "--help").stdout
	if !strings.Contains(help, "-V, --version") || strings.Contains(help, "version-short") {
		t.Errorf("help does not advertise -V, or shows the hidden alias:\n%s", help)
	}
}

// The real command tree builds, and every top-level command is there.
func TestCommandTree(t *testing.T) {
	cmd := New(&clictx.Context{Store: &auth.Store{Path: filepath.Join(t.TempDir(), "credentials.json")}})
	var names []string
	for _, c := range cmd.Commands() {
		names = append(names, c.Name())
	}
	// "completion" and "help" are cobra's, added up front (see newRoot).
	want := "audit auth canvas channel completion help inbox later message search unreads user watch workflow"
	if got := strings.Join(names, " "); got != want {
		t.Errorf("commands = %s, want %s", got, want)
	}
}

func TestErrorMessage(t *testing.T) {
	if ErrorMessage(nil) != "" || ErrorMessage(context.DeadlineExceeded) != "context deadline exceeded" {
		t.Error("ErrorMessage")
	}
}
