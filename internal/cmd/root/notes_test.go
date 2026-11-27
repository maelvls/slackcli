package root

import (
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/maelvls/slackcli/internal/appinfo"
	"github.com/maelvls/slackcli/internal/clictx"
	"github.com/maelvls/slackcli/internal/slack/enrich"
)

// Agents run "slackcli … 2>&1 | jq": a command that succeeds says what it
// has to say (warnings, hints) in its JSON, and nothing on stderr. Only
// what no result carried goes there, before the error.
func TestWarningsAreInTheResultNotOnStderr(t *testing.T) {
	build := func(rt *clictx.Context) []*cobra.Command {
		return []*cobra.Command{
			{Use: "read", RunE: func(cmd *cobra.Command, _ []string) error {
				rt.Warnf("file F1 could not be downloaded")
				enrich.Warnf(cmd.Context(), "1 of the 2 users referenced could not be resolved")
				rt.ResultHintf("Nothing else matched.")
				return rt.Print(map[string]any{"messages": []any{"m"}})
			}},
			{Use: "late", RunE: func(cmd *cobra.Command, _ []string) error {
				if err := rt.Print(map[string]any{"ok": true}); err != nil {
					return err
				}
				enrich.Warnf(cmd.Context(), "said after the result")
				return nil
			}},
			{Use: "fail", RunE: func(*cobra.Command, []string) error {
				rt.Warnf("the uploaded files were left behind")
				return errors.New("denied")
			}},
		}
	}

	got := runWith(t, time.Second, build, "read")
	want := `{"messages":["m"],"warnings":["file F1 could not be downloaded","1 of the 2 users referenced could not be resolved"],"hints":["Nothing else matched."]}` + "\n"
	if got.code != ExitOK || got.stdout != want || got.stderr != "" {
		t.Errorf("read: %+v", got)
	}

	got = runWith(t, time.Second, build, "late")
	if got.code != ExitOK || got.stdout != "{\"ok\":true}\n" || got.stderr != "Warning: said after the result\n" {
		t.Errorf("late: %+v", got)
	}

	got = runWith(t, time.Second, build, "fail")
	if got.code != ExitError || got.stdout != "" || got.stderr != "Warning: the uploaded files were left behind\ndenied\n" {
		t.Errorf("fail: %+v", got)
	}
}

// A command that times out never prints its result: what it had to say,
// before the timeout and while cleaning up after it, goes to stderr, once.
func TestTimeoutFlushesTheWarnings(t *testing.T) {
	t.Setenv(appinfo.EnvName("COMMAND_TIMEOUT_MS"), "40")
	var warned atomic.Bool
	build := func(rt *clictx.Context) []*cobra.Command {
		return []*cobra.Command{{Use: "slow", RunE: func(cmd *cobra.Command, _ []string) error {
			rt.Warnf("said before the timeout")
			warned.Store(true)
			<-cmd.Context().Done()
			// The cleanup the grace period is for.
			rt.Warnf("the uploaded file F1 could not be deleted")
			enrich.Warnf(cmd.Context(), "said by a lookup during the cleanup")
			return cmd.Context().Err()
		}}}
	}
	got := runRunner(t, runner{commands: build, grace: 5 * time.Second, elapsed: expiresOnceStarted(&warned, 40*time.Millisecond)}, "slow")
	if got.code != ExitTimeout || got.stdout != "" {
		t.Fatalf("%+v", got)
	}
	lines := strings.Split(strings.TrimSuffix(got.stderr, "\n"), "\n")
	want := []string{
		"Warning: said before the timeout",
		"Warning: the uploaded file F1 could not be deleted",
		"Warning: said by a lookup during the cleanup",
	}
	if len(lines) != 4 || !strings.Contains(lines[0], "command timed out after 40ms") || strings.Join(lines[1:], "|") != strings.Join(want, "|") {
		t.Errorf("stderr = %q, want the timeout then %q", got.stderr, want)
	}
}
