package audit

import (
	"os"
	"strings"
	"testing"
)

// A line too long for what is left of a file under half the limit is
// appended to it, not lost: such a file is not rotated.
func TestLongLineIsAppendedToASmallLog(t *testing.T) {
	path := useLog(t)
	old := maxSize
	maxSize = 1000
	t.Cleanup(func() { maxSize = old })

	Record(Entry{Kind: KindAPI, Method: "m"}) // a few hundred bytes, under 500
	if info, err := os.Stat(path); err != nil || info.Size() >= maxSize/2 {
		t.Fatalf("the first line is too long for this test: %v %v", info, err)
	}
	big := strings.Repeat("x", 900)
	Record(Entry{Kind: KindAPI, Method: big})
	lines := readLines(t, path)
	if len(lines) != 2 || lines[1].Method != big {
		t.Fatalf("%d lines, want 2 with the long one last", len(lines))
	}
	if _, err := os.Stat(rotatedPath(path)); !os.IsNotExist(err) {
		t.Errorf("the log was rotated: %v", err)
	}
}

// A line longer than the limit is written even to an empty log.
func TestLineLongerThanTheLimitIsWritten(t *testing.T) {
	path := useLog(t)
	old := maxSize
	maxSize = 1000
	t.Cleanup(func() { maxSize = old })

	Record(Entry{Kind: KindAPI, Method: strings.Repeat("y", 2000)})
	if n := len(readLines(t, path)); n != 1 {
		t.Fatalf("%d lines, want 1", n)
	}
}

// An error code is cut short: it comes from the answer.
func TestLongErrorIsCut(t *testing.T) {
	path := useLog(t)
	Record(Entry{Kind: KindAPI, Method: "m", Error: strings.Repeat("e", 5000)})
	lines := readLines(t, path)
	if len(lines) != 1 || len(lines[0].Error) != maxErrorLen+len(Placeholder) {
		t.Fatalf("lines = %d, error length %d", len(lines), len(lines[0].Error))
	}
}
