package jsonx

import (
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"testing"
)

// decode parses JSON the way the Slack client does: numbers are float64.
func decode(t *testing.T, s string) any {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatalf("decoding %s: %v", s, err)
	}
	return v
}

func TestMapSliceMaps(t *testing.T) {
	v := decode(t, `{"a": {"b": 1}, "list": [{"x": 1}, "skipped", null, 2, {"y": 2}]}`)

	if got := Map(v); len(got) != 2 {
		t.Errorf("Map(object) = %v", got)
	}
	if got := Map(Map(v)["a"]); !reflect.DeepEqual(got, map[string]any{"b": 1.0}) {
		t.Errorf("Map(nested) = %v", got)
	}
	for _, notObject := range []any{nil, "s", 1.0, true, []any{}} {
		if got := Map(notObject); got != nil {
			t.Errorf("Map(%#v) = %v, want nil", notObject, got)
		}
	}

	if got := Slice(Map(v)["list"]); len(got) != 5 {
		t.Errorf("Slice(array) = %v", got)
	}
	for _, notArray := range []any{nil, "s", 1.0, map[string]any{}} {
		if got := Slice(notArray); got != nil {
			t.Errorf("Slice(%#v) = %v, want nil", notArray, got)
		}
	}

	want := []map[string]any{{"x": 1.0}, {"y": 2.0}}
	if got := Maps(Map(v)["list"]); !reflect.DeepEqual(got, want) {
		t.Errorf("Maps = %v, want %v", got, want)
	}
	for _, noObjects := range []any{nil, "s", []any{}, []any{"a", 1.0}, map[string]any{"a": map[string]any{}}} {
		if got := Maps(noObjects); got != nil {
			t.Errorf("Maps(%#v) = %v, want nil", noObjects, got)
		}
	}
}

func TestStrBool(t *testing.T) {
	if got := Str("hello"); got != "hello" {
		t.Errorf("Str = %q", got)
	}
	for _, notString := range []any{nil, 1.0, true, json.Number("1"), []any{"a"}} {
		if got := Str(notString); got != "" {
			t.Errorf("Str(%#v) = %q, want \"\"", notString, got)
		}
	}
	if !Bool(true) || Bool(false) {
		t.Error("Bool does not return the bool it is given")
	}
	for _, notBool := range []any{nil, "true", 1.0} {
		if Bool(notBool) {
			t.Errorf("Bool(%#v) = true", notBool)
		}
	}
}

func TestNum(t *testing.T) {
	for _, tc := range []struct {
		in   any
		want float64
	}{
		{1.5, 1.5},
		{float32(0.5), 0.5},
		{int(-3), -3},
		{int64(1 << 40), 1 << 40},
		{int32(7), 7},
		{json.Number("12.25"), 12.25},
		{json.Number("1e3"), 1000},
		{decode(t, `1700000000.000100`), 1700000000.0001},
	} {
		if got, ok := Num(tc.in); !ok || got != tc.want {
			t.Errorf("Num(%#v) = %v, %v; want %v, true", tc.in, got, ok, tc.want)
		}
	}
	for _, notNumber := range []any{nil, "1", true, json.Number("abc"), json.Number(""), []any{1.0}, uint(1)} {
		if got, ok := Num(notNumber); ok || got != 0 {
			t.Errorf("Num(%#v) = %v, %v; want 0, false", notNumber, got, ok)
		}
	}
}

// intCases are shared by Int64 and Int, whose results only differ where
// int is narrower than int64.
var intCases = []struct {
	name string
	in   any
	want int64
}{
	{"zero", 0.0, 0},
	{"integer", 42.0, 42},
	{"negative", -42.0, -42},
	{"truncates towards zero", 1.9, 1},
	{"truncates a negative towards zero", -1.9, -1},
	{"float32", float32(3.5), 3},
	{"int", int(-7), -7},
	{"int32", int32(math.MinInt32), math.MinInt32},
	{"int64", int64(math.MaxInt32), math.MaxInt32},
	{"json.Number integer", json.Number("123"), 123},
	{"json.Number fraction", json.Number("123.9"), 123},
	{"json.Number exponent", json.Number("1e3"), 1000},
}

// int64Cases need all 64 bits.
var int64Cases = []struct {
	name string
	in   any
	want int64
}{
	// float64 holds integers exactly up to 2^53.
	{"2^53", float64(1 << 53), 1 << 53},
	{"largest float64 below 2^63", math.Nextafter(1<<63, 0), 1<<63 - 1024},
	{"smallest float64 above -2^63", math.Nextafter(-(1 << 63), 0), -(1 << 63) + 1024},

	// The values on which architectures disagree: they saturate.
	{"2^63", float64(1 << 63), math.MaxInt64},
	{"9e18 fits", 9e18, 9000000000000000000},
	{"1e19", 1e19, math.MaxInt64},
	{"MaxFloat64", math.MaxFloat64, math.MaxInt64},
	{"-2^63", -float64(1 << 63), math.MinInt64},
	{"below -2^63", math.Nextafter(-(1 << 63), math.Inf(-1)), math.MinInt64},
	{"-1e19", -1e19, math.MinInt64},
	{"-MaxFloat64", -math.MaxFloat64, math.MinInt64},

	// Integers do not go through float64, which would round them.
	{"int64 beyond 2^53", int64(1<<53 + 1), 1<<53 + 1},
	{"MaxInt64", int64(math.MaxInt64), math.MaxInt64},
	{"MinInt64", int64(math.MinInt64), math.MinInt64},
	{"json.Number beyond 2^53", json.Number("9007199254740993"), 9007199254740993},
	{"json.Number MaxInt64", json.Number("9223372036854775807"), math.MaxInt64},
	{"json.Number above MaxInt64", json.Number("9223372036854775808"), math.MaxInt64},
	{"json.Number below MinInt64", json.Number("-9223372036854775809"), math.MinInt64},
	{"json.Number 1e30", json.Number("1e30"), math.MaxInt64},
}

// notIntegers are refused by Int64 and Int.
var notIntegers = []any{
	nil, "1", true, []any{1.0}, map[string]any{}, uint(1),
	math.NaN(), math.Inf(1), math.Inf(-1), float32(math.Inf(1)),
	json.Number("abc"), json.Number(""),
	json.Number("1e999"), // out of float64's range
}

func TestInt64(t *testing.T) {
	for _, tc := range append(intCases, int64Cases...) {
		if got, ok := Int64(tc.in); !ok || got != tc.want {
			t.Errorf("%s: Int64(%#v) = %d, %v; want %d, true", tc.name, tc.in, got, ok, tc.want)
		}
	}
	for _, in := range notIntegers {
		if got, ok := Int64(in); ok || got != 0 {
			t.Errorf("Int64(%#v) = %d, %v; want 0, false", in, got, ok)
		}
	}
}

func TestInt(t *testing.T) {
	// Where int is 32 bits wide, what does not fit saturates.
	clamp := func(n int64) int {
		return int(min(max(n, math.MinInt), math.MaxInt))
	}
	for _, tc := range append(intCases, int64Cases...) {
		if got, ok := Int(tc.in); !ok || got != clamp(tc.want) {
			t.Errorf("%s: Int(%#v) = %d, %v; want %d, true", tc.name, tc.in, got, ok, clamp(tc.want))
		}
	}
	for _, in := range notIntegers {
		if got, ok := Int(in); ok || got != 0 {
			t.Errorf("Int(%#v) = %d, %v; want 0, false", in, got, ok)
		}
	}

	// Whatever the width of int and the architecture, a number beyond its
	// range never wraps around to the other sign.
	for _, tc := range []struct {
		in   any
		want int
	}{
		{1e300, math.MaxInt},
		{-1e300, math.MinInt},
		{float64(1 << 63), math.MaxInt},
		{-float64(1 << 63), math.MinInt},
		{int64(math.MaxInt64), math.MaxInt},
		{int64(math.MinInt64), math.MinInt},
	} {
		if got, ok := Int(tc.in); !ok || got != tc.want {
			t.Errorf("Int(%#v) = %d, %v; want %d, true", tc.in, got, ok, tc.want)
		}
	}
}

// TestIntFromSlackJSON pins the case that differed between amd64 and
// arm64: sizes read from a Slack payload that are far out of range.
func TestIntFromSlackJSON(t *testing.T) {
	v := decode(t, `{"indent": 9000000000000000000, "offset": 9223372036854775807, "huge": 1e40, "low": -1e40}`)
	for key, want := range map[string]int64{
		"indent": 9000000000000000000,
		"offset": math.MaxInt64, // decoded as 2^63, one more than MaxInt64
		"huge":   math.MaxInt64,
		"low":    math.MinInt64,
	} {
		if got, ok := Int64(Get(v, key)); !ok || got != want {
			t.Errorf("Int64(%s) = %d, %v; want %d, true", key, got, ok, want)
		}
		if got, ok := Int(Get(v, key)); !ok || (got > 0) != (want > 0) {
			t.Errorf("Int(%s) = %d, %v: the sign changed", key, got, ok)
		}
	}
}

func TestGet(t *testing.T) {
	v := decode(t, `{"channel": {"id": "C1", "topic": {"value": "hi"}, "members": ["U1", "U2"], "n": 3}, "ok": true}`)

	if got := Get(v); !reflect.DeepEqual(got, v) {
		t.Errorf("Get with no path = %v, want the value itself", got)
	}
	if got := Get(v, "channel", "topic", "value"); got != "hi" {
		t.Errorf("Get(channel, topic, value) = %v", got)
	}
	if got := Get(v, "ok"); got != true {
		t.Errorf("Get(ok) = %v", got)
	}
	for _, path := range [][]string{
		{"missing"},
		{"channel", "missing"},
		{"channel", "id", "deeper"},  // through a string
		{"channel", "members", "0"},  // through an array
		{"ok", "deeper"},             // through a bool
		{"missing", "deeper", "yet"}, // through nothing
	} {
		if got := Get(v, path...); got != nil {
			t.Errorf("Get(%s) = %v, want nil", strings.Join(path, ", "), got)
		}
	}
	if got := Get(nil, "a"); got != nil {
		t.Errorf("Get(nil, a) = %v", got)
	}

	if got := GetStr(v, "channel", "id"); got != "C1" {
		t.Errorf("GetStr = %q", got)
	}
	if got := GetStr(v, "channel", "n"); got != "" {
		t.Errorf("GetStr(number) = %q, want \"\"", got)
	}
	if got := GetMap(v, "channel", "topic"); !reflect.DeepEqual(got, map[string]any{"value": "hi"}) {
		t.Errorf("GetMap = %v", got)
	}
	if got := GetMap(v, "channel", "id"); got != nil {
		t.Errorf("GetMap(string) = %v, want nil", got)
	}
	if got := GetSlice(v, "channel", "members"); !reflect.DeepEqual(got, []any{"U1", "U2"}) {
		t.Errorf("GetSlice = %v", got)
	}
	if got := GetSlice(v, "channel", "topic"); got != nil {
		t.Errorf("GetSlice(object) = %v, want nil", got)
	}
}

func TestStringish(t *testing.T) {
	for _, tc := range []struct {
		in   any
		want string
	}{
		{"1700000000.000100", "1700000000.000100"},
		{"", ""},
		{json.Number("1700000000.000100"), "1700000000.000100"}, // kept verbatim
		{1700000000.0, "1700000000"},                            // no exponent
		{1.5, "1.5"},
		{-2.0, "-2"},
		{1e21, "1000000000000000000000"},
		{int(3), "3"},
		{int64(-4), "-4"},
		{int32(5), "5"},
		{float32(0.5), "0.5"},
		{nil, ""},
		{true, ""},
		{[]any{"a"}, ""},
		{map[string]any{"a": 1.0}, ""},
	} {
		if got := Stringish(tc.in); got != tc.want {
			t.Errorf("Stringish(%#v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
