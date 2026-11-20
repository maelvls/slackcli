package clictx

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/maelvls/slackcli/internal/slack/enrich"
)

func notesContext() (c *Context, stdout, stderr *bytes.Buffer) {
	stdout, stderr = &bytes.Buffer{}, &bytes.Buffer{}
	return &Context{Stdout: stdout, Stderr: stderr}, stdout, stderr
}

// The warnings and hints of a command are in the JSON it prints, so that
// stdout and stderr together ("2>&1 | jq") are still one JSON document.
func TestNotesArePrintedInTheResult(t *testing.T) {
	c, stdout, stderr := notesContext()
	ctx, incomplete := enrich.Collect(context.Background())
	c.Incomplete = incomplete

	c.Warnf("file %s could not be downloaded: <404> & more", "F1")
	enrich.Warnf(ctx, "2 users were not resolved")
	c.ResultHintf("Nothing matched in %q.", "#general")
	if err := c.Print(map[string]any{"messages": []any{map[string]any{"ts": "1"}}}); err != nil {
		t.Fatal(err)
	}
	c.FlushNotes()

	const want = `{"messages":[{"ts":"1"}],"warnings":["file F1 could not be downloaded: <404> & more","2 users were not resolved"],"hints":["Nothing matched in \"#general\"."]}` + "\n"
	if stdout.String() != want {
		t.Errorf("printed %s\nwant    %s", stdout, want)
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want nothing: the notes were printed", stderr)
	}
	var parsed map[string]any
	if err := json.Unmarshal(append(stdout.Bytes(), stderr.Bytes()...), &parsed); err != nil {
		t.Errorf("stdout and stderr together are not JSON: %v", err)
	}
}

func TestResultWithoutNotesIsUnchanged(t *testing.T) {
	c, stdout, stderr := notesContext()
	if err := c.Print(map[string]any{"ok": true}); err != nil {
		t.Fatal(err)
	}
	c.FlushNotes()
	if stdout.String() != "{\"ok\":true}\n" || stderr.Len() != 0 {
		t.Errorf("stdout = %q, stderr = %q", stdout, stderr)
	}
}

// A result that prunes to nothing still carries the notes.
func TestNotesInAnEmptyResult(t *testing.T) {
	c, stdout, _ := notesContext()
	c.Warnf("nothing could be read")
	if err := c.Print(map[string]any{}); err != nil {
		t.Fatal(err)
	}
	if want := `{"warnings":["nothing could be read"]}` + "\n"; stdout.String() != want {
		t.Errorf("printed %s, want %s", stdout, want)
	}
}

// What no result carried is not lost: it goes to stderr when the command
// is over (it failed, or it prints text).
func TestUnprintedNotesGoToStderr(t *testing.T) {
	c, stdout, stderr := notesContext()
	ctx, incomplete := enrich.Collect(context.Background())
	c.Incomplete = incomplete

	c.Warnf("before the result")
	if err := c.Print(map[string]any{"ok": true}); err != nil {
		t.Fatal(err)
	}
	// After the result was printed: too late for it.
	c.Warnf("the uploaded file %s was left behind", "F1")
	enrich.Warnf(ctx, "the permalink could not be fetched")
	c.ResultHintf("Try again.")
	if stderr.Len() != 0 {
		t.Errorf("stderr before the flush = %q", stderr)
	}
	c.FlushNotes()
	c.FlushNotes() // once only

	if want := `{"ok":true,"warnings":["before the result"]}` + "\n"; stdout.String() != want {
		t.Errorf("printed %s, want %s", stdout, want)
	}
	const want = "Warning: the uploaded file F1 was left behind\nWarning: the permalink could not be fetched\nTry again.\n"
	if stderr.String() != want {
		t.Errorf("stderr = %q, want %q", stderr, want)
	}
}

// Notes are only added to a JSON object that has no such keys already;
// otherwise they are kept for stderr rather than dropped or duplicated.
func TestNotesAreNotAddedWhereTheyDoNotFit(t *testing.T) {
	for _, compact := range []string{`[1,2]`, `"text"`, `{"warnings":["its own"]}`, `{"a":1,"hints":"its own"}`} {
		c, stdout, stderr := notesContext()
		c.Warnf("kept")
		if err := c.PrintCompact([]byte(compact)); err != nil {
			t.Fatal(err)
		}
		if stdout.String() != compact+"\n" || stderr.Len() != 0 {
			t.Errorf("%s: stdout = %q, stderr = %q", compact, stdout, stderr)
		}
		c.FlushNotes()
		if stderr.String() != "Warning: kept\n" {
			t.Errorf("%s: stderr after the flush = %q", compact, stderr)
		}
	}
}
