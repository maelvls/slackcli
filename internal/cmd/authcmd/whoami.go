package authcmd

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/maelvls/slackcli/internal/appinfo"
	"github.com/maelvls/slackcli/internal/auth"
	"github.com/maelvls/slackcli/internal/clictx"
	"github.com/maelvls/slackcli/internal/cmd/cmdutil"
)

// redactSecret keeps just enough of a secret to recognise it: its first 6
// and last 4 characters. Short values are hidden entirely.
func redactSecret(value string) string {
	const keepStart, keepEnd = 6, 4
	if value == "" {
		return ""
	}
	runes := []rune(value)
	if len(runes) <= keepStart+keepEnd+3 {
		return "[redacted]"
	}
	return string(runes[:keepStart]) + "…" + string(runes[len(runes)-keepEnd:])
}

type whoamiOutput struct {
	Version             int               `json:"version"`
	UpdatedAt           string            `json:"updated_at"`
	DefaultWorkspaceURL string            `json:"default_workspace_url"`
	Workspaces          []whoamiWorkspace `json:"workspaces"`
	Environment         *whoamiEnv        `json:"environment,omitempty"`
	// Hint says what to do when several workspaces are configured and
	// none is the default.
	Hint string `json:"hint,omitempty"`
}

type whoamiWorkspace struct {
	WorkspaceURL  string `json:"workspace_url"`
	WorkspaceName string `json:"workspace_name"`
	AuthType      string `json:"auth_type"`
	// Token and CookieD are redacted.
	Token   string `json:"token"`
	CookieD string `json:"cookie_d"`
	// Default marks the workspace used when none is selected.
	Default bool `json:"default,omitempty"`
	// Secrets is "ok", "missing" when the credentials file references
	// secrets that are no longer in the keychain, or "unavailable" when
	// the keychain could not be read (locked, timed out).
	Secrets string `json:"secrets"`
	// SecretsStorage is "keychain" or "file" (auth.SecretsStorage): where
	// the secrets are kept. It is set when they could be read ("ok").
	SecretsStorage string `json:"secrets_storage"`
	Hint           string `json:"hint,omitempty"`
}

// whoamiEnv describes credentials taken from the environment, which take
// precedence over every stored workspace.
type whoamiEnv struct {
	Note         string `json:"note"`
	AuthType     string `json:"auth_type"`
	Token        string `json:"token"`
	CookieD      string `json:"cookie_d"`
	WorkspaceURL string `json:"workspace_url"`
	// Error says why SLACK_TOKEN (or SLACK_COOKIE_D) cannot be used;
	// auth_type, token and cookie_d are then absent.
	Error string `json:"error"`
}

func newWhoami(rt *clictx.Context) *cobra.Command {
	return &cobra.Command{
		Use:   "whoami",
		Short: "Show configured workspaces and token sources",
		Long: `Show the configured workspaces, which one is the default, and where
credentials come from. Tokens and cookies are redacted.

This reads local configuration only; use "auth test" to check that the
credentials still work. A workspace with "secrets": "missing" has lost its
keychain entries and must be imported again; "unavailable" means that the
keychain could not be read just now (locked, or a prompt timed out).

"secrets_storage", present when "secrets" is "ok", says where the secrets
are: "keychain" (macOS), or "file" for the credentials file, which only your
user can read (mode 0600). "file" is the rule where there is no keychain
(Linux, Windows). On macOS it means that the keychain did not take the
secrets when they were saved: they work from the file, and "hint" says how
to move them into the keychain.

Prints {version, updated_at, default_workspace_url, workspaces: [{
workspace_url, workspace_name, auth_type, token, cookie_d, default, secrets,
secrets_storage, hint}], environment, hint}. "workspaces" is [] when none is
configured (a hint on stderr then says how to add credentials). "environment"
is present when SLACK_TOKEN is set; its "error" says so when that token
cannot be used.

"default_workspace_url" is empty when several workspaces are configured and
none was chosen: commands then need --workspace (or SLACK_WORKSPACE_URL)
until "auth set-default" is run; "hint" says so.`,
		Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			creds, err := rt.Store.Load()
			if err != nil {
				return err
			}
			out := whoamiOutput{
				Version:             creds.Version,
				UpdatedAt:           creds.UpdatedAt,
				DefaultWorkspaceURL: creds.DefaultWorkspaceURL,
				Environment:         environmentAuth(),
				Hint:                noDefaultAdvice(rt),
			}
			def := creds.Default()
			for _, w := range creds.Workspaces {
				row := whoamiWorkspace{
					WorkspaceURL:  w.URL,
					WorkspaceName: w.Name,
					AuthType:      string(w.Auth.Type),
					Default:       def != nil && def.URL == w.URL,
					Secrets:       "ok",
				}
				// The file may only hold keychain placeholders: redact
				// the real values, never a placeholder.
				resolved, err := rt.Store.Resolve(w)
				var unusable *auth.SecretsError
				switch {
				case err == nil:
					row.Token = redactSecret(resolved.BearerToken())
					row.CookieD = redactSecret(resolved.XoxdCookie)
					storage := rt.Store.SecretsStorage(w)
					row.SecretsStorage = string(storage)
					// Without a keychain the file is where secrets
					// belong: nothing to say.
					if storage == auth.SecretsInFile && rt.Store.Keychain != nil {
						row.Hint = plaintextAdvice(w)
					}
				case errors.As(err, &unusable) && !unusable.Missing():
					row.Secrets = "unavailable"
					row.Hint = fmt.Sprintf(`the keychain could not be read (%v): unlock it or raise %s, then try again; the workspace itself is still configured`,
						unusable.Cause, appinfo.EnvName("KEYCHAIN_TIMEOUT_MS"))
				default:
					row.Secrets = "missing"
					row.Hint = fmt.Sprintf(`the secrets of this workspace are not in the keychain any more: import them again (e.g. "%s auth import-desktop")`, appinfo.Name)
				}
				out.Workspaces = append(out.Workspaces, row)
			}
			if len(out.Workspaces) == 0 && out.Environment == nil {
				rt.Hintf(`No Slack credentials are configured: run "%[1]s auth import-desktop" (also: import-chrome, import-brave, import-firefox), save a token with "%[1]s auth add", or set SLACK_TOKEN.`, appinfo.Name)
			}
			// "workspaces" is printed even when empty.
			return cmdutil.PrintList(rt, out, "workspaces")
		},
	}
}

// plaintextAdvice says, for a workspace whose secrets the keychain did not
// take, where they are and which command saves them again: any save moves
// the secrets of every workspace into a keychain that works.
func plaintextAdvice(w auth.Workspace) string {
	again := fmt.Sprintf(`"%s auth import-desktop" (or the import-* command of the browser they came from)`, appinfo.Name)
	if !w.Auth.IsBrowser() {
		again = fmt.Sprintf(`"SLACK_TOKEN=… %s auth add --workspace-url %s --from-env"`, appinfo.Name, w.URL)
	}
	return fmt.Sprintf(`the secrets of this workspace are in plain text in the credentials file (readable by your user only) because the keychain did not take them when they were saved; they work from there. To move them into the keychain, make it usable (unlock it, answer its prompt, or raise %s), then save the credentials again with %s`,
		appinfo.EnvName("KEYCHAIN_TIMEOUT_MS"), again)
}

// environmentAuth describes the credentials of SLACK_TOKEN, as the
// commands will read them (clictx.EnvAuth); nil when it is not set.
func environmentAuth() *whoamiEnv {
	a, set, err := clictx.EnvAuth()
	if !set {
		return nil
	}
	env := &whoamiEnv{WorkspaceURL: strings.TrimSpace(os.Getenv("SLACK_WORKSPACE_URL"))}
	if err != nil {
		// Nothing of a value that is not a token is shown, not even
		// redacted: it may be another secret, pasted by mistake.
		env.Note = "SLACK_TOKEN is set but cannot be used: commands fail until it is fixed or unset"
		env.Error = err.Error()
		return env
	}
	env.Note = "SLACK_TOKEN is set: it is used instead of the stored workspaces"
	env.AuthType = string(a.Type)
	env.Token = redactSecret(a.BearerToken())
	env.CookieD = redactSecret(a.XoxdCookie)
	return env
}
