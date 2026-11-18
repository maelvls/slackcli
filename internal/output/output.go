// Package output renders command results as token-efficient JSON: null,
// blank strings, empty arrays and empty objects are pruned recursively, and
// object keys keep the order in which they were produced (struct field
// order; map keys sorted). The JSON is indented on a terminal and compact
// everywhere else (see Indented).
package output

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/mattn/go-isatty"
)

type member struct {
	key string
	val any
}

// object is a JSON object that remembers key order.
type object []member

// Marshal returns v as pruned, 2-space-indented JSON without a trailing
// newline. A value that prunes to nothing is rendered as {}.
func Marshal(v any) ([]byte, error) {
	compact, err := MarshalCompact(v)
	if err != nil {
		return nil, err
	}
	return Indent(compact)
}

// MarshalCompact is Marshal without the indentation: the same pruned JSON
// on one line, which is what a program (or a model, which pays for every
// character) wants to read.
func MarshalCompact(v any) ([]byte, error) {
	var raw bytes.Buffer
	enc := json.NewEncoder(&raw)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	dec := json.NewDecoder(&raw)
	dec.UseNumber()
	tree, err := decode(dec)
	if err != nil {
		return nil, err
	}
	pruned, keep := prune(tree)
	if !keep {
		return []byte("{}"), nil
	}
	var compact bytes.Buffer
	if err := encode(&compact, pruned); err != nil {
		return nil, err
	}
	return compact.Bytes(), nil
}

// Indent renders compact JSON the way Marshal does, with 2-space
// indentation.
func Indent(compact []byte) ([]byte, error) {
	var out bytes.Buffer
	if err := json.Indent(&out, compact, "", "  "); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// Indented reports whether results written to w are indented: they are
// when w is a terminal, where a person reads them. Anything else (a pipe,
// a file, the output captured by an agent) gets one line of compact JSON,
// which is 10 to 30% shorter.
func Indented(w io.Writer) bool {
	f, ok := w.(interface{ Fd() uintptr })
	return ok && (isatty.IsTerminal(f.Fd()) || isatty.IsCygwinTerminal(f.Fd()))
}

// Format returns v as pruned JSON for w, without a trailing newline:
// indented when Indented(w), compact otherwise.
func Format(w io.Writer, v any) ([]byte, error) {
	if Indented(w) {
		return Marshal(v)
	}
	return MarshalCompact(v)
}

// Write prints v to w as pruned JSON followed by a newline (see Format).
func Write(w io.Writer, v any) error {
	b, err := Format(w, v)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "%s\n", b)
	return err
}

func decode(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	d, ok := tok.(json.Delim)
	if !ok {
		return tok, nil
	}
	switch d {
	case '{':
		obj := object{}
		for dec.More() {
			kt, err := dec.Token()
			if err != nil {
				return nil, err
			}
			val, err := decode(dec)
			if err != nil {
				return nil, err
			}
			obj = append(obj, member{kt.(string), val})
		}
		_, err := dec.Token()
		return obj, err
	case '[':
		arr := []any{}
		for dec.More() {
			val, err := decode(dec)
			if err != nil {
				return nil, err
			}
			arr = append(arr, val)
		}
		_, err := dec.Token()
		return arr, err
	}
	return nil, fmt.Errorf("output: unexpected delimiter %q", d)
}

func prune(v any) (any, bool) {
	switch x := v.(type) {
	case nil:
		return nil, false
	case string:
		return x, strings.TrimSpace(x) != ""
	case []any:
		out := make([]any, 0, len(x))
		for _, e := range x {
			if p, keep := prune(e); keep {
				out = append(out, p)
			}
		}
		return out, len(out) > 0
	case object:
		out := make(object, 0, len(x))
		for _, m := range x {
			if p, keep := prune(m.val); keep {
				out = append(out, member{m.key, p})
			}
		}
		return out, len(out) > 0
	}
	return v, true
}

func encode(buf *bytes.Buffer, v any) error {
	switch x := v.(type) {
	case object:
		buf.WriteByte('{')
		for i, m := range x {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := encodeScalar(buf, m.key); err != nil {
				return err
			}
			buf.WriteByte(':')
			if err := encode(buf, m.val); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
		return nil
	case []any:
		buf.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := encode(buf, e); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
		return nil
	}
	return encodeScalar(buf, v)
}

func encodeScalar(buf *bytes.Buffer, v any) error {
	enc := json.NewEncoder(buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return err
	}
	buf.Truncate(buf.Len() - 1) // drop Encode's trailing newline
	return nil
}
