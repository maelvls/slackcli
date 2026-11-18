// Package jsonx has small helpers for walking loosely-typed JSON
// (map[string]any / []any) as returned by the Slack API.
package jsonx

import (
	"encoding/json"
	"math"
	"strconv"
)

// Map returns v as a JSON object, or nil if it is not one.
func Map(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

// Slice returns v as a JSON array, or nil if it is not one.
func Slice(v any) []any {
	s, _ := v.([]any)
	return s
}

// Maps returns the JSON objects contained in array v, skipping other values.
func Maps(v any) []map[string]any {
	var out []map[string]any
	for _, e := range Slice(v) {
		if m := Map(e); m != nil {
			out = append(out, m)
		}
	}
	return out
}

// Str returns v if it is a string, else "".
func Str(v any) string {
	s, _ := v.(string)
	return s
}

// Bool returns v if it is a bool, else false.
func Bool(v any) bool {
	b, _ := v.(bool)
	return b
}

// Num returns v as a float64 if it is a JSON number.
func Num(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case int32:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	}
	return 0, false
}

// Int64 returns v as an int64 if it is a finite JSON number, truncating
// towards zero. A number beyond the int64 range saturates to
// math.MaxInt64 or math.MinInt64; NaN and the infinities, which JSON
// cannot express, are not numbers (0, false).
//
// Use it rather than converting the result of Num: Go leaves the
// conversion of an out-of-range float to an integer up to the
// architecture (arm64 saturates, amd64 yields math.MinInt64).
func Int64(v any) (int64, bool) {
	// Integers are returned as they are: a float64 only holds them
	// exactly up to 2^53.
	switch n := v.(type) {
	case int:
		return int64(n), true
	case int64:
		return n, true
	case int32:
		return int64(n), true
	case json.Number:
		if i, err := n.Int64(); err == nil {
			return i, true
		}
	}
	f, ok := Num(v)
	if !ok || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false
	}
	// 2^63 is the smallest float64 above math.MaxInt64 (which a float64
	// cannot hold), and -2^63 is math.MinInt64 exactly.
	const limit = 1 << 63
	switch {
	case f >= limit:
		return math.MaxInt64, true
	case f <= -limit:
		return math.MinInt64, true
	}
	return int64(f), true
}

// Int is Int64 for an int: a number beyond the range of int (which is 32
// bits wide on some platforms) saturates to math.MaxInt or math.MinInt.
func Int(v any) (int, bool) {
	n, ok := Int64(v)
	if !ok {
		return 0, false
	}
	return int(min(max(n, math.MinInt), math.MaxInt)), true
}

// Get walks nested objects: Get(resp, "channel", "id").
func Get(v any, path ...string) any {
	for _, k := range path {
		m := Map(v)
		if m == nil {
			return nil
		}
		v = m[k]
	}
	return v
}

// GetStr is Str(Get(v, path...)).
func GetStr(v any, path ...string) string { return Str(Get(v, path...)) }

// GetMap is Map(Get(v, path...)).
func GetMap(v any, path ...string) map[string]any { return Map(Get(v, path...)) }

// GetSlice is Slice(Get(v, path...)).
func GetSlice(v any, path ...string) []any { return Slice(Get(v, path...)) }

// Stringish renders a string or number as a string ("" otherwise). Slack
// sometimes returns timestamps and counts as either.
func Stringish(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case json.Number:
		return x.String()
	}
	if f, ok := Num(v); ok {
		return strconv.FormatFloat(f, 'f', -1, 64)
	}
	return ""
}
