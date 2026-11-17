package auth

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// dirEntries returns the names of what dir holds.
func dirEntries(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestWriteFileAtomic(t *testing.T) {
	t.Run("creates the directory and replaces a file, privately", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "a", "b")
		path := filepath.Join(dir, "credentials.json")
		if err := writeFileAtomic(path, []byte("one")); err != nil {
			t.Fatal(err)
		}
		wantMode(t, dir, 0o700)
		// A file that was readable by others is replaced by one that is
		// not, rather than written in place.
		if err := os.Chmod(path, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := writeFileAtomic(path, []byte("two")); err != nil {
			t.Fatal(err)
		}
		if raw, err := os.ReadFile(path); err != nil || string(raw) != "two" {
			t.Fatalf("content = %q, %v", raw, err)
		}
		wantMode(t, path, 0o600)
		if names := dirEntries(t, dir); len(names) != 1 {
			t.Errorf("left behind: %v", names)
		}
	})

	t.Run("a rename that fails leaves the old state and no temporary file", func(t *testing.T) {
		dir := t.TempDir()
		// The destination is a directory that is not empty: no OS renames
		// a file over it.
		path := filepath.Join(dir, "credentials.json")
		if err := os.MkdirAll(filepath.Join(path, "child"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := writeFileAtomic(path, []byte("data")); err == nil {
			t.Fatal("writeFileAtomic replaced a directory")
		}
		if names := dirEntries(t, dir); len(names) != 1 || names[0] != "credentials.json" {
			t.Errorf("left behind: %v", names)
		}
		if _, err := os.Stat(filepath.Join(path, "child")); err != nil {
			t.Errorf("the destination was damaged: %v", err)
		}
	})

	t.Run("a parent that is a file", func(t *testing.T) {
		notADir := filepath.Join(t.TempDir(), "file")
		if err := os.WriteFile(notADir, []byte("keep"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := writeFileAtomic(filepath.Join(notADir, "credentials.json"), []byte("data")); err == nil {
			t.Fatal("writeFileAtomic wrote below a file")
		}
		if raw, _ := os.ReadFile(notADir); string(raw) != "keep" {
			t.Errorf("the file was modified: %q", raw)
		}
	})

	t.Run("a directory that cannot be written to", func(t *testing.T) {
		if runtime.GOOS == "windows" || os.Geteuid() == 0 {
			t.Skip("needs a directory that cannot be written to")
		}
		dir := filepath.Join(t.TempDir(), "readonly")
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, "credentials.json")
		if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(dir, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
		if err := writeFileAtomic(path, []byte("new")); !errors.Is(err, fs.ErrPermission) {
			t.Fatalf("err = %v, want a permission error", err)
		}
		if raw, _ := os.ReadFile(path); string(raw) != "old" {
			t.Errorf("content = %q, want the old one", raw)
		}
	})
}

func TestRenameOver(t *testing.T) {
	setup := func(t *testing.T) (tmp, path string) {
		dir := t.TempDir()
		tmp, path = filepath.Join(dir, "tmp"), filepath.Join(dir, "credentials.json")
		if err := os.WriteFile(tmp, []byte("new"), 0o600); err != nil {
			t.Fatal(err)
		}
		// A destination that cannot be replaced until it is removed.
		if err := os.MkdirAll(filepath.Join(path, "child"), 0o700); err != nil {
			t.Fatal(err)
		}
		return tmp, path
	}

	t.Run("a failure that passes is retried", func(t *testing.T) {
		tmp, path := setup(t)
		calls := 0
		err := renameOver(tmp, path, func(error) bool {
			calls++
			if calls == 2 {
				_ = os.RemoveAll(path) // the reader closed the file
			}
			return true
		})
		if err != nil || calls != 2 {
			t.Fatalf("err = %v after %d failures", err, calls)
		}
		if raw, _ := os.ReadFile(path); string(raw) != "new" {
			t.Errorf("content = %q", raw)
		}
	})

	t.Run("any other failure is returned at once", func(t *testing.T) {
		tmp, path := setup(t)
		calls := 0
		err := renameOver(tmp, path, func(error) bool { calls++; return false })
		if err == nil || calls != 1 {
			t.Fatalf("err = %v, asked %d times", err, calls)
		}
	})

	t.Run("a failure that does not pass is given up on", func(t *testing.T) {
		tmp, path := setup(t)
		calls := 0
		err := renameOver(tmp, path, func(error) bool { calls++; return true })
		if err == nil || calls < 2 || calls > 100 {
			t.Fatalf("err = %v after %d attempts", err, calls)
		}
	})

	// Only Windows refuses to replace a file that a reader has open.
	if blocked := renameBlockedByReader(fs.ErrPermission); blocked {
		t.Error("an error that is not a Windows sharing error is taken for one")
	}
}

func TestCopyFileExclusive(t *testing.T) {
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "src"), filepath.Join(dir, "dst")
	if err := os.WriteFile(src, []byte("content"), 0o644); err != nil {
		t.Fatal(err)
	}

	// A source that does not exist leaves no (empty) copy, which a later
	// run would take for the copy of something.
	if err := copyFileExclusive(filepath.Join(dir, "absent"), dst); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("err = %v", err)
	}
	if err := copyFileExclusive(src, filepath.Join(dir, "no-such-dir", "dst")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("err = %v", err)
	}
	if names := dirEntries(t, dir); len(names) != 1 {
		t.Fatalf("left behind: %v", names)
	}

	if err := copyFileExclusive(src, dst); err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(dst); string(raw) != "content" {
		t.Errorf("copy = %q", raw)
	}
	// The copy is private whatever the mode of the source.
	wantMode(t, dst, 0o600)

	// An existing copy is left alone, and that is not an error.
	if err := os.WriteFile(src, []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := copyFileExclusive(src, dst); err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(dst); string(raw) != "content" {
		t.Errorf("the earlier copy was overwritten: %q", raw)
	}
}
