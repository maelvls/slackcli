// Package trace records where the time of a command goes: every Slack API
// call, rate-limit wait, download, keychain read and credentials import is
// a span with a name and a duration.
//
// The record is always kept, in memory, and costs next to nothing. It is
// used twice:
//
//   - when a command times out, the root says what it spent its time on
//     and what was still running (Summary), so that a transcript is
//     enough to tell a slow Slack from a locked keychain;
//   - with SLACKCLI_DEBUG=1 every span is also written to stderr as it
//     ends, with the time since the process started.
//
// A process runs one command, hence the package-level record. Tests that
// run several commands call Reset in between.
package trace

import (
	"fmt"
	"io"
	"net/url"
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/maelvls/slackcli/internal/appinfo"
)

// EnvSuffix names the variable that turns the live trace on
// (SLACKCLI_DEBUG).
const EnvSuffix = "DEBUG"

var rec = newRecorder()

type recorder struct {
	mu      sync.Mutex
	start   time.Time
	out     io.Writer
	live    bool
	totals  map[string]*total
	order   []string
	running []*Span
	// pending are the lines of the live trace that are not written yet,
	// in the order of the events (see emit).
	pending []string

	// writing is held while lines are written to out, and never together
	// with mu: a stderr that blocks (a full pipe nobody reads) must not
	// keep Summary, which the message of a timeout needs, from reading
	// the record.
	writing sync.Mutex
}

// queue adds a line to the live trace. It is called with mu held, which
// is what orders the lines; flush writes them.
func (r *recorder) queue(line string) { r.pending = append(r.pending, line) }

// flush writes the lines that are queued, one writer at a time and in
// the order they were queued in: whoever gets to write takes everything
// there is, its own line and those of the others.
func (r *recorder) flush() {
	r.writing.Lock()
	defer r.writing.Unlock()
	r.mu.Lock()
	lines, out := r.pending, r.out
	r.pending = nil
	r.mu.Unlock()
	for _, line := range lines {
		fmt.Fprintln(out, line)
	}
}

type total struct {
	count    int
	failed   int
	duration time.Duration
}

func newRecorder() *recorder {
	return &recorder{start: time.Now(), out: os.Stderr, live: Enabled(), totals: map[string]*total{}}
}

// Enabled reports whether the live trace is asked for (SLACKCLI_DEBUG set
// to anything but an off value).
func Enabled() bool { return appinfo.EnvOnUnlessOff(EnvSuffix) }

// Reset forgets everything recorded, restarts the clock and reads the
// environment again; spans are written to out (os.Stderr when nil).
func Reset(out io.Writer) {
	fresh := newRecorder()
	if out != nil {
		fresh.out = out
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	rec.start, rec.out, rec.live = fresh.start, fresh.out, fresh.live
	rec.totals, rec.order, rec.running, rec.pending = fresh.totals, nil, nil, nil
}

// Span is one timed operation.
type Span struct {
	name   string
	detail string
	start  time.Time
	done   bool
}

// Begin starts a span. name groups the spans of one kind in the summary
// ("api conversations.history", "keychain", "download"); detail says which
// one this is. Neither may hold a secret or something of the user's (a
// file name, a channel, a text): both are shown when a command times out,
// whether the live trace is on or not. End it with End, Fail or EndErr.
func Begin(name, detail string) *Span {
	s := &Span{name: name, detail: detail, start: time.Now()}
	rec.mu.Lock()
	rec.running = append(rec.running, s)
	rec.mu.Unlock()
	return s
}

// End closes the span as a success. note is appended to the trace line
// ("attempt 2", "HTTP 200").
func (s *Span) End(note string) { s.end(note, false) }

// Fail closes the span as a failure; note says how it failed.
func (s *Span) Fail(note string) { s.end(note, true) }

// EndErr closes the span as a failure when err is not nil. The note is
// the error made safe to show (see SafeNote).
func (s *Span) EndErr(err error) {
	if err != nil {
		s.Fail(SafeNote(err.Error()))
		return
	}
	s.End("")
}

func (s *Span) end(note string, failed bool) {
	if s == nil {
		return
	}
	d := time.Since(s.start)
	rec.mu.Lock()
	if s.done {
		rec.mu.Unlock()
		return
	}
	s.done = true
	rec.running = slices.DeleteFunc(rec.running, func(r *Span) bool { return r == s })
	t := rec.totals[s.name]
	if t == nil {
		t = &total{}
		rec.totals[s.name] = t
		rec.order = append(rec.order, s.name)
	}
	t.count++
	t.duration += d
	if failed {
		t.failed++
	}
	if !rec.live {
		rec.mu.Unlock()
		return
	}
	line := fmt.Sprintf("[%s +%s] %s", appinfo.Name, round(time.Since(rec.start)), s.name)
	if s.detail != "" {
		line += " " + s.detail
	}
	line += ": " + round(d).String()
	switch {
	case failed && note != "":
		line += ", failed: " + note
	case failed:
		line += ", failed"
	case note != "":
		line += ", " + note
	}
	rec.queue(line)
	rec.mu.Unlock()
	rec.flush()
}

// Logf writes one line to the live trace, for an event that has no
// duration. It does nothing unless the live trace is on.
func Logf(format string, args ...any) {
	rec.mu.Lock()
	if !rec.live {
		rec.mu.Unlock()
		return
	}
	rec.queue(fmt.Sprintf("[%s +%s] %s", appinfo.Name, round(time.Since(rec.start)), fmt.Sprintf(format, args...)))
	rec.mu.Unlock()
	rec.flush()
}

// Summary says, on one line, what the time went to since the process
// started: the kinds of spans by total duration, longest first, then what
// is still running. It is "" when nothing was recorded.
func Summary() string {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	names := slices.Clone(rec.order)
	sort.SliceStable(names, func(i, j int) bool { return rec.totals[names[i]].duration > rec.totals[names[j]].duration })
	var done []string
	for _, name := range names {
		t := rec.totals[name]
		part := fmt.Sprintf("%s %s", name, round(t.duration))
		switch {
		case t.count > 1 && t.failed > 0:
			part += fmt.Sprintf(" (%d calls, %d failed)", t.count, t.failed)
		case t.count > 1:
			part += fmt.Sprintf(" (%d calls)", t.count)
		case t.failed > 0:
			part += " (failed)"
		}
		done = append(done, part)
	}
	var running []string
	for _, s := range rec.running {
		part := s.name
		if s.detail != "" {
			part += " " + s.detail
		}
		running = append(running, fmt.Sprintf("%s for %s", part, round(time.Since(s.start))))
	}
	var parts []string
	if len(done) > 0 {
		parts = append(parts, "time spent: "+strings.Join(done, ", "))
	}
	if len(running) > 0 {
		parts = append(parts, "still running: "+strings.Join(running, ", "))
	}
	return strings.Join(parts, "; ")
}

// round keeps durations readable: milliseconds below a second, tenths of
// a second above.
func round(d time.Duration) time.Duration {
	if d < time.Second {
		return d.Round(time.Millisecond)
	}
	return d.Round(100 * time.Millisecond)
}

var (
	urlRe    = regexp.MustCompile(`[a-zA-Z][a-zA-Z0-9+.-]*://[^\s"'<>)]+`)
	quotedRe = regexp.MustCompile(`"(?:[^"\\]|\\.)*"?`)
	// pathRe matches the start of an absolute path (/…, ~/…, C:\…).
	pathRe = regexp.MustCompile(`(?:^|[\s(=])(~?/|[A-Za-z]:\\)`)
)

// SafeNote shortens the text of an error to what a trace may show. An
// error can quote what it was about, and a trace ends up in transcripts
// and bug reports: of a URL only the scheme and the host are kept (the
// path and the query can name a file or carry a signature), whatever is
// between double quotes is dropped (the first bytes of a response, a file
// name, an email address), so is the path of a file (its name is its
// author's), and so is everything after the first line or 120 characters.
func SafeNote(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	line = quotedRe.ReplaceAllString(line, `"…"`)
	line = urlRe.ReplaceAllStringFunc(line, func(raw string) string {
		if u, err := url.Parse(raw); err == nil && u.Host != "" {
			return u.Scheme + "://" + u.Host + "/…"
		}
		return "…"
	})
	// A path may hold spaces and colons, so where it ends cannot be told:
	// everything from its start to the last ": " goes, which leaves the
	// cause that the errors of the os package and of "…: %w" end with.
	if m := pathRe.FindStringSubmatchIndex(line); m != nil {
		start, rest := m[2], ""
		if i := strings.LastIndex(line[start:], ": "); i >= 0 {
			rest = line[start+i:]
		}
		line = line[:start] + "…" + rest
	}
	const max = 120
	if r := []rune(line); len(r) > max {
		line = string(r[:max]) + "…"
	}
	return line
}
