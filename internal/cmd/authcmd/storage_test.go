package authcmd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/maelvls/slackcli/internal/appinfo"
	"github.com/maelvls/slackcli/internal/auth"
	"github.com/maelvls/slackcli/internal/authextract"
	"github.com/maelvls/slackcli/internal/slackapi"
)

// refusingKeychain reads fine but takes no secret while refuse is set,
// like a keychain whose prompt was denied.
type refusingKeychain struct {
	fakeKeychain
	refuse *bool
}

func (k refusingKeychain) Set(service, account, value string) error {
	if *k.refuse {
		return errors.New("the keychain did not take the secret")
	}
	return k.fakeKeychain.Set(service, account, value)
}

// "auth whoami" says where the secrets are and, when the keychain did not
// take them, that they are in plain text in the file and which command
// moves them.
func TestWhoamiSecretsStorage(t *testing.T) {
	const bot, acme = "https://bot.slack.com", "https://acme.slack.com"
	rows := func(t *testing.T, h *harness) map[string]whoamiWorkspace {
		t.Helper()
		out := h.mustRun("", "whoami")
		// "secrets_storage" comes right after "secrets".
		if secrets, storage, hint := strings.Index(out, `"secrets":`), strings.Index(out, `"secrets_storage":`), strings.Index(out, `"hint":`); secrets < 0 || storage < secrets || (hint >= 0 && hint < storage) {
			t.Errorf("secrets_storage is missing or misplaced:\n%s", out)
		}
		byURL := map[string]whoamiWorkspace{}
		for _, w := range decodeJSON[whoamiOutput](t, out).Workspaces {
			byURL[w.WorkspaceURL] = w
		}
		return byURL
	}

	t.Run("keychain that refuses", func(t *testing.T) {
		refuse := true
		kc := refusingKeychain{fakeKeychain{}, &refuse}
		h := newHarness(t, kc)
		if err := h.rt.Store.Upsert(
			auth.Workspace{URL: bot, Auth: slackapi.Auth{Type: slackapi.AuthStandard, Token: "xoxb-bot-0123456789-secret"}},
			browserWorkspace(acme, "Acme", "xoxc-acme-0123456789-secret"),
		); err != nil {
			t.Fatal(err)
		}
		got := rows(t, h)
		for url, command := range map[string]string{
			bot:  `"SLACK_TOKEN=… ` + appinfo.Name + ` auth add --workspace-url ` + bot + ` --from-env"`,
			acme: `"` + appinfo.Name + ` auth import-desktop"`,
		} {
			w := got[url]
			if w.Secrets != "ok" || w.SecretsStorage != "file" || w.Token == "" {
				t.Errorf("%s = %+v, want usable secrets stored in the file", url, w)
			}
			for _, want := range []string{"in plain text in the credentials file", "because the keychain did not take them", appinfo.EnvName("KEYCHAIN_TIMEOUT_MS"), command} {
				if !strings.Contains(w.Hint, want) {
					t.Errorf("%s: hint %q does not contain %q", url, w.Hint, want)
				}
			}
			if strings.Contains(w.Hint, "0123456789") {
				t.Errorf("%s: the hint holds a secret: %q", url, w.Hint)
			}
		}

		// The keychain works again and credentials are saved again: the
		// secrets move, and the hint goes.
		refuse = false
		t.Setenv("SLACK_TOKEN", "xoxb-bot-0123456789-secret")
		h.mustRun("", "add", "--workspace-url", bot, "--from-env")
		t.Setenv("SLACK_TOKEN", "")
		for url, w := range rows(t, h) {
			if w.Secrets != "ok" || w.SecretsStorage != "keychain" || w.Hint != "" {
				t.Errorf("%s = %+v, want secrets in the keychain and no hint", url, w)
			}
		}
	})

	// Without a keychain (Linux, Windows) the file is where secrets
	// belong: it is said, without a hint.
	t.Run("no keychain", func(t *testing.T) {
		h := newHarness(t, nil)
		if err := h.rt.Store.Upsert(browserWorkspace(acme, "Acme", "xoxc-acme-0123456789-secret")); err != nil {
			t.Fatal(err)
		}
		if w := rows(t, h)[acme]; w.Secrets != "ok" || w.SecretsStorage != "file" || w.Hint != "" {
			t.Errorf("workspace = %+v", w)
		}
	})

	// Secrets that cannot be read are stored nowhere that can be told.
	t.Run("missing secrets", func(t *testing.T) {
		kc := fakeKeychain{}
		h := newHarness(t, kc)
		if err := h.rt.Store.Upsert(browserWorkspace(acme, "Acme", "xoxc-acme-0123456789-secret")); err != nil {
			t.Fatal(err)
		}
		delete(kc, "svc|xoxc:"+acme)
		h.rt.Store = &auth.Store{Path: h.rt.Store.Path, Keychain: kc, Service: "svc"}
		out := h.mustRun("", "whoami")
		if w := decodeJSON[whoamiOutput](t, out).Workspaces[0]; w.Secrets != "missing" || w.SecretsStorage != "" || strings.Contains(out, "secrets_storage") {
			t.Errorf("workspace = %+v\n%s", w, out)
		}
	})
}

func TestHelpDocumentsSecretsStorage(t *testing.T) {
	root := New(newHarness(t, nil).rt)
	for name, wants := range map[string][]string{
		"whoami":     {"secrets,\nsecrets_storage, hint}", `"keychain"`, `"file"`, "0600", "did not take"},
		"add":        {"keychain refuses", "mode 0600", "warning on stderr", `"secrets_storage": "file"`},
		"parse-curl": {"keychain refuses", "mode 0600", "warning on stderr", `"secrets_storage": "file"`},
	} {
		cmd, _, err := root.Find([]string{name})
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range wants {
			if !strings.Contains(cmd.Long, want) {
				t.Errorf("auth %s help does not contain %q", name, want)
			}
		}
	}
}

// An import also keeps its hands off a workspace that is not stored yet
// because its standard token is still in the predecessor's credentials,
// waiting to be migrated. The note must not send the user to "auth
// remove", which knows nothing of that workspace.
func TestImportNoteForAWorkspacePendingMigration(t *testing.T) {
	dir := t.TempDir()
	legacy := filepath.Join(dir, "legacy.json")
	if err := os.WriteFile(legacy, []byte(`{"version":1,"workspaces":[
	  {"workspace_url":"https://acme.slack.com","auth":{"auth_type":"standard","token":"__KEYCHAIN__"}}
	]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// The keychain cannot be read: the migration has to wait.
	kc := fakeKeychain{unavailableKey: "the keychain is locked"}
	h := newHarness(t, kc)
	h.rt.Store = &auth.Store{Path: filepath.Join(dir, "credentials.json"), Keychain: kc, Service: "svc", LegacyPath: legacy, LegacyService: "old"}
	h.rt.Desktop = func(context.Context) (*authextract.Extracted, error) { return extraction("Slack Desktop"), nil }

	out := h.mustRun("", "import-desktop")
	got := decodeJSON[struct {
		Imported   int                 `json:"imported"`
		Workspaces []importedWorkspace `json:"workspaces"`
	}](t, out)
	if got.Imported != 1 || len(got.Workspaces) != 1 || got.Workspaces[0].WorkspaceURL != "https://zeta.slack.com" {
		t.Errorf("import-desktop printed %s", out)
	}
	stderr := h.stderr.String()
	for _, want := range []string{
		"the browser session found in Slack Desktop was not imported for https://acme.slack.com",
		appinfo.LegacyName + " has a standard token for that workspace, which could not be migrated yet",
		"not configured until that migration goes through",
	} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr does not contain %q: %q", want, stderr)
		}
	}
	for _, wrong := range []string{"auth remove", `saved with "auth add"`} {
		if strings.Contains(stderr, wrong) {
			t.Errorf("stderr says %q about a workspace that is not stored: %q", wrong, stderr)
		}
	}
	creds, err := h.rt.Store.Load()
	if err != nil || creds.Find("https://acme.slack.com") != nil || creds.Find("https://zeta.slack.com") == nil {
		t.Errorf("stored workspaces = %+v, err = %v", creds, err)
	}
}
