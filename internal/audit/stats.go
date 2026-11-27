package audit

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// maxLineLen bounds the lines that are read back; a longer one is not
// one this package wrote, and is skipped.
const maxLineLen = 64 << 10

// Read returns the entries of the log at path (its rotated part first)
// whose time is at or after since (all of them when since is zero), in
// the order they were written, and how many lines could not be read.
// A log that does not exist has no entries.
func Read(path string, since time.Time) (entries []Entry, skipped int, err error) {
	for _, p := range Paths(path) {
		f, err := os.Open(p)
		if err != nil {
			return nil, 0, err
		}
		n, err := readEntries(f, since, &entries)
		f.Close()
		if err != nil {
			return nil, 0, fmt.Errorf("reading %s: %w", p, err)
		}
		skipped += n
	}
	return entries, skipped, nil
}

func readEntries(r io.Reader, since time.Time, out *[]Entry) (skipped int, err error) {
	br := bufio.NewReaderSize(r, 64<<10)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			if ok := decodeLine(line, since, out); !ok {
				skipped++
			}
		}
		if errors.Is(err, io.EOF) {
			return skipped, nil
		}
		if err != nil {
			return skipped, err
		}
	}
}

// decodeLine appends the entry of line to out when it is at or after
// since; it returns false when the line is not an entry.
func decodeLine(line []byte, since time.Time, out *[]Entry) bool {
	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		return true
	}
	if len(line) > maxLineLen {
		return false
	}
	var e Entry
	if json.Unmarshal(line, &e) != nil {
		return false
	}
	t, err := time.Parse(time.RFC3339Nano, e.TS)
	if err != nil {
		return false
	}
	if !since.IsZero() && t.Before(since) {
		return true
	}
	*out = append(*out, e)
	return true
}

// Tail returns the last n lines of the log at path (its rotated part
// included when the current one has fewer), oldest first, as they are in
// the file.
func Tail(path string, n int) ([]string, error) {
	if n <= 0 {
		return nil, nil
	}
	paths := Paths(path)
	var lines []string
	for i := len(paths) - 1; i >= 0 && len(lines) < n; i-- {
		data, err := os.ReadFile(paths[i])
		if err != nil {
			return nil, err
		}
		var these []string
		for _, l := range strings.Split(string(data), "\n") {
			if l = strings.TrimSpace(l); l != "" {
				these = append(these, l)
			}
		}
		if want := n - len(lines); len(these) > want {
			these = these[len(these)-want:]
		}
		lines = append(these, lines...)
	}
	return lines, nil
}

// Count is a number of requests for one key.
type Count struct {
	Key   string `json:"key"`
	Count int    `json:"count"`
	// Errors counts the requests of Count that failed (see Entry.Failed).
	Errors int `json:"errors"`
}

// Source is a number of requests made by one command run by one program.
type Source struct {
	// Program is the process that ran slackcli, looking through the
	// shells and wrappers in between (see Program).
	Program string `json:"program"`
	Cmd     string `json:"cmd"`
	Count   int    `json:"count"`
	// Processes is how many slackcli processes those were.
	Processes int `json:"processes"`
}

// Bucket is a number of requests in the hour (or minute) that starts at
// Start.
type Bucket struct {
	Start string `json:"start"`
	Count int    `json:"count"`
}

// Stats sums up a set of entries.
type Stats struct {
	// From and To are the times of the first and last entries.
	From string `json:"from"`
	To   string `json:"to"`
	// Total is the number of requests; Processes the number of slackcli
	// processes that made them.
	Total     int `json:"total"`
	Processes int `json:"processes"`
	// Errors counts the requests that failed, RateLimited those Slack
	// answered with HTTP 429.
	Errors      int `json:"errors"`
	RateLimited int `json:"rate_limited"`
	// PeakMinute is the most requests made in any 60 seconds, and when
	// that window started.
	PeakMinute Bucket `json:"peak_minute"`
	// ByMethod, ByError, ByHost and BySource are sorted by count, highest
	// first; ByHour by time.
	ByMethod []Count  `json:"by_method"`
	BySource []Source `json:"by_source"`
	ByError  []Count  `json:"by_error"`
	ByHost   []Count  `json:"by_host"`
	ByHour   []Bucket `json:"by_hour"`
}

// Summarize computes the Stats of entries, which are taken to be in the
// order they were written (Read). Entries whose time cannot be parsed are
// counted but take no part in the time buckets.
func Summarize(entries []Entry) Stats {
	var s Stats
	type key struct{ program, cmd string }
	methods := map[string]*Count{}
	errs := map[string]*Count{}
	hosts := map[string]*Count{}
	sources := map[key]*Source{}
	sourcePIDs := map[key]map[int]bool{}
	pids := map[int]bool{}
	hours := map[string]int{}
	var times []time.Time

	bump := func(m map[string]*Count, k string, failed bool) {
		c := m[k]
		if c == nil {
			c = &Count{Key: k}
			m[k] = c
		}
		c.Count++
		if failed {
			c.Errors++
		}
	}
	for _, e := range entries {
		s.Total++
		failed := e.Failed()
		if failed {
			s.Errors++
		}
		if e.RateLimited {
			s.RateLimited++
		}
		pids[e.PID] = true
		bump(methods, e.Method, failed)
		bump(hosts, e.Host, failed)
		if code := errorCode(e); code != "" {
			bump(errs, code, true)
		}
		k := key{Program(e), e.Cmd}
		src := sources[k]
		if src == nil {
			src = &Source{Program: k.program, Cmd: e.Cmd}
			sources[k] = src
			sourcePIDs[k] = map[int]bool{}
		}
		src.Count++
		sourcePIDs[k][e.PID] = true

		t, err := time.Parse(time.RFC3339Nano, e.TS)
		if err != nil {
			continue
		}
		t = t.UTC()
		times = append(times, t)
		hours[t.Truncate(time.Hour).Format(time.RFC3339)]++
	}
	s.Processes = len(pids)

	sort.Slice(times, func(i, j int) bool { return times[i].Before(times[j]) })
	if len(times) > 0 {
		s.From = times[0].Format(time.RFC3339Nano)
		s.To = times[len(times)-1].Format(time.RFC3339Nano)
	}
	s.PeakMinute = peak(times, time.Minute)

	s.ByMethod = sortedCounts(methods)
	s.ByError = sortedCounts(errs)
	s.ByHost = sortedCounts(hosts)
	for k, src := range sources {
		src.Processes = len(sourcePIDs[k])
		s.BySource = append(s.BySource, *src)
	}
	sort.Slice(s.BySource, func(i, j int) bool {
		a, b := s.BySource[i], s.BySource[j]
		if a.Count != b.Count {
			return a.Count > b.Count
		}
		if a.Program != b.Program {
			return a.Program < b.Program
		}
		return a.Cmd < b.Cmd
	})
	for h, n := range hours {
		s.ByHour = append(s.ByHour, Bucket{Start: h, Count: n})
	}
	sort.Slice(s.ByHour, func(i, j int) bool { return s.ByHour[i].Start < s.ByHour[j].Start })
	return s
}

// wrappers are the programs that only run another one: shells, and the
// commands that run a command under some condition. Program looks
// through them.
var wrappers = map[string]bool{
	"sh": true, "bash": true, "zsh": true, "dash": true, "fish": true, "ksh": true,
	"mksh": true, "ash": true, "csh": true, "tcsh": true, "busybox": true, "nu": true,
	"env": true, "timeout": true, "gtimeout": true, "nohup": true, "nice": true,
	"ionice": true, "time": true, "xargs": true, "sudo": true, "doas": true,
	"script": true, "stdbuf": true, "caffeinate": true, "flock": true, "setsid": true,
}

// Program is the process an entry counts under in Stats.BySource: the
// first of its parent and ancestors that is not a shell or a wrapper
// (sh, bash, zsh, env, timeout, nohup…), else the direct parent. A daemon
// that runs slackcli through "sh -c" is named, not "sh".
func Program(e Entry) string {
	for _, name := range append([]string{e.Parent}, e.Ancestors...) {
		// A login shell is named "-zsh".
		if base := strings.TrimPrefix(name, "-"); base != "" && !wrappers[base] {
			return name
		}
	}
	return e.Parent
}

// errorCode is what an entry is counted under in Stats.ByError: its error
// code, "ratelimited" for an HTTP 429, "http_<status>" for another failed
// status, "" for a success.
func errorCode(e Entry) string {
	switch {
	case e.Error != "":
		return e.Error
	case e.RateLimited:
		return "ratelimited"
	case e.Status >= 400:
		return "http_" + strconv.Itoa(e.Status)
	case e.OK != nil && !*e.OK:
		return "not_ok"
	}
	return ""
}

// peak returns the window of length w, starting at one of times (which
// are sorted), that holds the most of them.
func peak(times []time.Time, w time.Duration) Bucket {
	var best Bucket
	start := 0
	for end := range times {
		for times[end].Sub(times[start]) >= w {
			start++
		}
		if n := end - start + 1; n > best.Count {
			best = Bucket{Start: times[start].Format(time.RFC3339Nano), Count: n}
		}
	}
	return best
}

func sortedCounts(m map[string]*Count) []Count {
	out := make([]Count, 0, len(m))
	for _, c := range m {
		out = append(out, *c)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Key < out[j].Key
	})
	return out
}

// ParseSince parses the --since of "audit stats": a Go duration ("90m",
// "1h") or a number of days ("7d"), positive.
func ParseSince(raw string) (time.Duration, error) {
	raw = strings.TrimSpace(raw)
	var d time.Duration
	if days, ok := strings.CutSuffix(raw, "d"); ok {
		n, err := strconv.ParseFloat(days, 64)
		if err != nil || !(n > 0 && n <= 3650) {
			return 0, fmt.Errorf("invalid duration %q: expected e.g. 1h, 24h or 7d", raw)
		}
		d = time.Duration(n * float64(24*time.Hour))
	} else {
		var err error
		if d, err = time.ParseDuration(raw); err != nil {
			return 0, fmt.Errorf("invalid duration %q: expected e.g. 1h, 24h or 7d", raw)
		}
	}
	if d <= 0 {
		return 0, fmt.Errorf("invalid duration %q: it must be positive", raw)
	}
	return d, nil
}
