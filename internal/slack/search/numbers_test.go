package search

import (
	"math"
	"testing"
)

// What an out-of-range float converts to depends on the architecture: the
// upload date of a file hit must be the same number everywhere.
func TestParseFileHitCreatedSaturates(t *testing.T) {
	for _, tt := range []struct {
		file map[string]any
		want int64
	}{
		{map[string]any{"id": "F1", "timestamp": 1700000000.0}, 1700000000},
		{map[string]any{"id": "F1", "created": 1700000001.0}, 1700000001},
		{map[string]any{"id": "F1", "timestamp": "soon", "created": 1700000001.0}, 1700000001},
		{map[string]any{"id": "F1", "timestamp": 1e300}, math.MaxInt64},
		{map[string]any{"id": "F1", "created": -1e300}, math.MinInt64},
		{map[string]any{"id": "F1"}, 0},
	} {
		hit, ok := parseFileHit(tt.file)
		if !ok || hit.created != tt.want {
			t.Errorf("parseFileHit(%v).created = %d (ok=%v), want %d", tt.file, hit.created, ok, tt.want)
		}
	}
}
