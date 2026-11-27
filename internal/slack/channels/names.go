package channels

import (
	"cmp"
	"context"
	"encoding/json"
	"maps"
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

// age returns how long ago e was fetched. An entry dated in the future
// (the clock moved backwards) is not trusted: it would otherwise never
// expire, so it is given an age past every limit.
func (e nameEntry) age(now time.Time) time.Duration {
	age := now.Sub(time.UnixMilli(e.FetchedAt))
	if age < 0 {
		return time.Duration(1<<63 - 1)
	}
	return age
}

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
		if ok && e.age(n.now()) < NameCacheTTL {
			return e.Name, nil
		}
	}
	name, err := LookupName(ctx, client, channelID)
	if err == nil {
		n.Remember(client, channelID, name)
	}
	return name, err
}

// Stale returns the cached name of a conversation however old it is, ""
// when there is none: for a conversation whose lookup was rate limited, a
// stale name is better than none.
func (n *Names) Stale(client *slackapi.Client, channelID string) string {
	if n == nil {
		return ""
	}
	n.open(client)
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.entries[channelID].Name
}

// Remember caches a name learnt from Slack by a call made for more than
// the name (Lookup, for the read cursor). An empty name is not cached.
//
// The file is written at once, not when the command ends, so that a
// long-running command (watch, inbox wait) shares what it learns. It is
// read again, merged and written under the lock of the file
// (cachefile.Lock), so that the entries other invocations add meanwhile
// are kept.
func (n *Names) Remember(client *slackapi.Client, channelID, name string) {
	if n == nil || name == "" || !slackid.IsChannelID(channelID) {
		return
	}
	n.open(client)
	now := n.now()
	e := nameEntry{FetchedAt: now.UnixMilli(), Name: name}
	n.mu.Lock()
	defer n.mu.Unlock()
	n.entries[channelID] = e
	n.fetched[channelID] = e
	if n.path == "" {
		return
	}
	// n.mu keeps the lookups of this invocation from writing at the same
	// time, the file lock those of other invocations.
	defer cachefile.Lock(n.path)()
	entries := loadNames(n.path)
	for id, old := range entries {
		if old.age(now) >= nameCacheKeep {
			delete(entries, id)
		}
	}
	maps.Copy(entries, n.fetched)
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
