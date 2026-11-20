package files

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/maelvls/slackcli/internal/trace"
)

// The trace of a download, failed or not, shows neither the name of the
// file, nor its URL, nor what Slack answered.
func TestDownloadTraceDoesNotNameTheFile(t *testing.T) {
	// Registered first, so that it runs once the variable is back to
	// what it was: Reset reads it.
	t.Cleanup(func() { trace.Reset(nil) })
	t.Setenv("SLACKCLI_DEBUG", "1")
	var out bytes.Buffer
	trace.Reset(&out)

	const url = "https://files.slack.com/files-pri/T1-F1/Secret%20plan.pdf?t=xoxe-secret"
	ctx := context.Background()

	// The file cannot be written: a directory is in its place.
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "Secret plan.pdf"), 0o700); err != nil {
		t.Fatal(err)
	}
	_, err := Download(ctx, standardClient(serving(200, "", "x")), url, DownloadOptions{DestDir: dir})
	if err == nil || !strings.Contains(err.Error(), "Secret plan.pdf") {
		t.Fatalf("err = %v, want a failure to write the file", err)
	}
	// Slack answers a page, an error, nothing; then the file.
	for _, tr := range []*transport{
		serving(200, "text/html", "<html><title>Secret page</title></html>"),
		serving(404, "", "Secret answer"),
		{serve: func(*http.Request) (*http.Response, error) { return nil, errors.New("connection reset") }},
		serving(200, "", "x"),
	} {
		_, _ = Download(ctx, standardClient(tr), url, DownloadOptions{DestDir: t.TempDir()})
	}

	got := out.String()
	if n := strings.Count(got, "] download: "); n != 5 || strings.Count(got, "\n") != 5 {
		t.Errorf("%d download lines:\n%s", n, got)
	}
	if !strings.Contains(got, ", failed: could not write the file\n") {
		t.Errorf("the failed write is not traced as such:\n%s", got)
	}
	for _, secret := range []string{"Secret", "secret", "plan", "xoxe", "files-pri", dir} {
		if strings.Contains(got, secret) {
			t.Errorf("the trace shows %q:\n%s", secret, got)
		}
	}
}
