package auth

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/maelvls/slackcli/internal/appinfo"
)

// refusingKeychain is a keychain that reads fine but takes no secret while
// refuse is set, like one whose prompt was denied.
type refusingKeychain struct {
	fakeKeychain
	refuse *bool
}

const refusal = "the keychain did not take the secret (security add-generic-password returned status 36)"

func (k refusingKeychain) Set(service, account, value string) error {
	if *k.refuse {
		return errors.New(refusal)
	}
	return k.fakeKeychain.Set(service, account, value)
}

// A keychain that does not take a secret must not make the save fail (the
// user would be locked out), but the plain-text fallback is said, once per
// workspace, and shows in SecretsStorage. The next save that the keychain
// accepts moves the secrets out of the file.
func TestKeychainRefusalFallsBackToFileLoudly(t *testing.T) {
	refuse := true
	kc := refusingKeychain{fakeKeychain{}, &refuse}
	path := filepath.Join(t.TempDir(), "credentials.json")
	warnf, warnings := collectWarnings()
	s := &Store{Path: path, Keychain: kc, Service: "svc", Warnf: warnf}
	const a, b = "https://a.slack.com", "https://b.slack.com"
	secrets := []string{"xoxc-a-secret", "xoxd-a-secret", "xoxb-b-secret"}
	fileHolds := func(t *testing.T) (found []string) {
		t.Helper()
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, secret := range secrets {
			if strings.Contains(string(raw), secret) {
				found = append(found, secret)
			}
		}
		return found
	}
	storage := func(t *testing.T, s *Store, url string) SecretsStorage {
		t.Helper()
		creds, err := s.Load()
		if err != nil || creds.Find(url) == nil {
			t.Fatalf("%s not stored: %v", url, err)
		}
		return s.SecretsStorage(*creds.Find(url))
	}

	if err := s.Upsert(browserWS(a, "xoxc-a-secret", "xoxd-a-secret")); err != nil {
		t.Fatalf("a refused keychain made the save fail: %v", err)
	}
	if got := fileHolds(t); len(got) != 2 {
		t.Fatalf("the file holds %q, want both secrets of a", got)
	}
	wantMode(t, path, 0o600)
	// One warning for the two secrets of the workspace.
	if len(*warnings) != 1 {
		t.Fatalf("warnings = %q", *warnings)
	}
	for _, want := range []string{a, path, "plain text", refusal, appinfo.EnvName("KEYCHAIN_TIMEOUT_MS"), "auth import-desktop"} {
		if !strings.Contains((*warnings)[0], want) {
			t.Errorf("warning %q does not contain %q", (*warnings)[0], want)
		}
	}
	for _, secret := range secrets {
		if strings.Contains((*warnings)[0], secret) {
			t.Errorf("warning holds the secret %q: %q", secret, (*warnings)[0])
		}
	}
	if got := storage(t, s, a); got != SecretsInFile {
		t.Errorf("SecretsStorage(a) = %q, want %q", got, SecretsInFile)
	}

	// The next save tries a again, in vain: only b, which is new, is
	// reported.
	if err := s.Upsert(standardWS(b, "xoxb-b-secret")); err != nil {
		t.Fatal(err)
	}
	if len(*warnings) != 2 || strings.Contains((*warnings)[1], a) || !strings.Contains((*warnings)[1], b) {
		t.Fatalf("warnings = %q", *warnings)
	}
	fresh := &Store{Path: path, Keychain: kc, Service: "svc", Warnf: warnf}
	for _, url := range []string{a, b} {
		if got := storage(t, fresh, url); got != SecretsInFile {
			t.Errorf("SecretsStorage(%s) = %q in a new process, want %q", url, got, SecretsInFile)
		}
	}
	creds, _ := fresh.Load()
	if got, err := fresh.Resolve(*creds.Find(a)); err != nil || got.XoxcToken != "xoxc-a-secret" || got.XoxdCookie != "xoxd-a-secret" {
		t.Fatalf("resolve from the file = %+v, %v", got, err)
	}
	// Resolved credentials do not change the answer.
	resolved := *creds.Find(a)
	resolved.Auth.XoxcToken, resolved.Auth.XoxdCookie = keychainPlaceholder, keychainPlaceholder
	if got := fresh.SecretsStorage(resolved); got != SecretsInFile {
		t.Errorf("SecretsStorage of a modified copy = %q, want %q", got, SecretsInFile)
	}

	// The keychain works again: any save moves the secrets into it and
	// leaves placeholders in the file.
	refuse = false
	if err := fresh.SetDefault(b); err != nil {
		t.Fatal(err)
	}
	if got := fileHolds(t); len(got) != 0 {
		t.Fatalf("the file still holds %q", got)
	}
	if raw, _ := os.ReadFile(path); strings.Count(string(raw), keychainPlaceholder) != 3 {
		t.Fatalf("want three placeholders in the file:\n%s", raw)
	}
	if kc.fakeKeychain["svc|xoxc:"+a] != "xoxc-a-secret" || kc.fakeKeychain["svc|xoxd:"+a] != "xoxd-a-secret" || kc.fakeKeychain["svc|token:"+b] != "xoxb-b-secret" {
		t.Fatalf("keychain = %v", kc.fakeKeychain)
	}
	if len(*warnings) != 2 {
		t.Errorf("warnings = %q", *warnings)
	}
	for _, store := range []*Store{fresh, {Path: path, Keychain: kc, Service: "svc"}} {
		for _, url := range []string{a, b} {
			if got := storage(t, store, url); got != SecretsInKeychain {
				t.Errorf("SecretsStorage(%s) = %q, want %q", url, got, SecretsInKeychain)
			}
		}
		creds, _ := store.Load()
		if got, err := store.Resolve(*creds.Find(a)); err != nil || got.XoxcToken != "xoxc-a-secret" || got.XoxdCookie != "xoxd-a-secret" {
			t.Fatalf("resolve from the keychain = %+v, %v", got, err)
		}
		if got, err := store.Resolve(*creds.Find(b)); err != nil || got.Token != "xoxb-b-secret" {
			t.Fatalf("resolve from the keychain = %+v, %v", got, err)
		}
	}
}

// One secret in the file is enough for the workspace to count as stored in
// the file.
func TestSecretsStorageOfPartialKeychainSave(t *testing.T) {
	kc := xoxcRefusingKeychain{fakeKeychain{}}
	warnf, warnings := collectWarnings()
	s := &Store{Path: filepath.Join(t.TempDir(), "credentials.json"), Keychain: kc, Service: "svc", Warnf: warnf}
	if err := s.Upsert(browserWS("https://a.slack.com", "xoxc-a", "xoxd-a"), standardWS("https://bot.slack.com", "xoxb-bot")); err != nil {
		t.Fatal(err)
	}
	creds, _ := s.Load()
	if got := s.SecretsStorage(*creds.Find("https://a.slack.com")); got != SecretsInFile {
		t.Errorf("SecretsStorage(a) = %q", got)
	}
	if got := s.SecretsStorage(*creds.Find("https://bot.slack.com")); got != SecretsInKeychain {
		t.Errorf("SecretsStorage(bot) = %q", got)
	}
	if len(*warnings) != 1 || !strings.Contains((*warnings)[0], "https://a.slack.com") || strings.Contains((*warnings)[0], "https://bot.slack.com") {
		t.Errorf("warnings = %q", *warnings)
	}
}

// Without a keychain the file is where secrets belong: nothing to warn
// about.
func TestNoKeychainIsNotAFallback(t *testing.T) {
	warnf, warnings := collectWarnings()
	s := &Store{Path: filepath.Join(t.TempDir(), "credentials.json"), Warnf: warnf}
	if err := s.Upsert(browserWS("https://a.slack.com", "xoxc-a", "xoxd-a")); err != nil {
		t.Fatal(err)
	}
	creds, _ := s.Load()
	if got := s.SecretsStorage(creds.Workspaces[0]); got != SecretsInFile {
		t.Errorf("SecretsStorage = %q", got)
	}
	if len(*warnings) != 0 {
		t.Errorf("warnings = %q", *warnings)
	}
}

// A predecessor's file that cannot be read may hold workspaces: not
// importing them is said, once, and the migration is not marked as done.
func TestUnreadableLegacyFileIsReported(t *testing.T) {
	dir := t.TempDir()
	legacy, kc := legacyFixture(t, dir)
	valid, err := os.ReadFile(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacy, []byte("{nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	warnf, warnings := collectWarnings()
	newStore := func() *Store {
		return &Store{Path: filepath.Join(dir, "new", "credentials.json"), Keychain: kc, Service: "new", LegacyPath: legacy, LegacyService: "old", Warnf: warnf}
	}

	s := newStore()
	if creds, err := s.Load(); err != nil || len(creds.Workspaces) != 0 {
		t.Fatalf("creds = %+v, err = %v", creds, err)
	}
	if err := s.Upsert(browserWS("https://c.slack.com", "xoxc-c", "xoxd-c")); err != nil {
		t.Fatal(err)
	}
	if len(*warnings) != 1 {
		t.Fatalf("warnings = %q", *warnings)
	}
	for _, want := range []string{legacy, appinfo.LegacyName, "not valid JSON", "not imported", "auth import-desktop"} {
		if !strings.Contains((*warnings)[0], want) {
			t.Errorf("warning %q does not contain %q", (*warnings)[0], want)
		}
	}
	if raw, _ := os.ReadFile(legacy); string(raw) != "{nope" {
		t.Errorf("the predecessor's file was modified: %q", raw)
	}

	// Once the file is readable its workspaces are imported, although the
	// credentials file was written in between.
	if err := os.WriteFile(legacy, valid, 0o600); err != nil {
		t.Fatal(err)
	}
	creds, err := newStore().Load()
	if err != nil || workspaceURLs(creds) != "c.slack.com a.slack.com b.slack.com bot.slack.com" {
		t.Fatalf("workspaces = %s, err = %v", workspaceURLs(creds), err)
	}
	if len(*warnings) != 1 {
		t.Errorf("warnings = %q", *warnings)
	}
}

// No predecessor at all is the normal case: nothing is said.
func TestMissingLegacyFileIsSilent(t *testing.T) {
	dir := t.TempDir()
	warnf, warnings := collectWarnings()
	s := &Store{Path: filepath.Join(dir, "credentials.json"), LegacyPath: filepath.Join(dir, "none", "credentials.json"), LegacyService: "old", Warnf: warnf}
	if _, err := s.Load(); err != nil {
		t.Fatal(err)
	}
	if err := s.Upsert(browserWS("https://a.slack.com", "xoxc-a", "xoxd-a")); err != nil {
		t.Fatal(err)
	}
	if len(*warnings) != 0 {
		t.Errorf("warnings = %q", *warnings)
	}
}
