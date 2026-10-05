//go:build linux

package library

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestDeleteRefusesWhatIsNeitherFileNorFolder(t *testing.T) {
	l, root := newLib(t)
	if err := syscall.Mkfifo(filepath.Join(root, "pipe"), 0o644); err != nil {
		t.Skip("cannot make a named pipe here:", err)
	}
	if _, err := l.Delete("pipe"); !errors.Is(err, ErrNotFile) {
		t.Errorf("a pipe is not deletable through the library: %v", err)
	}
}

func TestRenameRefusals(t *testing.T) {
	l, root := newLib(t)
	ro, _ := New(root, "", true)
	if _, err := ro.Rename("Album B", "x"); !errors.Is(err, ErrReadOnly) {
		t.Errorf("read-only: %v", err)
	}
	if _, err := l.Rename(".trash/x", "y"); !errors.Is(err, ErrHidden) {
		t.Errorf("hidden source: %v", err)
	}
	// A link that leaves the library cannot be renamed onto.
	if err := os.Symlink(t.TempDir(), filepath.Join(root, "escape")); err != nil {
		t.Skip("no symlinks here:", err)
	}
	if _, err := l.Rename("Album B", "escape"); !errors.Is(err, ErrOutside) {
		t.Errorf("destination outside the library: %v", err)
	}
	swapRename(t, func(string, string) error { return errors.New("disk on fire") })
	if _, err := l.Rename("album a", "zzz"); err == nil {
		t.Error("a failed rename is reported")
	}
}

func TestUndoReportsAFailedMove(t *testing.T) {
	l, _ := newLib(t)
	swapRand(t, fixedRand)
	tr, err := l.Delete("album a")
	if err != nil {
		t.Fatal(err)
	}
	swapRename(t, func(string, string) error { return errors.New("disk on fire") })
	if _, err := l.Undo(tr.Token); err == nil {
		t.Error("a folder that cannot be moved back is reported, and stays in the trash")
	}
}
