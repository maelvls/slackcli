package later

import (
	"math"
	"testing"
)

// What an out-of-range float converts to depends on the architecture: a
// huge date used to read as 0 ("no date") on amd64 and as the largest
// int64 on arm64.
func TestUnixSecondsSaturates(t *testing.T) {
	for _, tt := range []struct {
		in   any
		want int64
	}{
		{1700000000.0, 1700000000},
		{1700000000.9, 1700000000},
		{int64(1700000000), 1700000000},
		{1e300, math.MaxInt64},
		{-1e300, 0},
		{-5.0, 0},
		{math.NaN(), 0},
		{"1700000000", 0},
		{nil, 0},
	} {
		if got := unixSeconds(tt.in); got != tt.want {
			t.Errorf("unixSeconds(%v) = %d, want %d", tt.in, got, tt.want)
		}
	}
}
