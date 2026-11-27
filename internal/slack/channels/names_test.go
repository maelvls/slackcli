package channels

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/maelvls/slackcli/internal/appinfo"
	"github.com/maelvls/slackcli/internal/cachefile"
	"github.com/maelvls/slackcli/internal/slack/enrich"
	"github.com/maelvls/slackcli/internal/slackapi"
	"github.com/maelvls/slackcli/internal/testenv"
)

var t0 = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

func at(t time.Time) func() time.Time { return func() time.Time { return t } }

// namesServer answers conversations.info and users.info like a workspace
// with two channels and a DM. A conversation in missing is not found.
func namesServer(missing ...string) *recorder {
	return &recorder{serve: func(method string, params map[string]any) (map[string]any, error) {
		switch method {
		case "conversations.info":
			id := params["channel"].(string)
			for _, m := range missing {
				if m == id {
					return apiError("channel_not_found"), nil
				}
			}
			if id == "D0DM000001" {
				return map[string]any{"channel": map[string]any{"id": id, "is_im": true, "user": "U0ALICE001", "last_read": "1.000001"}}, nil
			}
			return map[string]any{"channel": map[string]any{"id": id, "name": "chan-" + id, "last_read": "1.000001"}}, nil
		case "users.info":
			return map[string]any{"user": map[string]any{"id": "U0ALICE001", "name": "alice", "real_name": "Alice Example",
				"profile": map[string]any{"display_name": "ali", "real_name": "Alice Example"}}}, nil
		}
		return nil, fmt.Errorf("unexpected method %s", method)
	}}
}

func namesFile(t *testing.T, dir string) string {
	t.Helper()
	return cachefile.Path(dir, "names-cache", "https://test.slack.com")
}

func TestNamesCacheNames(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "cache")
	opts := NamesOptions{CacheDir: dir, Now: at(t0)}

	first := namesServer()
	names := NewNames(opts)
	for _, id := range []string{"C0GENERAL1", "D0DM000001"} {
		if _, err := names.Lookup(ctx, first.client(), id); err != nil {
			t.Fatal(err)
		}
	}
	if got := first.methods(); !reflect.DeepEqual(got, []string{"conversations.info", "conversations.info", "users.info"}) {
		t.Errorf("first run: calls %v", got)
	}

	// Another invocation, a little less than a day later, asks Slack
	// nothing and prints the same names. The DM keeps its own precedence
	// (display name first).
	again := namesServer()
	names = NewNames(NamesOptions{CacheDir: dir, Now: at(t0.Add(NameCacheTTL - time.Minute))})
	for id, want := range map[string]string{"C0GENERAL1": "chan-C0GENERAL1", "D0DM000001": "ali"} {
		got, err := names.Lookup(ctx, again.client(), id)
		if err != nil || got != want {
			t.Errorf("%s: got %q, %v, want %q", id, got, err, want)
		}
		if live := ResolveName(ctx, namesServer().client(), id); live != want {
			t.Errorf("%s: cached name %q, live name %q", id, want, live)
		}
	}
	if len(again.calls) != 0 {
		t.Errorf("second run: calls %v", again.methods())
	}
}

func TestNamesExpire(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	if _, err := NewNames(NamesOptions{CacheDir: dir, Now: at(t0)}).Lookup(ctx, namesServer().client(), "C0GENERAL1"); err != nil {
		t.Fatal(err)
	}
	rec := namesServer()
	names := NewNames(NamesOptions{CacheDir: dir, Now: at(t0.Add(NameCacheTTL))})
	if got, err := names.Lookup(ctx, rec.client(), "C0GENERAL1"); err != nil || got != "chan-C0GENERAL1" {
		t.Fatalf("got %q, %v", got, err)
	}
	if len(rec.only("conversations.info")) != 1 {
		t.Errorf("an expired name was served: calls %v", rec.methods())
	}
	// The name looked up again is good for another day.
	if e := loadNames(namesFile(t, dir))["C0GENERAL1"]; e.FetchedAt != t0.Add(NameCacheTTL).UnixMilli() {
		t.Errorf("entry = %+v", e)
	}
}

// --refresh-names: every name is looked up again, and cached again.
func TestNamesForceRefresh(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	if _, err := NewNames(NamesOptions{CacheDir: dir, Now: at(t0)}).Lookup(ctx, namesServer().client(), "C0GENERAL1"); err != nil {
		t.Fatal(err)
	}
	renamed := &recorder{serve: func(string, map[string]any) (map[string]any, error) {
		return map[string]any{"channel": map[string]any{"id": "C0GENERAL1", "name": "renamed"}}, nil
	}}
	later := t0.Add(time.Hour)
	names := NewNames(NamesOptions{CacheDir: dir, Now: at(later), ForceRefresh: true})
	if got, err := names.Lookup(ctx, renamed.client(), "C0GENERAL1"); err != nil || got != "renamed" || len(renamed.calls) != 1 {
		t.Fatalf("got %q, %v after %d calls", got, err, len(renamed.calls))
	}
	rec := namesServer()
	if got, _ := NewNames(NamesOptions{CacheDir: dir, Now: at(later)}).Lookup(ctx, rec.client(), "C0GENERAL1"); got != "renamed" || len(rec.calls) != 0 {
		t.Errorf("after the refresh: got %q after %d calls", got, len(rec.calls))
	}
}

// A name that could not be had is asked for again next time.
func TestNamesFailuresAreNotCached(t *testing.T) {
	ctx := context.Background()
	failures := map[string]func(method string) (map[string]any, error){
		"channel not found": func(string) (map[string]any, error) { return apiError("channel_not_found"), nil },
		"DM user not found": func(method string) (map[string]any, error) {
			if method == "users.info" {
				return apiError("user_not_found"), nil
			}
			return map[string]any{"channel": map[string]any{"is_im": true, "user": "U0ALICE001"}}, nil
		},
		"rate limited": func(method string) (map[string]any, error) {
			return nil, &slackapi.RateLimitError{Method: method, RetryAfter: time.Minute}
		},
		"no name": func(string) (map[string]any, error) {
			return map[string]any{"channel": map[string]any{"id": "C0GENERAL1"}}, nil
		},
	}
	for name, fail := range failures {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			opts := NamesOptions{CacheDir: dir, Now: at(t0)}
			rec := &recorder{serve: func(method string, _ map[string]any) (map[string]any, error) { return fail(method) }}
			if got, _ := NewNames(opts).Lookup(ctx, rec.client(), "C0GENERAL1"); got != "" {
				t.Errorf("got %q", got)
			}
			if _, err := os.Stat(namesFile(t, dir)); !os.IsNotExist(err) {
				t.Errorf("cache file written: %v", err)
			}
			rec = namesServer()
			if got, err := NewNames(opts).Lookup(ctx, rec.client(), "C0GENERAL1"); err != nil || got != "chan-C0GENERAL1" || len(rec.calls) != 1 {
				t.Errorf("next run: got %q, %v after %d calls", got, err, len(rec.calls))
			}
		})
	}
}

func TestNamesErrorsAreLookupNames(t *testing.T) {
	ctx := context.Background()
	rec := &recorder{serve: func(string, map[string]any) (map[string]any, error) { return apiError("invalid_auth"), nil }}
	if _, err := NewNames(NamesOptions{CacheDir: t.TempDir()}).Lookup(ctx, rec.client(), "C0GENERAL1"); !slackapi.IsAuthError(err) {
		t.Errorf("err = %v, want an auth error", err)
	}
	rec = &recorder{serve: func(method string, _ map[string]any) (map[string]any, error) {
		return nil, &slackapi.RateLimitError{Method: method, RetryAfter: time.Minute}
	}}
	if _, err := NewNames(NamesOptions{CacheDir: t.TempDir()}).Lookup(ctx, rec.client(), "C0GENERAL1"); !enrich.IsRateLimit(err) {
		t.Errorf("err = %v, want a rate limit", err)
	}
}

// A nil *Names caches nothing: every lookup is LookupName.
func TestNilNames(t *testing.T) {
	testenv.Isolate(t)
	var names *Names
	rec := namesServer()
	for range 2 {
		if got, err := names.Lookup(context.Background(), rec.client(), "D0DM000001"); err != nil || got != "ali" {
			t.Fatalf("got %q, %v", got, err)
		}
	}
	names.Remember(rec.client(), "C0GENERAL1", "general")
	if len(rec.calls) != 4 || names.Stale(rec.client(), "C0GENERAL1") != "" {
		t.Errorf("calls %v", rec.methods())
	}
	if entries, _ := os.ReadDir(appinfo.CacheDir()); len(entries) != 0 {
		t.Errorf("cache dir holds %v", entries)
	}
}

// A clock that moved backwards leaves entries dated in the future: they
// are not trusted, or they would never expire.
func TestNamesClockMovedBackwards(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	if _, err := NewNames(NamesOptions{CacheDir: dir, Now: at(t0.Add(time.Hour))}).Lookup(ctx, namesServer().client(), "C0GENERAL1"); err != nil {
		t.Fatal(err)
	}
	rec := namesServer()
	if _, err := NewNames(NamesOptions{CacheDir: dir, Now: at(t0)}).Lookup(ctx, rec.client(), "C0GENERAL1"); err != nil || len(rec.calls) != 1 {
		t.Errorf("%d calls, %v: a name from the future was served", len(rec.calls), err)
	}
	if e := loadNames(namesFile(t, dir))["C0GENERAL1"]; e.FetchedAt != t0.UnixMilli() {
		t.Errorf("entry = %+v, want it dated now", e)
	}
}

// When looking a name up again is rate limited, the expired name is
// better than none. It stays in the file well past its TTL for that.
func TestNamesStale(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	if _, err := NewNames(NamesOptions{CacheDir: dir, Now: at(t0)}).Lookup(ctx, namesServer().client(), "C0GENERAL1"); err != nil {
		t.Fatal(err)
	}
	// Another name cached a week later keeps the first one in the file.
	week := t0.Add(7 * 24 * time.Hour)
	names := NewNames(NamesOptions{CacheDir: dir, Now: at(week)})
	if _, err := names.Lookup(ctx, namesServer().client(), "C0RANDOM01"); err != nil {
		t.Fatal(err)
	}
	if got := names.Stale(namesServer().client(), "C0GENERAL1"); got != "chan-C0GENERAL1" {
		t.Errorf("Stale = %q", got)
	}
	if got := names.Stale(namesServer().client(), "C0UNKNOWN1"); got != "" {
		t.Errorf("Stale of an unknown conversation = %q", got)
	}
	// After nameCacheKeep, the next write drops it.
	names = NewNames(NamesOptions{CacheDir: dir, Now: at(t0.Add(nameCacheKeep))})
	names.Remember(namesServer().client(), "C0OTHER001", "other")
	if entries := loadNames(namesFile(t, dir)); len(entries) != 2 || entries["C0GENERAL1"].Name != "" {
		t.Errorf("entries = %+v", entries)
	}
}

func TestNamesKeyedByWorkspace(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	for _, ws := range []string{"https://acme.slack.com", "https://other.slack.com"} {
		rec := namesServer()
		if _, err := NewNames(NamesOptions{CacheDir: dir, Now: at(t0), WorkspaceURL: ws}).Lookup(ctx, rec.client(), "C0GENERAL1"); err != nil {
			t.Fatal(err)
		}
		if len(rec.calls) != 1 {
			t.Errorf("%s: %d calls, want 1 (no cross-workspace hit)", ws, len(rec.calls))
		}
	}
	// By default, the workspace of the client (NewFake: test.slack.com).
	if _, err := NewNames(NamesOptions{CacheDir: dir, Now: at(t0)}).Lookup(ctx, namesServer().client(), "C0GENERAL1"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(namesFile(t, dir)); err != nil {
		t.Error(err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 3 {
		t.Errorf("cache files %v, want 3", entries)
	}
}

func TestNamesWithoutWorkspaceDoNotCache(t *testing.T) {
	dir := t.TempDir()
	rec := namesServer()
	client := slackapi.New(slackapi.Auth{Type: slackapi.AuthStandard, Token: "xoxb-test"}, "", slackapi.WithHandler(
		func(_ context.Context, method string, params map[string]any) (map[string]any, error) {
			rec.calls = append(rec.calls, call{method, params})
			return rec.serve(method, params)
		}))
	names := NewNames(NamesOptions{CacheDir: dir, Now: at(t0)})
	for range 2 {
		if got, err := names.Lookup(context.Background(), client, "C0GENERAL1"); err != nil || got != "chan-C0GENERAL1" {
			t.Fatalf("got %q, %v", got, err)
		}
	}
	// Within the invocation, the name is still not asked for twice.
	if entries, _ := os.ReadDir(dir); len(rec.calls) != 1 || len(entries) != 0 {
		t.Errorf("%d calls, files %v", len(rec.calls), entries)
	}
}

func TestNamesDefaultToTheAppCacheDir(t *testing.T) {
	home := testenv.Isolate(t)
	if _, err := NewNames(NamesOptions{}).Lookup(context.Background(), namesServer().client(), "C0GENERAL1"); err != nil {
		t.Fatal(err)
	}
	if rel, err := filepath.Rel(home, appinfo.CacheDir()); err != nil || !filepath.IsLocal(rel) {
		t.Fatalf("cache dir %s is not below the test's home %s", appinfo.CacheDir(), home)
	}
	if entries, _ := os.ReadDir(appinfo.CacheDir()); len(entries) != 1 {
		t.Errorf("cache dir %s holds %v", appinfo.CacheDir(), entries)
	}
}

// Private channel names are on disk: only their owner may read them.
func TestNamesFilePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no Unix permissions on Windows")
	}
	dir := filepath.Join(t.TempDir(), "cache")
	if _, err := NewNames(NamesOptions{CacheDir: dir, Now: at(t0)}).Lookup(context.Background(), namesServer().client(), "C0GENERAL1"); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]os.FileMode{dir: 0o700, namesFile(t, dir): 0o600} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Errorf("%s: mode %v, want %v", filepath.Base(path), got, want)
		}
	}
}

func TestNamesFileFormat(t *testing.T) {
	dir := t.TempDir()
	if _, err := NewNames(NamesOptions{CacheDir: dir, Now: at(t0)}).Lookup(context.Background(), namesServer().client(), "C0GENERAL1"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(namesFile(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	var file map[string]any
	if err := json.Unmarshal(data, &file); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"version": float64(1),
		"entries": map[string]any{"C0GENERAL1": map[string]any{"fetched_at": float64(t0.UnixMilli()), "name": "chan-C0GENERAL1"}},
	}
	if !reflect.DeepEqual(file, want) {
		t.Errorf("file = %s", data)
	}
}

func TestNamesFileTolerance(t *testing.T) {
	ctx := context.Background()
	fresh := t0.UnixMilli()
	tests := map[string]string{
		"corrupt JSON":     `{"version": 1, "entries": {"C0GENERAL1": `,
		"not an object":    `[]`,
		"other version":    fmt.Sprintf(`{"version": 2, "entries": {"C0GENERAL1": {"fetched_at": %d, "name": "stale"}}}`, fresh),
		"no fetched_at":    `{"version": 1, "entries": {"C0GENERAL1": {"name": "stale"}}}`,
		"empty name":       fmt.Sprintf(`{"version": 1, "entries": {"C0GENERAL1": {"fetched_at": %d, "name": ""}}}`, fresh),
		"wrong types":      `{"version": 1, "entries": {"C0GENERAL1": "nope"}}`,
		"empty file":       ``,
		"not a channel ID": fmt.Sprintf(`{"version": 1, "entries": {"../x": {"fetched_at": %d, "name": "x"}}}`, fresh),
	}
	for name, content := range tests {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := namesFile(t, dir)
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			rec := namesServer()
			got, err := NewNames(NamesOptions{CacheDir: dir, Now: at(t0)}).Lookup(ctx, rec.client(), "C0GENERAL1")
			if err != nil || got != "chan-C0GENERAL1" || len(rec.calls) != 1 {
				t.Fatalf("got %q, %v after %d calls", got, err, len(rec.calls))
			}
			// The bad file was replaced by a good one.
			if entries := loadNames(path); len(entries) != 1 {
				t.Errorf("entries = %+v", entries)
			}
		})
	}

	t.Run("an unreadable or unwritable cache is not an error", func(t *testing.T) {
		blocker := filepath.Join(t.TempDir(), "blocker")
		if err := os.WriteFile(blocker, []byte("a file, not a directory"), 0o600); err != nil {
			t.Fatal(err)
		}
		rec := namesServer()
		names := NewNames(NamesOptions{CacheDir: filepath.Join(blocker, "cache")})
		for range 2 {
			if got, err := names.Lookup(ctx, rec.client(), "C0GENERAL1"); err != nil || got != "chan-C0GENERAL1" {
				t.Errorf("got %q, %v", got, err)
			}
		}
		if len(rec.calls) != 1 {
			t.Errorf("%d calls: the name is still remembered for the invocation", len(rec.calls))
		}
	})
}

// The lookups of one invocation run concurrently, and other invocations
// may write the file at the same time.
func TestNamesConcurrentWriters(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := namesFile(t, dir)

	stop := make(chan struct{})
	var readers sync.WaitGroup
	readers.Go(func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			// A reader must never see a partially written file.
			if data, err := os.ReadFile(path); err == nil {
				var file nameCacheFile
				if err := json.Unmarshal(data, &file); err != nil {
					t.Errorf("torn cache file: %v", err)
					return
				}
			}
		}
	})

	var writers sync.WaitGroup
	for w := range 8 {
		writers.Go(func() {
			for i := range 10 {
				id := fmt.Sprintf("C1%04d%04d", w, i)
				names := NewNames(NamesOptions{CacheDir: dir, Now: at(t0)})
				for _, id := range []string{"C0SHARED01", id} {
					if got, err := names.Lookup(ctx, namesServer().client(), id); err != nil || got != "chan-"+id {
						t.Errorf("writer %d: got %q, %v", w, got, err)
					}
				}
			}
		})
	}
	writers.Wait()
	close(stop)
	readers.Wait()
	// Every invocation keeps what it learnt: the read, merge and write of
	// the file are one step across processes.
	entries := loadNames(path)
	if entries["C0SHARED01"].Name == "" {
		t.Error("shared name missing from the cache")
	}
	for w := range 8 {
		for i := range 10 {
			if id := fmt.Sprintf("C1%04d%04d", w, i); entries[id].Name != "chan-"+id {
				t.Errorf("%s lost (%d entries)", id, len(entries))
			}
		}
	}
	if left, _ := os.ReadDir(dir); len(left) != 1 {
		t.Errorf("temporary files left behind: %v", left)
	}

	// One invocation looking 40 names up at once keeps them all.
	names := NewNames(NamesOptions{CacheDir: dir, Now: at(t0)})
	var lookups sync.WaitGroup
	for i := range 40 {
		lookups.Go(func() {
			id := fmt.Sprintf("C0%08d", i)
			if got, err := names.Lookup(ctx, namesServer().client(), id); err != nil || got != "chan-"+id {
				t.Errorf("got %q, %v", got, err)
			}
		})
	}
	lookups.Wait()
	entries = loadNames(path)
	for i := range 40 {
		if id := fmt.Sprintf("C0%08d", i); entries[id].Name == "" {
			t.Errorf("%s missing from the cache (%d entries)", id, len(entries))
		}
	}
}

// The cache only ever names an ID. A name typed by the user is resolved
// live, whatever the cache says: a message must never go to a channel
// picked from a stale cache.
func TestResolveIDIgnoresTheNamesCache(t *testing.T) {
	testenv.Isolate(t)
	// The cache says C0STALE001 is #general (it was, before a rename).
	NewNames(NamesOptions{Now: at(t0)}).Remember(namesServer().client(), "C0STALE001", "general")

	rec := workspace{search: map[string]map[string]any{"general": ch("C0LIVE0001", "general")}}.recorder()
	id, err := ResolveID(context.Background(), rec.client(), "#general")
	if err != nil || id != "C0LIVE0001" {
		t.Errorf("ResolveID = %q, %v, want the live C0LIVE0001", id, err)
	}
	if len(rec.only("search.messages")) != 1 {
		t.Errorf("calls %v, want a live lookup", rec.methods())
	}
}

// A name dated in the future (the clock moved backwards) is no more
// trusted as a stale fallback than as a fresh one.
func TestNamesStaleRejectsFutureEntries(t *testing.T) {
	dir := t.TempDir()
	NewNames(NamesOptions{CacheDir: dir, WorkspaceURL: "https://test.slack.com", Now: at(t0.Add(time.Hour))}).Remember(nil, "C0GENERAL1", "general")
	if got := NewNames(NamesOptions{CacheDir: dir, Now: at(t0)}).Stale(namesServer().client(), "C0GENERAL1"); got != "" {
		t.Errorf("Stale = %q, want nothing from the future", got)
	}
	// Nor is a name older than what the file keeps.
	if got := NewNames(NamesOptions{CacheDir: dir, Now: at(t0.Add(time.Hour + nameCacheKeep))}).Stale(namesServer().client(), "C0GENERAL1"); got != "" {
		t.Errorf("Stale = %q, want nothing past nameCacheKeep", got)
	}
}

// A long-running invocation writes what it learnt each time it learns a
// new name. That must not undo a refresh another invocation made since,
// nor bring back what the file stopped keeping.
func TestNamesWritesKeepNewerEntries(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	now := t0
	watch := NewNames(NamesOptions{CacheDir: dir, Now: func() time.Time { return now }})
	if _, err := watch.Lookup(ctx, namesServer().client(), "C0GENERAL1"); err != nil {
		t.Fatal(err)
	}

	// An hour later, another invocation refreshes it after a rename.
	renamed := &recorder{serve: func(string, map[string]any) (map[string]any, error) {
		return map[string]any{"channel": map[string]any{"id": "C0GENERAL1", "name": "renamed"}}, nil
	}}
	if _, err := NewNames(NamesOptions{CacheDir: dir, Now: at(t0.Add(time.Hour)), ForceRefresh: true}).Lookup(ctx, renamed.client(), "C0GENERAL1"); err != nil {
		t.Fatal(err)
	}
	// Then the long-running one learns another name.
	now = t0.Add(2 * time.Hour)
	if _, err := watch.Lookup(ctx, namesServer().client(), "C0RANDOM01"); err != nil {
		t.Fatal(err)
	}
	if e := loadNames(namesFile(t, dir))["C0GENERAL1"]; e.Name != "renamed" {
		t.Errorf("C0GENERAL1 = %+v, want the refreshed name", e)
	}

	// Past nameCacheKeep, its old names are dropped, not written again.
	now = t0.Add(time.Hour + nameCacheKeep + time.Minute)
	if _, err := watch.Lookup(ctx, namesServer().client(), "C0OTHER001"); err != nil {
		t.Fatal(err)
	}
	entries := loadNames(namesFile(t, dir))
	if _, ok := entries["C0GENERAL1"]; ok {
		t.Errorf("C0GENERAL1 written back past nameCacheKeep: %+v", entries)
	}
	if entries["C0RANDOM01"].Name == "" || entries["C0OTHER001"].Name == "" {
		t.Errorf("entries = %+v", entries)
	}
}

// A writer may wait for the lock while another one commits its names: by
// the time it reads the file, those are newer than when it started. They
// must not be taken for names from the future and pruned.
func TestNamesWriteAfterWaitingKeepsWhatWasCommittedMeanwhile(t *testing.T) {
	dir := t.TempDir()
	// The clock reads t0 when this writer starts, two minutes later once
	// it holds the lock.
	var calls atomic.Int32
	clock := func() time.Time {
		if calls.Add(1) == 1 {
			return t0
		}
		return t0.Add(2 * time.Minute)
	}
	slow := NewNames(NamesOptions{CacheDir: dir, WorkspaceURL: "https://test.slack.com", Now: clock})
	slow.open(nil)
	// Meanwhile, another writer commits two names.
	other := NewNames(NamesOptions{CacheDir: dir, WorkspaceURL: "https://test.slack.com", Now: at(t0.Add(time.Minute))})
	other.Remember(nil, "C0GENERAL1", "renamed")
	other.Remember(nil, "C0OTHER001", "other")

	slow.fetched["C0GENERAL1"] = nameEntry{FetchedAt: t0.Add(-time.Hour).UnixMilli(), Name: "general"}
	slow.Remember(nil, "C0RANDOM01", "random")

	entries := loadNames(namesFile(t, dir))
	want := map[string]nameEntry{
		"C0GENERAL1": {FetchedAt: t0.Add(time.Minute).UnixMilli(), Name: "renamed"},
		"C0OTHER001": {FetchedAt: t0.Add(time.Minute).UnixMilli(), Name: "other"},
		// Stamped when it was fetched, not when it was written.
		"C0RANDOM01": {FetchedAt: t0.UnixMilli(), Name: "random"},
	}
	if !reflect.DeepEqual(entries, want) {
		t.Errorf("entries = %+v\nwant %+v", entries, want)
	}
}
