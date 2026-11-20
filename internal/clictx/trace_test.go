package clictx

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/maelvls/slackcli/internal/authextract"
	"github.com/maelvls/slackcli/internal/trace"
)

// Every run of an extractor is in the trace, whoever runs it ("auth
// import-…", the refresh of an expired session, the automatic lookup):
// the production context wraps all four.
func TestExtractorsAreTraced(t *testing.T) {
	// Registered first, so that it runs once the variable is back to
	// what it was: Reset reads it.
	t.Cleanup(func() { trace.Reset(nil) })
	t.Setenv("SLACKCLI_DEBUG", "1")
	var out bytes.Buffer
	trace.Reset(&out)

	want := &authextract.Extracted{CookieD: "xoxd-test"}
	got, err := traced("Slack Desktop", func(context.Context) (*authextract.Extracted, error) { return want, nil })(context.Background())
	if got != want || err != nil {
		t.Errorf("got %v, %v", got, err)
	}
	failure := errors.New(`reading "/Users/me/Library/Application Support/Slack/Cookies": locked`)
	if _, err := traced("Chrome", func(context.Context) (*authextract.Extracted, error) { return nil, failure })(context.Background()); err != failure {
		t.Errorf("err = %v", err)
	}
	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], "] credentials import Slack Desktop: ") || strings.Contains(lines[0], "failed") ||
		!strings.Contains(lines[1], "] credentials import Chrome: ") || !strings.HasSuffix(lines[1], `, failed: reading "…": locked`) {
		t.Errorf("trace:\n%s", out.String())
	}

	// A hung extraction is named by the message of a timeout.
	trace.Reset(&out)
	release, running := make(chan struct{}), make(chan struct{})
	go traced("Firefox", func(context.Context) (*authextract.Extracted, error) {
		close(running)
		<-release
		return nil, nil
	})(context.Background())
	<-running
	if got := trace.Summary(); !strings.Contains(got, "still running: credentials import Firefox for ") {
		t.Errorf("summary = %q", got)
	}
	close(release)

	c := New()
	if c.Desktop == nil || c.Chrome == nil || c.Brave == nil || c.Firefox == nil {
		t.Error("an extractor of the production context is missing")
	}
}
