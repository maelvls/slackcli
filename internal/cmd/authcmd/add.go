package authcmd

import (
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/maelvls/slackcli/internal/appinfo"
	"github.com/maelvls/slackcli/internal/auth"
	"github.com/maelvls/slackcli/internal/clictx"
	"github.com/maelvls/slackcli/internal/slackapi"
)

func newAdd(rt *clictx.Context) *cobra.Command {
	var (
		workspaceURL, token, xoxc, xoxd string
		fromEnv                         bool
	)
	cmd := &cobra.Command{
		Use:   "add",
		Short: "Add credentials (standard token or browser xoxc/xoxd)",
		Long: fmt.Sprintf(`Save credentials for a workspace: either a standard token (xoxb-/xoxp-), or
a browser token (xoxc-) together with the browser's "d" cookie (xoxd-).

Prefer --from-env, which reads SLACK_TOKEN (and SLACK_COOKIE_D for an xoxc-
token) from the environment:

  SLACK_TOKEN=xoxb-… %[1]s auth add --workspace-url https://myteam.slack.com --from-env

--token, --xoxc and --xoxd put the secret on the command line, where other
processes on the machine and the shell history can see it.

--workspace-url must be an https Slack workspace URL (*.slack.com or
*.slack-gov.com): credentials are never saved for, or sent to, another host.
Credentials already saved for that workspace are replaced. The first
workspace saved becomes the default (see "auth set-default").

Secrets are saved in the macOS keychain, and elsewhere in the credentials
file, which only your user can read (mode 0600). On macOS, a secret that the
keychain refuses (locked, prompt denied or timed out) is kept in that file
too, in plain text, with a warning on stderr: the command still succeeds, and
"auth whoami" then shows "secrets_storage": "file" for the workspace.

A value that is not a Slack token (or cookie) is refused. Prints one line of
text, not JSON: what was saved, for which workspace, and which workspace is
now the default.`, appinfo.Name),
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			u, err := auth.NormalizeURL(workspaceURL)
			if err != nil {
				return fmt.Errorf("--workspace-url must be a workspace URL like https://myteam.slack.com (got %q): credentials are only saved for, and sent to, https Slack workspaces (*.slack.com, *.slack-gov.com)", workspaceURL)
			}
			onArgv := token != "" || xoxc != "" || xoxd != ""
			var a slackapi.Auth
			if fromEnv {
				if onArgv {
					return errors.New("--from-env cannot be combined with --token, --xoxc or --xoxd")
				}
				// The same reading, and the same checks, as when
				// SLACK_TOKEN is used directly.
				var set bool
				if a, set, err = clictx.EnvAuth(); !set {
					return errors.New("--from-env: SLACK_TOKEN is not set")
				} else if err != nil {
					return fmt.Errorf("--from-env: %w", err)
				}
			} else if a, err = authFromSecrets(strings.TrimSpace(token), strings.TrimSpace(xoxc), strings.TrimSpace(xoxd)); err != nil {
				return err
			}
			if err := rt.Store.Upsert(auth.Workspace{URL: u, Auth: a}); err != nil {
				return err
			}
			if onArgv {
				rt.Hintf("warning: secrets passed as flags are visible in the process list and the shell history; prefer --from-env")
			}
			what := "standard token"
			if a.IsBrowser() {
				what = "browser tokens"
			}
			_, err = fmt.Fprintf(rt.Stdout, "Saved %s for %s%s.\n", what, u, defaultAfterSaving(rt, u))
			return err
		},
	}
	flags := cmd.Flags()
	flags.StringVar(&workspaceURL, "workspace-url", "", "Workspace URL like https://myteam.slack.com (required)")
	flags.StringVar(&token, "token", "", "Standard Slack token (xoxb-/xoxp-); visible in the process list, prefer --from-env")
	flags.StringVar(&xoxc, "xoxc", "", "Browser token (xoxc-...), with --xoxd; visible in the process list, prefer --from-env")
	flags.StringVar(&xoxd, "xoxd", "", "Browser cookie d (xoxd-...), with --xoxc; visible in the process list, prefer --from-env")
	flags.BoolVar(&fromEnv, "from-env", false, "Read the token from SLACK_TOKEN (and the cookie from SLACK_COOKIE_D for an xoxc- token) instead of flags")
	_ = cmd.MarkFlagRequired("workspace-url")
	return cmd
}

// defaultAfterSaving says, to complete "Saved … for <url>", whether the
// workspace just saved is the default one, and which one is if not.
func defaultAfterSaving(rt *clictx.Context, saved string) string {
	creds, err := rt.Store.Load()
	if err != nil {
		return ""
	}
	switch def := creds.Default(); {
	case def == nil:
		return fmt.Sprintf(`; no default workspace is set: choose one with "%s auth set-default <workspace>"`, appinfo.Name)
	case def.URL == saved:
		return ", which is the default workspace"
	default:
		return "; the default workspace is still " + def.URL
	}
}

// authFromSecrets validates the secrets given to "auth add" as flags.
// Catching a value that is not a token, or a token passed under the wrong
// flag, here beats a puzzling invalid_auth later.
func authFromSecrets(token, xoxc, xoxd string) (slackapi.Auth, error) {
	var none slackapi.Auth
	switch {
	case token != "" && (xoxc != "" || xoxd != ""):
		return none, errors.New("provide either --token or both --xoxc and --xoxd, not all of them")
	case token != "":
		kind, err := clictx.CheckToken("--token", token)
		if err != nil {
			return none, err
		}
		if kind == slackapi.AuthBrowser {
			return none, errors.New("--token got a browser token (xoxc-): pass it with --xoxc, together with --xoxd")
		}
		return slackapi.Auth{Type: kind, Token: token}, nil
	case xoxc != "" && xoxd != "":
		if kind, err := clictx.CheckToken("--xoxc", xoxc); err != nil || kind != slackapi.AuthBrowser {
			return none, errors.New("--xoxc must be a browser token starting with xoxc-")
		}
		cookie, err := clictx.CheckCookieD("--xoxd", xoxd)
		if err != nil {
			return none, err
		}
		return slackapi.Auth{Type: slackapi.AuthBrowser, XoxcToken: xoxc, XoxdCookie: cookie}, nil
	}
	return none, errors.New("provide either --token or both --xoxc and --xoxd (or --from-env)")
}
