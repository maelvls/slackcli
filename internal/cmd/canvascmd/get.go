package canvascmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/maelvls/slackcli/internal/appinfo"
	"github.com/maelvls/slackcli/internal/clictx"
	"github.com/maelvls/slackcli/internal/cmd/cmdutil"
	"github.com/maelvls/slackcli/internal/slack/canvas"
)

func newGet(rt *clictx.Context) *cobra.Command {
	var workspace string
	var maxChars int
	cmd := &cobra.Command{
		Use:   "get <canvas>",
		Short: "Fetch a Slack canvas and convert it to Markdown",
		Long: `Fetch a Slack canvas and convert it to Markdown.

` + canvasArgHelp + `

Prints {"canvas": {"id", "title"?, "markdown"}}. When the Markdown is longer
than --max-chars it is cut and ends with "…"; the output then also has
"truncated": true and "total_chars" (the full length), and a hint in
"hints". The canvas is always fetched afresh.`,
		Example: fmt.Sprintf(`  %[1]s canvas get "https://acme.slack.com/docs/T12345678/F12345678"
  %[1]s canvas get F12345678 --workspace acme --max-chars -1`, appinfo.Name),
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			t, err := parseTarget(rt, args[0], workspace)
			if err != nil {
				return err
			}
			// 0 would print an empty canvas.
			if err := cmdutil.CheckMaxChars("--max-chars", maxChars); err != nil {
				return err
			}

			ctx := cmd.Context()
			payload, err := clictx.WithAutoRefresh(ctx, rt, func() (canvas.FetchResult, error) {
				r, err := rt.Client(ctx, t.workspace)
				if err != nil {
					return canvas.FetchResult{}, err
				}
				id, err := t.id(ctx, r.Client)
				if err != nil {
					return canvas.FetchResult{}, err
				}
				return canvas.Fetch(ctx, r.Client, id, maxChars)
			})
			if err != nil {
				return err
			}
			if c := payload.Canvas; c.Truncated {
				rt.ResultHintf("Canvas %s is %d characters long; only the first %d are shown. Pass --max-chars -1 for all of it.", c.ID, c.TotalChars, maxChars)
			}
			return rt.Print(payload)
		},
	}
	cmd.Flags().StringVar(&workspace, "workspace", "", canvasWorkspaceUsage)
	cmd.Flags().IntVar(&maxChars, "max-chars", canvas.DefaultMaxChars, "Maximum number of Markdown characters to include: a positive integer, or -1 for no limit")
	return cmd
}
