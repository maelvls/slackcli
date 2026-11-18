// Package auth persists Slack credentials: workspace metadata in
// ~/.config/slackcli/credentials.json (0600) and, on macOS, the secrets in
// the login keychain.
package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/maelvls/slackcli/internal/appinfo"
	"github.com/maelvls/slackcli/internal/slackapi"
)

// keychainPlaceholder replaces a secret in the credentials file when the
// real value lives in the keychain.
const keychainPlaceholder = "__KEYCHAIN__"

// Workspace is one configured Slack workspace.
type Workspace struct {
	URL        string        `json:"workspace_url"`
	Name       string        `json:"workspace_name,omitempty"`
	TeamID     string        `json:"team_id,omitempty"`
	TeamDomain string        `json:"team_domain,omitempty"`
	Auth       slackapi.Auth `json:"auth"`
}

// Credentials is the content of the credentials file. Secrets in
// Workspaces[i].Auth may be keychain placeholders: use Store.Resolve to get
// usable credentials.
type Credentials struct {
	Version             int    `json:"version"`
	UpdatedAt           string `json:"updated_at,omitempty"`
	DefaultWorkspaceURL string `json:"default_workspace_url,omitempty"`
	// LegacyMigrated records that the predecessor's credentials were
	// imported (see Store.LegacyPath), so that they are imported once only.
	// A file that exists does not say as much: it may have been written
	// while the predecessor's secrets could not be read.
	LegacyMigrated bool        `json:"legacy_migrated,omitempty"`
	Workspaces     []Workspace `json:"workspaces"`
}

// Find returns the workspace with the given (normalized) URL.
func (c *Credentials) Find(workspaceURL string) *Workspace {
	for i := range c.Workspaces {
		if c.Workspaces[i].URL == workspaceURL {
			return &c.Workspaces[i]
		}
	}
	return nil
}

// Default returns the workspace to use when none is selected: the
// configured default, else the only workspace there is. It is nil when
// there is no workspace, and when there are several without a default:
// picking one of them would be a guess (see Store.Upsert for when a default
// is set).
func (c *Credentials) Default() *Workspace {
	if c.DefaultWorkspaceURL != "" {
		if w := c.Find(c.DefaultWorkspaceURL); w != nil {
			return w
		}
	}
	if len(c.Workspaces) == 1 {
		return &c.Workspaces[0]
	}
	return nil
}

// Store reads and writes credentials.
type Store struct {
	// Path is the credentials file.
	Path string
	// Keychain holds the secrets; nil means secrets stay in the file. A
	// secret that the keychain does not take is kept in the file too, with
	// a warning (see SecretsStorage).
	Keychain Keychain
	// Service is the keychain service name.
	Service string
	// LegacyPath and LegacyService locate the predecessor's (agent-slack)
	// credentials, imported once (see Credentials.LegacyMigrated). Empty
	// disables the migration.
	LegacyPath    string
	LegacyService string
	// Warnf reports a problem that does not stop the command (a corrupt
	// credentials file, a migration that has to wait, secrets that the
	// keychain did not take). Nil discards the warning.
	Warnf func(format string, args ...any)

	mu       sync.Mutex
	loaded   *Credentials
	resolved map[string]slackapi.Auth
	// legacyUnmigrated holds the predecessor's workspaces that the last
	// reload could not import (their secrets are placeholders).
	legacyUnmigrated []Workspace
	warnedCorrupt    bool
	warnedMigration  bool
	warnedLegacy     bool
	// warnedPlaintext holds the workspaces for which the keychain fallback
	// was reported already.
	warnedPlaintext map[string]bool
}

// DefaultStore is the store used by the CLI.
func DefaultStore() *Store {
	return &Store{
		Path:          filepath.Join(appinfo.ConfigDir(), "credentials.json"),
		Keychain:      SystemKeychain(),
		Service:       appinfo.Name,
		LegacyPath:    filepath.Join(appinfo.LegacyConfigDir(), "credentials.json"),
		LegacyService: appinfo.LegacyName,
	}
}

// NormalizeURL reduces a workspace URL to its origin
// ("https://acme.slack.com"). A bare host is accepted.
//
// Credentials are sent to that origin, so it must be an https URL on a
// Slack workspace host (slack.com, slack-gov.com and their subdomains; see
// slackapi.WorkspaceOrigin): anything else is an error.
func NormalizeURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if !strings.Contains(raw, "://") && strings.Contains(raw, ".") {
		raw = "https://" + raw
	}
	return slackapi.WorkspaceOrigin(raw)
}

// SecretsError says that the secrets of a stored workspace cannot be used.
type SecretsError struct {
	// WorkspaceURL is the workspace whose secrets were asked for.
	WorkspaceURL string
	// Cause is why the keychain could not be read (locked, timed out…).
	// It is nil when the keychain answered and the secrets are not there.
	Cause error
}

// Missing reports whether the secrets are gone (as opposed to temporarily
// unreadable), in which case only a new import brings them back.
func (e *SecretsError) Missing() bool { return e.Cause == nil }

func (e *SecretsError) Error() string {
	if e.Missing() {
		return fmt.Sprintf("the secrets of %s are not stored any more (deleted from the keychain?)", e.WorkspaceURL)
	}
	return fmt.Sprintf("the secrets of %s are unavailable: %v", e.WorkspaceURL, e.Cause)
}

func (e *SecretsError) Unwrap() error { return e.Cause }

// Load returns the stored credentials (never nil). A missing file yields an
// empty set, and so does a corrupted one, which is preserved (see
// preserveCorrupt); invalid workspaces are skipped. The result is cached
// for the lifetime of the Store and must not be mutated: use the Store's
// mutating methods instead.
func (s *Store) Load() (*Credentials, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.load()
}

func (s *Store) load() (*Credentials, error) {
	if s.loaded != nil {
		return s.loaded, nil
	}
	creds, err := s.readFileOrEmpty()
	if err != nil {
		return nil, err
	}
	if pending, _ := s.legacyPending(creds); len(pending) > 0 {
		// The migration writes the file: do it under the lock. A command
		// that only reads does not fail when the lock cannot be taken (a
		// directory that is not writable): it goes without the migration.
		unlock, lockErr := lockFile(s.Path)
		if lockErr == nil {
			defer unlock()
			return s.reload()
		}
		s.warnMigration(lockErr)
	}
	s.loaded = creds
	return creds, nil
}

// reload reads the credentials file again, ignoring the cache, and
// migrates the predecessor's credentials if that is still to be done. A
// migration that fails is reported as a warning and left for a later run.
// The caller holds the file lock.
func (s *Store) reload() (*Credentials, error) {
	creds, err := s.readFileOrEmpty()
	if err != nil {
		return nil, err
	}
	s.loaded = creds
	s.legacyUnmigrated = nil
	if pending, defaultURL := s.legacyPending(creds); len(pending) > 0 {
		if err := s.migrateLegacy(creds, pending, defaultURL); err != nil {
			s.legacyUnmigrated = pending
			s.warnMigration(err)
		}
	}
	return s.loaded, nil
}

// readFileOrEmpty is readFile, with a missing file read as an empty set.
func (s *Store) readFileOrEmpty() (*Credentials, error) {
	creds, err := s.readFile()
	if errors.Is(err, fs.ErrNotExist) {
		return &Credentials{Version: 1}, nil
	}
	return creds, err
}

// warnMigration says once that the predecessor's credentials are still to
// be imported.
func (s *Store) warnMigration(cause error) {
	if s.warnedMigration || s.Warnf == nil {
		return
	}
	s.warnedMigration = true
	s.Warnf("warning: the workspaces stored in %s could not be imported yet (%v): they are left there, and the next run tries again",
		s.LegacyPath, cause)
}

// warnLegacyUnreadable says once that the predecessor's credentials file
// cannot be read, so that workspaces do not silently fail to migrate. The
// migration stays to be done: a later run reads the file again.
func (s *Store) warnLegacyUnreadable(cause error) {
	if s.warnedLegacy || s.Warnf == nil {
		return
	}
	s.warnedLegacy = true
	s.Warnf(`warning: the credentials file of %s, %s, cannot be read (%v), so its workspaces were not imported: repair or delete that file, or import credentials again (e.g. "%s auth import-desktop")`,
		appinfo.LegacyName, s.LegacyPath, cause, appinfo.Name)
}

// update is one read-modify-write cycle on the credentials file. It holds
// the cross-process lock and starts from the file as it is now, not from
// what this Store loaded earlier, so that concurrent invocations do not
// overwrite each other's changes.
func (s *Store) update(change func(next *Credentials) error) error {
	unlock, err := lockFile(s.Path)
	if err != nil {
		return err
	}
	defer unlock()
	cur, err := s.reload()
	if err != nil {
		return err
	}
	next := *cur
	next.Workspaces = append([]Workspace(nil), cur.Workspaces...)
	if err := change(&next); err != nil {
		return err
	}
	return s.save(&next)
}

// errCorrupt marks a credentials file that is not valid JSON.
var errCorrupt = errors.New("not valid JSON")

// readFile reads the credentials file. A corrupt file is preserved and
// read as empty: it must not brick the CLI, since credentials can be
// imported again.
func (s *Store) readFile() (*Credentials, error) {
	creds, err := readCredentialsFile(s.Path)
	if errors.Is(err, errCorrupt) {
		s.preserveCorrupt(err)
		return &Credentials{Version: 1}, nil
	}
	return creds, err
}

// preserveCorrupt keeps a 0600 copy of a corrupt credentials file next to
// it ("credentials.json.corrupt-<time of last change>") before the next
// save replaces it, and warns once. The copy is named after the file's
// modification time so that repeated runs do not pile up copies.
func (s *Store) preserveCorrupt(cause error) {
	if s.warnedCorrupt {
		return
	}
	s.warnedCorrupt = true
	kept := "it could not be copied"
	if fi, err := os.Stat(s.Path); err == nil {
		backup := s.Path + ".corrupt-" + fi.ModTime().UTC().Format("20060102T150405Z")
		if copyFileExclusive(s.Path, backup) == nil {
			kept = "a copy was kept at " + backup
		}
	}
	if s.Warnf != nil {
		s.Warnf(`warning: the credentials file %s is unreadable (%v), so no stored workspace is available; %s. Import credentials again (e.g. "%s auth import-desktop") or repair the file`,
			s.Path, cause, kept, appinfo.Name)
	}
}

// copyFileExclusive copies src to dst (0600). An existing dst is left
// alone: it is an earlier copy of the same file. A copy that fails half-way
// is removed, since the next run would take it for a complete one.
func copyFileExclusive(src, dst string) error {
	raw, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := writeAndSync(f, raw); err != nil {
		_ = os.Remove(dst)
		return err
	}
	return nil
}

func readCredentialsFile(path string) (*Credentials, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		return nil, fmt.Errorf("reading credentials: %w", err)
	}
	var parsed Credentials
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("%w: %v", errCorrupt, err)
	}
	out := &Credentials{Version: 1, UpdatedAt: parsed.UpdatedAt, LegacyMigrated: parsed.LegacyMigrated}
	for _, w := range parsed.Workspaces {
		// A workspace whose URL is not a Slack origin is dropped: its
		// credentials must never be sent there.
		u, err := NormalizeURL(w.URL)
		if err != nil {
			continue
		}
		if w.Auth.Type != slackapi.AuthStandard && w.Auth.Type != slackapi.AuthBrowser {
			continue
		}
		w.URL = u
		if out.Find(u) == nil {
			out.Workspaces = append(out.Workspaces, w)
		}
	}
	if u, err := NormalizeURL(parsed.DefaultWorkspaceURL); err == nil && out.Find(u) != nil {
		out.DefaultWorkspaceURL = u
	}
	return out, nil
}

// legacyPending returns the predecessor's workspaces that are still to be
// imported into cur, and the predecessor's default workspace. There are
// none once the migration was done, and a workspace that cur has already
// is not one: what was saved since is newer.
func (s *Store) legacyPending(cur *Credentials) (pending []Workspace, defaultURL string) {
	if cur.LegacyMigrated || s.LegacyPath == "" || s.LegacyPath == s.Path {
		return nil, ""
	}
	legacy, err := readCredentialsFile(s.LegacyPath)
	if err != nil {
		// No file: nothing to import. One that cannot be read may hold
		// workspaces that are not imported: say so.
		if !errors.Is(err, fs.ErrNotExist) {
			s.warnLegacyUnreadable(err)
		}
		return nil, ""
	}
	for _, w := range legacy.Workspaces {
		if cur.Find(w.URL) == nil {
			pending = append(pending, w)
		}
	}
	return pending, legacy.DefaultWorkspaceURL
}

// migrateLegacy adds the predecessor's workspaces (file + keychain service)
// to cur, marks the migration as done and saves. Workspaces whose secrets
// are gone are left behind; if the keychain cannot be read at all, nothing
// is migrated and the migration is not marked as done, so that a later run
// tries again instead of dropping workspaces for good, whatever was saved
// in between.
func (s *Store) migrateLegacy(cur *Credentials, pending []Workspace, defaultURL string) error {
	next := *cur
	next.Workspaces = append([]Workspace(nil), cur.Workspaces...)
	for _, w := range pending {
		a, err := resolveAuth(s.Keychain, s.LegacyService, w)
		var se *SecretsError
		if errors.As(err, &se) && !se.Missing() {
			return err
		}
		if err != nil {
			continue
		}
		w.Auth = a
		next.Workspaces = append(next.Workspaces, w)
	}
	if len(cur.Workspaces) == 0 && next.Find(defaultURL) != nil {
		next.DefaultWorkspaceURL = defaultURL
	}
	next.LegacyMigrated = true
	return s.save(&next)
}

// unmigratedStandard reports whether the predecessor's credentials, which
// could not be imported yet, hold a standard token for the workspace.
func (s *Store) unmigratedStandard(workspaceURL string) bool {
	for _, w := range s.legacyUnmigrated {
		if w.URL == workspaceURL {
			return w.Auth.Type == slackapi.AuthStandard
		}
	}
	return false
}

func isSecret(v string) bool { return v != "" && v != keychainPlaceholder }

// storedSecrets tells how many of the secrets that a's type needs are real
// values (as opposed to keychain placeholders), and how many it needs.
func storedSecrets(a slackapi.Auth) (real, needed int) {
	var secrets []string
	switch a.Type {
	case slackapi.AuthStandard:
		secrets = []string{a.Token}
	case slackapi.AuthBrowser:
		secrets = []string{a.XoxcToken, a.XoxdCookie}
	}
	for _, v := range secrets {
		if isSecret(v) {
			real++
		}
	}
	return real, len(secrets)
}

// resolveAuth returns usable credentials for w, reading placeholders from
// the keychain. The error is a *SecretsError: a secret is missing, or the
// keychain could not be read.
func resolveAuth(kc Keychain, service string, w Workspace) (slackapi.Auth, error) {
	a := w.Auth
	var cause error
	get := func(stored string, accounts ...string) string {
		if isSecret(stored) {
			return stored
		}
		if kc == nil || cause != nil {
			return ""
		}
		for _, account := range accounts {
			v, err := kc.Get(service, account)
			if err != nil {
				cause = err
				return ""
			}
			if v != "" {
				return v
			}
		}
		return ""
	}
	ok := false
	switch a.Type {
	case slackapi.AuthStandard:
		a.Token = get(a.Token, "token:"+w.URL)
		ok = a.Token != ""
	case slackapi.AuthBrowser:
		a.XoxcToken = get(a.XoxcToken, "xoxc:"+w.URL)
		// "xoxd" (no workspace) is the predecessor's single shared cookie.
		a.XoxdCookie = get(a.XoxdCookie, "xoxd:"+w.URL, "xoxd")
		ok = a.XoxcToken != "" && a.XoxdCookie != ""
	}
	if !ok {
		return slackapi.Auth{Type: a.Type}, &SecretsError{WorkspaceURL: w.URL, Cause: cause}
	}
	return a, nil
}

// Resolve returns usable credentials for a workspace returned by Load. It
// fails with a *SecretsError when its secrets cannot be used: either they
// are gone (Missing: the caller should re-import that workspace), or the
// keychain could not be read just now (locked, timed out), in which case
// the caller must not conclude anything about the workspace.
func (s *Store) Resolve(w Workspace) (slackapi.Auth, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if a, ok := s.resolved[w.URL]; ok {
		return a, nil
	}
	a, err := resolveAuth(s.Keychain, s.Service, w)
	if err != nil {
		return a, err
	}
	if s.resolved == nil {
		s.resolved = map[string]slackapi.Auth{}
	}
	s.resolved[w.URL] = a
	return a, nil
}

// save writes creds, moving real secrets to the keychain when possible.
//
// It also maintains the default workspace: a default that no longer exists
// is dropped, and a store holding exactly one workspace has it as its
// default. Several workspaces without a default stay that way: nothing
// says which one the user means.
func (s *Store) save(creds *Credentials) error {
	out := &Credentials{
		Version:             1,
		UpdatedAt:           time.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
		DefaultWorkspaceURL: creds.DefaultWorkspaceURL,
		LegacyMigrated:      creds.LegacyMigrated,
		Workspaces:          make([]Workspace, len(creds.Workspaces)),
	}
	copy(out.Workspaces, creds.Workspaces)
	if out.Find(out.DefaultWorkspaceURL) == nil {
		out.DefaultWorkspaceURL = ""
	}
	if out.DefaultWorkspaceURL == "" && len(out.Workspaces) == 1 {
		out.DefaultWorkspaceURL = out.Workspaces[0].URL
	}

	// refused collects, in order, the workspaces of which the keychain did
	// not take a secret, and why (the first reason is enough).
	var refused []string
	var refusedWhy error
	stash := func(workspaceURL, account string, secret *string) {
		if s.Keychain == nil || !isSecret(*secret) {
			return
		}
		if cur, err := s.Keychain.Get(s.Service, account); err == nil && cur == *secret {
			*secret = keychainPlaceholder
			return
		}
		err := s.Keychain.Set(s.Service, account, *secret)
		if err == nil {
			*secret = keychainPlaceholder
			return
		}
		// The secret stays in the file: refusing to save would lock the
		// user out. It is said below, once the file is written.
		if len(refused) == 0 {
			refusedWhy = err
		}
		if len(refused) == 0 || refused[len(refused)-1] != workspaceURL {
			refused = append(refused, workspaceURL)
		}
	}
	resolved := map[string]slackapi.Auth{}
	for i := range out.Workspaces {
		w := &out.Workspaces[i]
		// Only complete credentials are cached. An auth read back from a
		// file that holds one secret and a placeholder for the other (the
		// keychain refused one of them) is neither what is to be saved in
		// full nor what was resolved earlier: resolveAuth sorts it out.
		switch real, needed := storedSecrets(w.Auth); {
		case needed > 0 && real == needed:
			resolved[w.URL] = w.Auth
		case real == 0:
			if a, ok := s.resolved[w.URL]; ok {
				resolved[w.URL] = a
			}
		}
		switch w.Auth.Type {
		case slackapi.AuthStandard:
			stash(w.URL, "token:"+w.URL, &w.Auth.Token)
		case slackapi.AuthBrowser:
			stash(w.URL, "xoxc:"+w.URL, &w.Auth.XoxcToken)
			stash(w.URL, "xoxd:"+w.URL, &w.Auth.XoxdCookie)
		}
	}

	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	if err := writeFileAtomic(s.Path, append(data, '\n')); err != nil {
		return fmt.Errorf("writing credentials: %w", err)
	}
	s.loaded = out
	s.resolved = resolved
	s.warnPlaintext(refused, refusedWhy)
	return nil
}

// warnPlaintext says that secrets which belong in the keychain were written
// to the credentials file instead, once per workspace: the next saves find
// them there and try the keychain again, which need not be said each time.
func (s *Store) warnPlaintext(workspaces []string, cause error) {
	var fresh []string
	for _, u := range workspaces {
		if !s.warnedPlaintext[u] {
			fresh = append(fresh, u)
		}
	}
	if len(fresh) == 0 || s.Warnf == nil {
		return
	}
	if s.warnedPlaintext == nil {
		s.warnedPlaintext = map[string]bool{}
	}
	for _, u := range fresh {
		s.warnedPlaintext[u] = true
	}
	s.Warnf(`warning: the secrets of %s were stored in plain text in the credentials file %s (readable by your user only) instead of the keychain: %v. They work from there. To move them into the keychain, make it usable (unlock it, answer its prompt, or raise %s), then save credentials again, e.g. with "%s auth import-desktop" or the same "auth add"`,
		strings.Join(fresh, ", "), s.Path, cause, appinfo.EnvName("KEYCHAIN_TIMEOUT_MS"), appinfo.Name)
}

// SecretsStorage says where the secrets of a stored workspace are kept.
type SecretsStorage string

const (
	// SecretsInKeychain means that the credentials file holds placeholders
	// for the secrets of the workspace, which are expected in the keychain
	// (Resolve tells whether they are still there).
	SecretsInKeychain SecretsStorage = "keychain"
	// SecretsInFile means that at least one secret of the workspace is in
	// plain text in the credentials file. That is the rule where there is
	// no keychain (Store.Keychain is nil); where there is one, the keychain
	// did not take the secret, and the next save tries again.
	SecretsInFile SecretsStorage = "file"
)

// SecretsStorage tells where the secrets of a workspace returned by Load
// are kept, without reading the keychain.
func (s *Store) SecretsStorage(w Workspace) SecretsStorage {
	s.mu.Lock()
	defer s.mu.Unlock()
	// What was loaded or saved last says it, not a copy of the workspace
	// whose placeholders the caller may have replaced.
	if s.loaded != nil {
		if cur := s.loaded.Find(w.URL); cur != nil {
			w = *cur
		}
	}
	if real, _ := storedSecrets(w.Auth); real > 0 {
		return SecretsInFile
	}
	return SecretsInKeychain
}

// writeFileAtomic replaces path with a 0600 file holding data, so that a
// reader sees either the old content or the new one, and a crash leaves one
// of the two: the data is written to a temporary file in the same directory
// (a rename does not cross file systems), flushed to disk, then renamed
// over path. The temporary file is created 0600 before anything is written
// to it, and removed if the replacement fails.
func writeFileAtomic(path string, data []byte) (err error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = os.Remove(tmp.Name())
		}
	}()
	if err := writeAndSync(tmp, data); err != nil {
		return err
	}
	return renameOver(tmp.Name(), path, renameBlockedByReader)
}

// writeAndSync writes data to f, flushes it to disk and closes f, whatever
// happens. Without the flush, a crash soon after the rename can leave the
// new name on an empty file.
func writeAndSync(f *os.File, data []byte) error {
	_, err := f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	return err
}

// renameOver renames tmp over path, trying again for a short while as long
// as transient says that the failure will pass.
func renameOver(tmp, path string, transient func(error) bool) error {
	const attempts, pause = 50, 10 * time.Millisecond
	err := os.Rename(tmp, path)
	for i := 1; err != nil && i < attempts && transient(err); i++ {
		time.Sleep(pause)
		err = os.Rename(tmp, path)
	}
	return err
}

// renameBlockedByReader reports whether a rename failed because another
// process has the destination open, which only Windows minds: a command
// that reads the credentials file does not take the lock, and for as long
// as it has the file open the file cannot be replaced (ERROR_ACCESS_DENIED
// or ERROR_SHARING_VIOLATION).
func renameBlockedByReader(err error) bool {
	const accessDenied, sharingViolation = syscall.Errno(5), syscall.Errno(32)
	var errno syscall.Errno
	return runtime.GOOS == "windows" && errors.As(err, &errno) &&
		(errno == accessDenied || errno == sharingViolation)
}

// Upsert adds or replaces workspaces (matched by URL) and saves. Metadata
// that the new entry leaves blank (name, team id, team domain) is kept from
// the old one. Use it for what the user asked for explicitly, workspace by
// workspace ("auth add", "auth parse-curl"); imports go through
// UpsertImported.
//
// It saves even while the predecessor's credentials could not be migrated
// (see migrateLegacy): they are merged into the file by a later run.
//
// The default workspace is only ever set implicitly when the store holds
// exactly one workspace: the first one added on its own becomes the default
// and stays it as others are added later, whereas several workspaces added
// at once to an empty store leave it without a default (see SetDefault).
func (s *Store) Upsert(workspaces ...Workspace) error {
	_, err := s.upsert(false, workspaces)
	return err
}

// UpsertImported is Upsert for credentials found in a local application
// (Slack Desktop, a browser) rather than given for one workspace: it adds
// new workspaces and refreshes browser-auth ones, but never replaces a
// workspace stored with a standard token (xoxb-/xoxp-, saved by "auth add")
// with a browser session, which would silently change the identity that
// commands act as. Such workspaces are left untouched and their URLs
// returned in kept. That includes a standard token that is still in the
// predecessor's credentials because they could not be migrated yet.
func (s *Store) UpsertImported(workspaces ...Workspace) (kept []string, err error) {
	return s.upsert(true, workspaces)
}

func (s *Store) upsert(keepStandard bool, workspaces []Workspace) (kept []string, err error) {
	if len(workspaces) == 0 {
		return nil, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	err = s.update(func(next *Credentials) error {
		kept = nil
		for _, w := range workspaces {
			u, err := NormalizeURL(w.URL)
			if err != nil {
				return err
			}
			w.URL = u
			old := next.Find(u)
			if old == nil {
				if keepStandard && w.Auth.IsBrowser() && s.unmigratedStandard(u) {
					kept = append(kept, u)
					continue
				}
				next.Workspaces = append(next.Workspaces, w)
				continue
			}
			if keepStandard && old.Auth.Type == slackapi.AuthStandard && w.Auth.IsBrowser() {
				kept = append(kept, u)
				continue
			}
			if w.Name == "" {
				w.Name = old.Name
			}
			if w.TeamID == "" {
				w.TeamID = old.TeamID
			}
			if w.TeamDomain == "" {
				w.TeamDomain = old.TeamDomain
			}
			*old = w
			delete(s.resolved, u)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return kept, nil
}

// SetDefault makes a configured workspace the default.
func (s *Store) SetDefault(workspaceURL string) error {
	u, err := NormalizeURL(workspaceURL)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.update(func(next *Credentials) error {
		if next.Find(u) == nil {
			return fmt.Errorf("workspace %s is not configured", u)
		}
		next.DefaultWorkspaceURL = u
		return nil
	})
}

// Remove deletes a workspace and its keychain secrets. It reports whether
// the workspace existed. Removing the default workspace leaves the store
// without a default, unless exactly one workspace remains.
func (s *Store) Remove(workspaceURL string) (bool, error) {
	u, err := NormalizeURL(workspaceURL)
	if err != nil {
		return false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	errAbsent := errors.New("absent")
	err = s.update(func(next *Credentials) error {
		if next.Find(u) == nil {
			return errAbsent
		}
		kept := next.Workspaces[:0]
		for _, w := range next.Workspaces {
			if w.URL != u {
				kept = append(kept, w)
			}
		}
		next.Workspaces = kept
		if next.DefaultWorkspaceURL == u {
			next.DefaultWorkspaceURL = ""
		}
		delete(s.resolved, u)
		return nil
	})
	if errors.Is(err, errAbsent) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if s.Keychain != nil {
		for _, prefix := range []string{"token:", "xoxc:", "xoxd:"} {
			s.Keychain.Delete(s.Service, prefix+u)
		}
	}
	return true, nil
}
