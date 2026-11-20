package root

import (
	"errors"
	"testing"
	"time"

	"github.com/spf13/cobra"

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
