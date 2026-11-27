// Package audit keeps a persistent record of every request slackcli makes
// to Slack: one JSON line per HTTP request (each attempt of a Web API
// call, each file download or upload request, each WebSocket connection),
// appended to ~/.config/slackcli/audit.jsonl.
//
// The record answers, after the fact, how often Slack was called, with
// which methods, by which program and which command, and which calls
// failed or were rate limited. It never holds a token, a cookie, a message
// or a search query: of the command line it keeps the command, the names of
// the flags, and the arguments that are Slack IDs, timestamps, numbers or
// Slack URLs (see SanitizeArgs).
//
// Writing it never fails a command and costs next to nothing: each line is
// one write to a file opened with O_APPEND, which keeps the lines of
// processes that run at the same time whole, and a write that fails is
// dropped (and reported once in the live trace, SLACKCLI_DEBUG).
//
// SLACKCLI_AUDIT_LOG=<path> writes it elsewhere; SLACKCLI_AUDIT_LOG=off
// turns it off. Test binaries do not write it unless the variable is set,
// so that tests never touch the real one.
package audit

import (
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/maelvls/slackcli/internal/appinfo"
	"github.com/maelvls/slackcli/internal/trace"
)

// EnvSuffix names the variable that moves or turns off the log
// (SLACKCLI_AUDIT_LOG).
const EnvSuffix = "AUDIT_LOG"

// FileName is the name of the log in the configuration directory.
const FileName = "audit.jsonl"

// maxSize is the size past which the log is rotated to <path>.1, which
// replaces the previous one. A variable so that tests can shrink it.
var maxSize int64 = 20 << 20

// Kinds of request.
const (
	KindAPI       = "api"
	KindFile      = "file"
	KindWebSocket = "websocket"
)

// Entry is one line of the log: one HTTP request to Slack.
type Entry struct {
	// TS is when the request was sent (RFC 3339, UTC, nanoseconds).
	TS string `json:"ts"`
	// PID and PPID are those of the slackcli process; Parent is the name
	// of the parent process and Ancestors those of its own parents, the
	// closest first, when the OS tells them cheaply (macOS, Linux).
	PID       int      `json:"pid"`
	PPID      int      `json:"ppid"`
	Parent    string   `json:"parent,omitempty"`
	Ancestors []string `json:"ancestors,omitempty"`
	// Cmd is the slackcli command ("message list") and Args its command
	// line, made safe to keep (see SanitizeArgs). Both are empty for a
	// request made before the command line was parsed.
	Cmd  string   `json:"cmd,omitempty"`
	Args []string `json:"args,omitempty"`
	// Kind is KindAPI, KindFile or KindWebSocket.
	Kind string `json:"kind"`
	// Host is the host the request was sent to.
	Host string `json:"host,omitempty"`
	// Method is the Web API method ("conversations.history"), or for the
	// other kinds what the request was for ("files.download",
	// "files.upload", "websocket.connect").
	Method string `json:"method"`
	// Status is the HTTP status; 0 when there was no answer.
	Status int `json:"status,omitempty"`
	// OK is the "ok" of a Web API answer that had one.
	OK *bool `json:"ok,omitempty"`
	// Error is the error code Slack gave ("ratelimited",
	// "invalid_auth"…) or, for a request that got no usable answer, one
	// of "network", "timeout", "cancelled", "redirect", "http_status",
	// "not_json", "dial_failed". Never the text of an error.
	Error string `json:"error,omitempty"`
	// DurationMS is how long the request took, in milliseconds.
	DurationMS int64 `json:"duration_ms"`
	// Attempt counts the tries of one Web API call, from 1.
	Attempt int `json:"attempt,omitempty"`
	// RateLimited is true for a request Slack rate limited (an HTTP 429,
	// or the "ratelimited" error code in the body of another answer), and
	// RetryAfterS is the wait Slack asked for, in seconds.
	RateLimited bool    `json:"rate_limited,omitempty"`
	RetryAfterS float64 `json:"retry_after_s,omitempty"`
}

// Failed reports whether the request did not succeed.
func (e Entry) Failed() bool {
	return e.Error != "" || e.RateLimited || (e.OK != nil && !*e.OK) || e.Status >= 400
}

// Path returns where the log is written, and whether it is written.
//
// The path is SLACKCLI_AUDIT_LOG when it names a file, else audit.jsonl in
// the configuration directory, next to credentials.json
// (appinfo.ConfigDir). enabled is false when the variable says "off" (or
// "0", "false", "no"), in which case path is the default one, and in a
// test binary when the variable is unset.
func Path() (path string, enabled bool) {
	_, raw := appinfo.EnvSource(EnvSuffix)
	switch {
	case isOff(raw):
		return defaultPath(), false
	case raw != "":
		if abs, err := filepath.Abs(raw); err == nil {
			return abs, true
		}
		return raw, true
	}
	return defaultPath(), !testing.Testing()
}

func defaultPath() string { return filepath.Join(appinfo.ConfigDir(), FileName) }

func isOff(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "off", "0", "false", "no":
		return true
	}
	return false
}

// process is what every line says about this process; it is the same for
// the whole run, except the command, which is known once the command line
// is parsed.
var process struct {
	mu   sync.Mutex
	cmd  string
	args []string

	once      sync.Once
	parent    string
	ancestors []string
}

// SetCommand records the command that runs ("message list") and its
// command line, which is kept as SanitizeArgs makes it.
func SetCommand(cmd string, args []string) {
	safe := SanitizeArgs(cmd, args)
	process.mu.Lock()
	process.cmd, process.args = cmd, safe
	process.mu.Unlock()
}

// Reset forgets the command set by SetCommand (tests).
func Reset() {
	process.mu.Lock()
	process.cmd, process.args = "", nil
	process.mu.Unlock()
}

// Record appends e to the log, after filling in the time (when unset), the
// process and the command. It never fails: a line that cannot be written
// is dropped.
func Record(e Entry) {
	path, enabled := Path()
	if !enabled {
		return
	}
	if e.TS == "" {
		e.TS = time.Now().UTC().Format(time.RFC3339Nano)
	}
	e.PID, e.PPID = os.Getpid(), os.Getppid()
	process.once.Do(func() {
		names := ancestry(e.PPID, maxAncestors)
		if len(names) > 0 {
			process.parent, process.ancestors = names[0], names[1:]
		}
	})
	e.Parent, e.Ancestors = process.parent, process.ancestors
	process.mu.Lock()
	e.Cmd, e.Args = process.cmd, process.args
	process.mu.Unlock()

	line, err := json.Marshal(e)
	if err != nil {
		reportOnce(err)
		return
	}
	if err := appendLine(path, append(line, '\n')); err != nil {
		reportOnce(err)
	}
}

// maxAncestors is how many processes up from slackcli are named.
const maxAncestors = 5

var reported sync.Once

// reportOnce says in the live trace, once per process, that the log
// could not be written.
func reportOnce(err error) {
	reported.Do(func() {
		trace.Logf("audit log not written: %s", trace.SafeNote(err.Error()))
	})
}

// appendLine writes line at the end of the file at path in one write,
// creating the file (0600) and its directory (0700) when needed, and
// rotating the file first when it is full.
func appendLine(path string, line []byte) error {
	f, err := openAppend(path)
	if err != nil {
		return err
	}
	if info, err := f.Stat(); err == nil && info.Size()+int64(len(line)) > maxSize {
		// rotate closes f; the line goes to the file that replaces it.
		if err := rotate(f, info, path); err != nil {
			return err
		}
		if f, err = openAppend(path); err != nil {
			return err
		}
	}
	_, werr := f.Write(line)
	cerr := f.Close()
	if werr != nil {
		return werr
	}
	return cerr
}

func openAppend(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil && os.IsNotExist(err) {
		if mkErr := os.MkdirAll(filepath.Dir(path), 0o700); mkErr != nil {
			return nil, mkErr
		}
		f, err = os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	}
	return f, err
}

// rotatedPath is where a full log goes.
func rotatedPath(path string) string { return path + ".1" }

// renameIfSame moves the file at path to <path>.1, unless it is no longer
// the file f is open on (another process rotated it first) or no longer
// full.
func renameIfSame(info os.FileInfo, path string) error {
	now, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if !os.SameFile(info, now) || now.Size() < maxSize/2 {
		return nil
	}
	return os.Rename(path, rotatedPath(path))
}

// Paths returns the files that hold the log, oldest first: the rotated
// one and the current one, those that exist.
func Paths(path string) []string {
	var out []string
	for _, p := range []string{rotatedPath(path), path} {
		if info, err := os.Stat(p); err == nil && info.Mode().IsRegular() {
			out = append(out, p)
		}
	}
	return out
}

var slackSecretRe = regexp.MustCompile(`xox[a-z]-[A-Za-z0-9%._-]+`)

// RedactSecrets replaces anything that looks like a Slack token or "d"
// cookie (xoxb-, xoxp-, xoxc-, xoxd-…) with [REDACTED].
func RedactSecrets(s string) string {
	return slackSecretRe.ReplaceAllString(s, "[REDACTED]")
}

// Limits of the command line that is kept, so that a line stays short.
const (
	maxArgs   = 32
	maxArgLen = 200
)

// Placeholder stands for an argument that is not kept.
const Placeholder = "…"

var (
	// slackIDRe matches the ID of a channel, user, team, file…
	slackIDRe = regexp.MustCompile(`^[CDGUWTBFEAKLRSV][A-Z0-9]{6,20}$`)
	// tsRe matches a message timestamp.
	tsRe = regexp.MustCompile(`^\d{9,11}\.\d{1,6}$`)
	// intRe matches a number (a limit, a count).
	intRe = regexp.MustCompile(`^-?\d{1,10}$`)
)

// SanitizeArgs returns the command line args (without the program name)
// with only what is safe to keep: the words of cmd, the names of the flags
// (not their values after "="), and the arguments that are Slack IDs,
// message timestamps, numbers or Slack URLs (of which the scheme, host and
// path are kept). Every other argument, which may be a message, a search
// query, a name or a secret, is replaced by Placeholder. Anything that
// still looks like a token is redacted.
func SanitizeArgs(cmd string, args []string) []string {
	words := strings.Fields(cmd)
	out := make([]string, 0, min(len(args), maxArgs+1))
	for i, a := range args {
		if len(out) == maxArgs {
			out = append(out, Placeholder+"+"+itoa(len(args)-i))
			break
		}
		var kept string
		switch {
		case len(words) > 0 && a == words[0]:
			kept, words = a, words[1:]
		case strings.HasPrefix(a, "--"):
			kept, _, _ = strings.Cut(a, "=")
		case strings.HasPrefix(a, "-") && !intRe.MatchString(a):
			// -n5, -n=5: the flag is the first letter.
			kept = a[:min(len(a), 2)]
		default:
			kept = safeValue(a)
		}
		kept = RedactSecrets(kept)
		if len(kept) > maxArgLen {
			kept = kept[:maxArgLen] + Placeholder
		}
		out = append(out, kept)
	}
	return out
}

// safeValue returns a, or as much of it as is safe to keep, or
// Placeholder.
func safeValue(a string) string {
	switch {
	case slackIDRe.MatchString(a), tsRe.MatchString(a), intRe.MatchString(a):
		return a
	}
	if u, err := url.Parse(a); err == nil && (u.Scheme == "https" || u.Scheme == "http") && isSlackHost(u.Hostname()) {
		return u.Scheme + "://" + u.Host + safePath(u.Path)
	}
	return Placeholder
}

// pathWords are the segments of the path of a Slack URL that say what
// kind of thing it points at, and are kept as they are.
var pathWords = map[string]bool{
	"archives": true, "client": true, "docs": true, "team": true, "files": true,
	"files-pri": true, "canvas": true, "lists": true, "later": true, "messages": true,
}

// permalinkRe matches the message part of a permalink (p + timestamp).
var permalinkRe = regexp.MustCompile(`^p\d{10,20}$`)

// safePath keeps of the path of a Slack URL the segments that are IDs,
// permalink timestamps or pathWords; the others (a file name, a channel
// name) are replaced by Placeholder.
func safePath(p string) string {
	if p == "" {
		return ""
	}
	segs := strings.Split(p, "/")
	for i, s := range segs {
		if s == "" || pathWords[s] || slackIDRe.MatchString(s) || permalinkRe.MatchString(s) || allIDs(s) {
			continue
		}
		segs[i] = Placeholder
	}
	return strings.Join(segs, "/")
}

// allIDs reports whether s is IDs joined by "-" ("T0123ABCD-F0123ABCD",
// how a file URL names a file).
func allIDs(s string) bool {
	parts := strings.Split(s, "-")
	for _, p := range parts {
		if !slackIDRe.MatchString(p) {
			return false
		}
	}
	return len(parts) > 1
}

// isSlackHost reports whether host is owned by Slack, as
// slackapi.IsSlackHost does (which cannot be used here: slackapi imports
// this package).
func isSlackHost(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	for _, d := range []string{"slack.com", "slack-gov.com", "slack-edge.com", "slack-files.com"} {
		if host == d || strings.HasSuffix(host, "."+d) {
			return true
		}
	}
	return false
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}
