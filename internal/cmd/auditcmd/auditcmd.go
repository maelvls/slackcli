// Package auditcmd implements the "audit" command: reading the audit log
// of the requests made to Slack (see package audit).
package auditcmd

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/maelvls/slackcli/internal/appinfo"
	"github.com/maelvls/slackcli/internal/audit"
	"github.com/maelvls/slackcli/internal/clictx"
	"github.com/maelvls/slackcli/internal/cmd/cmdutil"
)

// now is the clock of "audit stats" (tests).
var now = time.Now

// New returns the "audit" command.
func New(rt *clictx.Context) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "audit",
		Short: "Audit log of the requests made to Slack",
		Long: fmt.Sprintf(`Every request %[1]s makes to Slack is one JSON line of the audit log:
each attempt of an API call (rate-limited ones and retries included), each
file download or upload request, each opening of the real-time WebSocket.
A line has the time (ts), the pid and ppid of %[1]s, the names of its
parent processes (parent, ancestors, on macOS and Linux), the command
(cmd) and its arguments (args), the host, the method, the HTTP status,
Slack's ok and error code, duration_ms, attempt, rate_limited and
retry_after_s.

The log never holds a token, a cookie, a message or a search query: of
the arguments it keeps the names of the flags, and the Slack IDs,
timestamps, numbers and Slack URLs; every other argument is "…".

The log is ~/.config/%[1]s/%[2]s, next to credentials.json
($XDG_CONFIG_HOME/%[1]s when that is set); it is moved to %[2]s.1
once it reaches 20 MB. %[3]s=<path> writes it elsewhere, and
%[3]s=off turns it off.`, appinfo.Name, audit.FileName, appinfo.EnvName(audit.EnvSuffix)),
		Args: cobra.NoArgs,
	}
	cmd.AddCommand(newPathCommand(rt), newTailCommand(rt), newStatsCommand(rt))
	return cmd
}

func newPathCommand(rt *clictx.Context) *cobra.Command {
	return &cobra.Command{
		Use:   "path",
		Short: "Print where the audit log is",
		Long: fmt.Sprintf(`Print {"path": …, "enabled": …}: the file the audit log is written
to, and whether it is written (it is not when %s is
off).`, appinfo.EnvName(audit.EnvSuffix)),
		Example: fmt.Sprintf(`  tail -f "$(%s audit path | jq -r .path)"`, appinfo.Name),
		Args:    cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			path, enabled := audit.Path()
			return rt.Print(struct {
				Path    string `json:"path"`
				Enabled bool   `json:"enabled"`
			}{path, enabled})
		},
	}
}

func newTailCommand(rt *clictx.Context) *cobra.Command {
	var n int
	cmd := &cobra.Command{
		Use:   "tail",
		Short: "Print the last lines of the audit log",
		Long: `Print the last lines of the audit log as they are in the file: one
JSON object per line, oldest first. Nothing is printed when the log is
empty or does not exist.`,
		Example: fmt.Sprintf(`  %s audit tail -n 50 | jq -c 'select(.rate_limited)'`, appinfo.Name),
		Args:    cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			if n < 1 {
				return fmt.Errorf("invalid --lines value %d: must be a positive integer", n)
			}
			path, _ := audit.Path()
			lines, err := audit.Tail(path, n)
			if err != nil {
				return err
			}
			if len(lines) == 0 {
				return nil
			}
			_, err = fmt.Fprintln(rt.Stdout, strings.Join(lines, "\n"))
			return err
		},
	}
	cmd.Flags().IntVarP(&n, "lines", "n", 20, "How many lines to print")
	return cmd
}

// statsResult is what "audit stats" prints.
type statsResult struct {
	Path string `json:"path"`
	// Since is the start of the period looked at.
	Since string `json:"since"`
	audit.Stats
	// Skipped counts the lines that could not be read.
	Skipped int `json:"skipped,omitempty"`
}

func newStatsCommand(rt *clictx.Context) *cobra.Command {
	var since string
	cmd := &cobra.Command{
		Use:   "stats",
		Short: "Sum up the requests made to Slack",
		Long: `Sum up the requests of the audit log made in the last --since (24h by
default; e.g. 1h, 90m, 7d).

Prints {"path", "since", "from", "to", "total", "processes", "errors",
"rate_limited", "peak_minute", "by_method", "by_source", "by_error",
"by_host", "by_hour"}. processes is how many slackcli processes made the
requests; peak_minute the most requests made in any 60 seconds, and when
that minute started. by_method, by_error and by_host count the requests,
and those that failed, per key; by_source per parent process and command,
with how many processes that was; all sorted by count, highest first.
by_hour counts the requests per hour (UTC). Lines that could not be read
are counted in "skipped".`,
		Example: fmt.Sprintf(`  %s audit stats --since 1h`, appinfo.Name),
		Args:    cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			d, err := audit.ParseSince(since)
			if err != nil {
				return fmt.Errorf("invalid --since value: %w", err)
			}
			start := now().Add(-d).UTC()
			path, _ := audit.Path()
			entries, skipped, err := audit.Read(path, start)
			if err != nil {
				return err
			}
			return cmdutil.PrintList(rt, statsResult{
				Path:    path,
				Since:   start.Format(time.RFC3339),
				Stats:   audit.Summarize(entries),
				Skipped: skipped,
			}, "by_method", "by_source", "by_error", "by_host", "by_hour")
		},
	}
	cmd.Flags().StringVar(&since, "since", "24h", "How far back to look: a duration such as 1h, 24h or 7d")
	return cmd
}
