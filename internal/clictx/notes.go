package clictx

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/maelvls/slackcli/internal/output"
)

// The warnings and hints of a command are part of its result: they are
// printed in the JSON object on stdout, as "warnings" and "hints", rather
// than on stderr. A consumer that merges the two streams (`2>&1 | jq`,
// which agents do all the time) then still gets JSON, and one that drops
// stderr still gets the warnings. Stderr only gets what no JSON object
// carried: the notes of a command that failed, or that printed text.

// Keys of the notes in a printed result.
const (
	WarningsKey = "warnings"
	HintsKey    = "hints"
)

// Warnf records a warning about the result: something that went wrong
// without failing the command. It is printed in the "warnings" array of
// the next result (Print, PrintCompact), or by FlushNotes on stderr, as
// "Warning: …", if no result follows. The message does not start with
// "Warning:".
func (c *Context) Warnf(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	c.mu.Lock()
	defer c.mu.Unlock()
	// A command retried after a credentials refresh says things twice.
	if !slices.Contains(c.warnings, msg) {
		c.warnings = append(c.warnings, msg)
	}
}

// ResultHintf records advice about the result (why it is empty, how to
// get the rest of it). It is printed in the "hints" array of the next
// result, or by FlushNotes on stderr if no result follows. Use Hintf for
// what a human has to read right now.
func (c *Context) ResultHintf(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	c.mu.Lock()
	defer c.mu.Unlock()
	if !slices.Contains(c.hints, msg) {
		c.hints = append(c.hints, msg)
	}
}

// takeNotes returns the warnings and hints not printed yet, those of
// Incomplete included, and forgets them.
func (c *Context) takeNotes() (warnings, hints []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	warnings, hints = c.warnings, c.hints
	c.warnings, c.hints = nil, nil
	if c.Incomplete != nil {
		list := c.Incomplete.List()
		if c.incompleteSeen < len(list) {
			warnings = append(warnings, list[c.incompleteSeen:]...)
		}
		c.incompleteSeen = len(list)
	}
	return warnings, hints
}

// FlushNotes writes to Stderr the warnings and hints that no result
// carried. The root command calls it when the command is over.
func (c *Context) FlushNotes() {
	warnings, hints := c.takeNotes()
	for _, w := range warnings {
		c.Hintf("%s", "Warning: "+w)
	}
	for _, h := range hints {
		c.Hintf("%s", h)
	}
}

// PrintCompact writes a result that is already compact JSON (see
// output.MarshalCompact) to Stdout: with the pending warnings and hints
// added when it is an object, and indented on a terminal.
func (c *Context) PrintCompact(compact []byte) error {
	warnings, hints := c.takeNotes()
	// keep puts the notes back, for FlushNotes to write on stderr.
	keep := func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.warnings = append(warnings, c.warnings...)
		c.hints = append(hints, c.hints...)
	}
	out, ok := withNotes(compact, warnings, hints)
	if !ok {
		// Not something notes can be added to.
		keep()
	}
	if output.Indented(c.Stdout) {
		var err error
		if out, err = output.Indent(out); err != nil {
			if ok {
				keep()
			}
			return err
		}
	}
	_, err := fmt.Fprintf(c.Stdout, "%s\n", out)
	if err != nil && ok {
		// The result did not reach stdout (a closed pipe, a full disk):
		// its notes must not be lost with it. One of them may be the only
		// trace that a message was sent.
		keep()
	}
	return err
}

// withNotes returns the compact JSON object with "warnings" and "hints"
// appended. ok is false, and compact is returned as it is, when there are
// notes and compact is not an object or already has one of those keys.
func withNotes(compact []byte, warnings, hints []string) (out []byte, ok bool) {
	if len(warnings) == 0 && len(hints) == 0 {
		return compact, true
	}
	keys, isObject := topLevelKeys(compact)
	if !isObject || slices.Contains(keys, WarningsKey) || slices.Contains(keys, HintsKey) {
		return compact, false
	}
	var buf bytes.Buffer
	buf.Write(bytes.TrimSuffix(compact, []byte("}")))
	sep := ","
	if len(keys) == 0 {
		sep = ""
	}
	for _, notes := range []struct {
		key  string
		list []string
	}{{WarningsKey, warnings}, {HintsKey, hints}} {
		if len(notes.list) == 0 {
			continue
		}
		// output.MarshalCompact rather than json.Marshal: it leaves <, >
		// and & alone, like the rest of the result.
		list, err := output.MarshalCompact(notes.list)
		if err != nil {
			return compact, false
		}
		fmt.Fprintf(&buf, "%s%q:%s", sep, notes.key, list)
		sep = ","
	}
	buf.WriteString("}")
	return buf.Bytes(), true
}

// topLevelKeys lists the keys of a JSON object; isObject is false when
// data is not one.
func topLevelKeys(data []byte) (keys []string, isObject bool) {
	dec := json.NewDecoder(bytes.NewReader(data))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil, false
	}
	for dec.More() {
		tok, err := dec.Token()
		key, isString := tok.(string)
		if err != nil || !isString {
			return nil, false
		}
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			return nil, false
		}
		keys = append(keys, key)
	}
	return keys, true
}
