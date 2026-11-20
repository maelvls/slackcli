//go:build unix

package root

import (
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/maelvls/slackcli/internal/clictx"
)

// Ctrl-C cancels the command's context; the run ends with exit code 130
// and "interrupted", not with the command's own error.
func TestRunInterrupt(t *testing.T) {
	waiting := &cobra.Command{Use: "wait", RunE: func(cmd *cobra.Command, _ []string) error {
		// Run has installed its signal handler by now, so the signal
		// reaches it instead of killing the test binary.
		if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
			return err
		}
		select {
		case <-cmd.Context().Done():
			return cmd.Context().Err()
		case <-time.After(10 * time.Second):
			return nil
		}
	}}
	got := runWith(t, time.Second, one(waiting), "wait")
	if got.code != 130 || got.stderr != "interrupted\n" {
		t.Errorf("got %+v", got)
	}

	// A command that writes to Slack may have got its change through.
	message := &cobra.Command{Use: "message"}
	message.AddCommand(&cobra.Command{Use: "send", RunE: waiting.RunE}, &cobra.Command{Use: "list", RunE: waiting.RunE})
	got = runWith(t, time.Second, one(message), "message", "send")
	if got.code != 130 || got.stderr != "interrupted. What the command was sending or changing may still have gone through: check before retrying.\n" {
		t.Errorf("message send: got %+v", got)
	}
	message = &cobra.Command{Use: "message"}
	message.AddCommand(&cobra.Command{Use: "list", RunE: waiting.RunE})
	if got = runWith(t, time.Second, one(message), "message", "list"); got.code != 130 || got.stderr != "interrupted\n" {
		t.Errorf("message list: got %+v", got)
	}
}

// Ctrl-C ends the process with 130 even when what is running does not
// look at its context, whether it has a timeout behind it or not.
func TestRunInterruptDoesNotWaitForACommandThatIgnoresIt(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	stuck := func(*cobra.Command, []string) error {
		if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
			return err
		}
		<-release
		return nil
	}
	for name, cmd := range map[string]*cobra.Command{
		"with a timeout":    {Use: "stuck", RunE: stuck},
		"without a timeout": clictx.NoTimeout(&cobra.Command{Use: "stuck", RunE: stuck}),
	} {
		start := time.Now()
		got := runRunner(t, runner{commands: one(cmd), grace: time.Second, interruptGrace: 50 * time.Millisecond}, "stuck")
		if elapsed := time.Since(start); got.code != 130 || got.stderr != "interrupted\n" || elapsed > 5*time.Second {
			t.Errorf("%s: %+v after %s", name, got, elapsed)
		}
	}
}

// An interrupted command never prints its result: the warnings it had
// recorded go to stderr before "interrupted", whether it returned or had
// to be left behind.
func TestRunInterruptFlushesTheWarnings(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	build := func(rt *clictx.Context) []*cobra.Command {
		interrupt := func() error {
			rt.Warnf("the uploaded file F1 was left behind")
			return syscall.Kill(os.Getpid(), syscall.SIGINT)
		}
		return []*cobra.Command{
			{Use: "wait", RunE: func(cmd *cobra.Command, _ []string) error {
				if err := interrupt(); err != nil {
					return err
				}
				<-cmd.Context().Done()
				return cmd.Context().Err()
			}},
			{Use: "stuck", RunE: func(*cobra.Command, []string) error {
				if err := interrupt(); err != nil {
					return err
				}
				<-release
				return nil
			}},
		}
	}
	const want = "Warning: the uploaded file F1 was left behind\ninterrupted\n"
	for _, name := range []string{"wait", "stuck"} {
		got := runRunner(t, runner{commands: build, grace: time.Second, interruptGrace: 50 * time.Millisecond}, name)
		if got.code != 130 || got.stdout != "" || got.stderr != want {
			t.Errorf("%s: %+v, want stderr %q", name, got, want)
		}
	}
}
