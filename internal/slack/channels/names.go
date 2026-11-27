package channels

import (
	"cmp"
	"context"
	"encoding/json"
	"os"
	"sync"
	"time"

	"github.com/maelvls/slackcli/internal/appinfo"
	"github.com/maelvls/slackcli/internal/cachefile"
	"github.com/maelvls/slackcli/internal/slackapi"
	"github.com/maelvls/slackcli/internal/slackid"
)

const (
	nameCacheVersion = 1
	// NameCacheTTL is how long a cached conversation name is shown before
	// being looked up again. It is that of the user cache: a DM is named
	// after a user, and channels are renamed about as rarely as people
	// change their name. A renamed channel keeps its old name for at most
	// a day, and only in the output: the ID printed next to it is right.
	NameCacheTTL = 24 * time.Hour
	// nameCacheKeep is how long an expired name stays in the file, to be
	// shown when looking it up again is rate limited.
	nameCacheKeep = 30 * 24 * time.Hour
)

// namesPolicy decides, for the file and the memory alike, whether a name
// is fresh, stale or to be discarded, and which of two copies wins (see
// cachefile.Policy).
var namesPolicy = cachefile.Policy{TTL: NameCacheTTL, Keep: nameCacheKeep}

// NamesOptions tunes NewNames.
type NamesOptions struct {
	// WorkspaceURL keys the cache; "" means the WorkspaceURL of the client
	// of the first lookup. When neither is known (a standard token used
	// without a workspace), names are looked up without being cached.
	WorkspaceURL string
	// ForceRefresh looks every name up again, ignoring cached entries.
	// What is looked up is cached as usual.
	ForceRefresh bool
	// CacheDir is where cache files live; "" means appinfo.CacheDir().
	CacheDir string
	// Now is the clock; nil means time.Now.
	Now func() time.Time
}

// Names gives the names of conversations to show, from a cache kept on
// disk per workspace for NameCacheTTL, so that a command run again and
// again (unreads, draft list …) does not cost one conversations.info call
// per conversation each time just to print its name. The cache is safe to
// share between concurrent invocations; failing to read or write it is
// never an error.
//
// It is for display only. A name typed by the user is always resolved
// live (ResolveID): a message must never go to a conversation picked from
// a stale cache. Nor does it replace the calls that read more than the
// name (the read cursor of Lookup, FetchInfo).
//
// A nil *Names caches nothing: Lookup is LookupName. A Names is safe for
// concurrent use.
type Names struct {
	opts NamesOptions

	load    sync.Once
	path    string
	mu      sync.Mutex
	entries map[string]nameEntry
	// fetched holds the names learnt from Slack by this Names: each write
	// has them all, in case another invocation wrote the file in between.
	fetched map[string]nameEntry
}

type nameEntry struct {
	// FetchedAt is in Unix milliseconds.
	FetchedAt int64  `json:"fetched_at"`
	Name      string `json:"name"`
}

type nameCacheFile struct {
	Version int                  `json:"version"`
	Entries map[string]nameEntry `json:"entries"`
}

// Fetched is when the lookup that got e started (cachefile.Entry).
func (e nameEntry) Fetched() int64 { return e.FetchedAt }

// NewNames returns a cache of conversation names. The file is read on the
// first lookup.
func NewNames(opts NamesOptions) *Names {
	return &Names{opts: opts}
}

// Lookup returns the name of a conversation to show, as LookupName does:
// from the cache when it holds a fresh one, else from Slack, in which case
// the name is cached. A name that could not be had (an error, or Slack
// has none) is not cached. The error is that of LookupName, and the same
// rules apply to it.
func (n *Names) Lookup(ctx context.Context, client *slackapi.Client, channelID string) (string, error) {
	if n == nil {
		return LookupName(ctx, client, channelID)
	}
	n.open(client)
	if !n.opts.ForceRefresh {
		n.mu.Lock()
		e, ok := n.entries[channelID]
		n.mu.Unlock()
		if ok && namesPolicy.State(e.FetchedAt, n.now()) == cachefile.Fresh {
			return e.Name, nil
		}
	}
	// Dated by when the lookup started, not when it ended: a lookup that
	// started before a refresh made by another invocation may get the
	// name from before a rename, and must not win over the refresh.
	started := n.now()
	name, err := LookupName(ctx, client, channelID)
	if err == nil {
		n.Remember(client, channelID, name, started)
	}
	return name, err
}

// Now is the time to give Remember for a lookup that starts now; for a nil
// *Names, the current time.
func (n *Names) Now() time.Time {
	if n == nil {
		return time.Now()
	}
	return n.now()
}

// Stale returns the cached name of a conversation, fresh or stale, ""
// when there is none or it is to be discarded (dated in the future, or
// older than nameCacheKeep): for a conversation whose lookup was rate
// limited, a stale name is better than none.
func (n *Names) Stale(client *slackapi.Client, channelID string) string {
	if n == nil {
		return ""
	}
	n.open(client)
	n.mu.Lock()
	e, ok := n.entries[channelID]
	n.mu.Unlock()
	if !ok || namesPolicy.State(e.FetchedAt, n.now()) == cachefile.Discard {
		return ""
	}
	return e.Name
}

// Remember caches a name learnt from Slack by a call made for more than
// the name (Lookup, for the read cursor) that started at fetchedAt (see
// Now). An empty name is not cached. The file, the names this Names
// shows and the ones it learnt are all merged by namesPolicy: what is to
// be discarded is dropped, and of two names of a conversation, the one
// whose lookup started last wins, whichever was written last.
//
// The file is written at once, not when the command ends, so that a
// long-running command (watch, inbox wait) shares what it learns. It is
// read again, merged and written under the lock of the file
// (cachefile.Lock), so that the entries other invocations add meanwhile
// are kept.
func (n *Names) Remember(client *slackapi.Client, channelID, name string, fetchedAt time.Time) {
	if n == nil || name == "" || !slackid.IsChannelID(channelID) {
		return
	}
	n.open(client)
	e := nameEntry{FetchedAt: fetchedAt.UnixMilli(), Name: name}
	// n.mu keeps the lookups of this invocation from writing at the same
	// time, the file lock those of other invocations.
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.path != "" {
		defer cachefile.Lock(n.path)()
	}
	// The clock is read once the locks are held: while this call looked
	// the name up and waited, other invocations may have committed names
	// dated after fetchedAt, which must not look like names from the
	// future.
	now := n.now()
	cachefile.Prune(namesPolicy, n.entries, now, cachefile.Stale)
	cachefile.Prune(namesPolicy, n.fetched, now, cachefile.Stale)
	cachefile.Merge(namesPolicy, n.entries, channelID, e, now)
	cachefile.Merge(namesPolicy, n.fetched, channelID, e, now)
	if n.path == "" {
		return
	}
	entries := loadNames(n.path)
	cachefile.Prune(namesPolicy, entries, now, cachefile.Stale)
	// Writing again every name this invocation learnt makes up for an
	// update lost when the lock could not be had.
	for id, mine := range n.fetched {
		cachefile.Merge(namesPolicy, entries, id, mine, now)
	}
	// What other invocations refreshed since this one read the file is
	// what it shows from now on (a long-running watch or inbox wait).
	for id, theirs := range entries {
		cachefile.Merge(namesPolicy, n.entries, id, theirs, now)
	}
	data, err := json.MarshalIndent(nameCacheFile{Version: nameCacheVersion, Entries: entries}, "", "  ")
	if err != nil {
		return
	}
	cachefile.Write(n.path, append(data, '\n'))
}

// open reads the cache file of the workspace, once.
func (n *Names) open(client *slackapi.Client) {
	n.load.Do(func() {
		workspaceURL := n.opts.WorkspaceURL
		if workspaceURL == "" && client != nil {
			workspaceURL = client.WorkspaceURL()
		}
		n.path = cachefile.Path(cmp.Or(n.opts.CacheDir, appinfo.CacheDir()), "names-cache", workspaceURL)
		entries := loadNames(n.path)
		n.mu.Lock()
		n.entries, n.fetched = entries, map[string]nameEntry{}
		n.mu.Unlock()
	})
}

func (n *Names) now() time.Time {
	if n.opts.Now != nil {
		return n.opts.Now()
	}
	return time.Now()
}

// loadNames reads a cache file. A missing, unreadable, corrupt or
// foreign-version file is an empty cache.
func loadNames(path string) map[string]nameEntry {
	entries := map[string]nameEntry{}
	if path == "" {
		return entries
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return entries
	}
	var file nameCacheFile
	if err := json.Unmarshal(data, &file); err != nil || file.Version != nameCacheVersion {
		return entries
	}
	for id, e := range file.Entries {
		if slackid.IsChannelID(id) && e.FetchedAt > 0 && e.Name != "" {
			entries[id] = e
		}
	}
	return entries
}
