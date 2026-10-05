package library

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func swapRand(t *testing.T, f func([]byte) (int, error)) {
	t.Helper()
	old := randRead
	randRead = f
	t.Cleanup(func() { randRead = old })
}

func swapRename(t *testing.T, f func(oldpath, newpath string) error) {
	t.Helper()
	old := renameFile
	renameFile = f
	t.Cleanup(func() { renameFile = old })
}

func fixedRand(b []byte) (int, error) {
	for i := range b {
		b[i] = 1
	}
	return len(b), nil
}

const fixedToken = "0101010101010101"

func skipIfWindows(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("relies on POSIX path semantics")
	}
}

func writableLib(t *testing.T) (*Library, string) {
	t.Helper()
	l, root := newLib(t)
	if l.ReadOnly() {
		t.Skip("read-only temp dir")
	}
	return l, root
}

func TestWritableProbe(t *testing.T) {
	if writable(filepath.Join(t.TempDir(), "does-not-exist")) {
		t.Error("a missing directory is not writable")
	}
}

func TestResolveAndKindEdgeCases(t *testing.T) {
	skipIfWindows(t)
	l, _ := newLib(t)
	if _, _, err := l.Kind(".trash/x"); !errors.Is(err, ErrHidden) {
		t.Errorf("hidden kind: %v", err)
	}
	// A file used as a folder yields a plain OS error, not "outside" or "hidden".
	_, err := l.Resolve("alpha.wav/inside")
	if err == nil || errors.Is(err, ErrHidden) || errors.Is(err, ErrOutside) {
		t.Errorf("file as folder: %v", err)
	}
	if h := readHeader(filepath.Join(t.TempDir(), "missing")); h != nil {
		t.Errorf("missing file header: %v", h)
	}
}

func TestBrowseSkipsDanglingSymlinks(t *testing.T) {
	l, root := newLib(t)
	if err := os.Symlink(filepath.Join(root, "nowhere.mp3"), filepath.Join(root, "dangling.mp3")); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	es, err := l.Browse("")
	if err != nil {
		t.Fatal(err)
	}
	if contains(names(es), "dangling.mp3") {
		t.Error("a broken symlink must not be listed")
	}
}

func TestSortTieBreaksOnExactName(t *testing.T) {
	l, root := newLib(t)
	mustWrite(t, filepath.Join(root, "Same.mp3"), mp3)
	mustWrite(t, filepath.Join(root, "same.mp3"), mp3)
	es, err := l.Browse("")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, n := range names(es) {
		if strings.EqualFold(n, "same.mp3") {
			got = append(got, n)
		}
	}
	if len(got) != 2 {
		t.Skip("case-insensitive file system")
	}
	if got[0] != "Same.mp3" || got[1] != "same.mp3" {
		t.Errorf("names that differ only by case should sort deterministically: %v", got)
	}
}

func TestTracksSkipsEscapingFoldersAndCapsResults(t *testing.T) {
	l, root := newLib(t)
	outside := t.TempDir()
	mustWrite(t, filepath.Join(outside, "secret.mp3"), mp3)
	if err := os.Symlink(outside, filepath.Join(root, "out")); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	tr, err := l.Tracks("")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range tr {
		if strings.HasPrefix(e.Path, "out/") {
			t.Errorf("track from outside the library: %s", e.Path)
		}
	}

	old := maxTracks
	maxTracks = 2
	t.Cleanup(func() { maxTracks = old })
	capped, err := l.Tracks("")
	if err != nil || len(capped) != 2 {
		t.Fatalf("cap: %d %v", len(capped), err)
	}
}

func TestDeleteRandomSourceFailure(t *testing.T) {
	l, root := writableLib(t)
	swapRand(t, func([]byte) (int, error) { return 0, errors.New("no entropy") })
	if _, err := l.Delete("alpha.wav"); err == nil || !strings.Contains(err.Error(), "no entropy") {
		t.Fatalf("got %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "alpha.wav")); err != nil {
		t.Error("a failed delete must leave the file alone")
	}
}

func TestDeleteTrashUnavailable(t *testing.T) {
	l, root := writableLib(t)
	blocker := filepath.Join(t.TempDir(), "file")
	mustWrite(t, blocker, []byte("x"))
	l.trashDir = filepath.Join(blocker, "trash") // parent is a file, so it cannot be created
	if _, err := l.Delete("alpha.wav"); err == nil {
		t.Fatal("expected an error")
	}
	if _, err := os.Stat(filepath.Join(root, "alpha.wav")); err != nil {
		t.Error("file must survive")
	}
}

func TestDeleteCleansUpWhenTrashEntryCannotBeWritten(t *testing.T) {
	skipIfWindows(t)
	l, root := writableLib(t)
	swapRand(t, fixedRand)
	// A directory where the "orig" note must go makes WriteFile fail.
	if err := os.MkdirAll(filepath.Join(l.trashDir, fixedToken, "orig"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Delete("alpha.wav"); err == nil {
		t.Fatal("expected an error")
	}
	if _, err := os.Stat(filepath.Join(root, "alpha.wav")); err != nil {
		t.Error("file must survive")
	}
	if _, err := os.Stat(filepath.Join(l.trashDir, fixedToken)); err == nil {
		t.Error("the half-made trash entry must be removed")
	}
}

func TestDeleteCleansUpWhenMoveFails(t *testing.T) {
	skipIfWindows(t)
	l, root := writableLib(t)
	swapRand(t, fixedRand)
	// A non-empty directory where the file must go makes both the rename and the copy fail.
	blocked := filepath.Join(l.trashDir, fixedToken, "f")
	mustWrite(t, filepath.Join(blocked, "keep"), []byte("x"))
	if _, err := l.Delete("alpha.wav"); err == nil {
		t.Fatal("expected an error")
	}
	if _, err := os.Stat(filepath.Join(root, "alpha.wav")); err != nil {
		t.Error("file must survive")
	}
}

func TestUndoFailures(t *testing.T) {
	l, root := writableLib(t)
	trash := func(token, orig string, withFile bool) {
		dir := filepath.Join(l.trashDir, token)
		mustWrite(t, filepath.Join(dir, "orig"), []byte(orig))
		if withFile {
			mustWrite(t, filepath.Join(dir, "f"), mp3)
		}
	}

	trash("aaaaaaaaaaaaaaaa", ".hidden/x.mp3", true)
	if _, err := l.Undo("aaaaaaaaaaaaaaaa"); !errors.Is(err, ErrHidden) {
		t.Errorf("hidden destination: %v", err)
	}

	trash("bbbbbbbbbbbbbbbb", "restored.mp3", false) // the trashed file itself is missing
	if _, err := l.Undo("bbbbbbbbbbbbbbbb"); err == nil || errors.Is(err, ErrBadToken) {
		t.Errorf("missing trashed file: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "restored.mp3")); err == nil {
		t.Error("nothing should have been restored")
	}
}

func TestPurgeTrashReadError(t *testing.T) {
	l, _ := newLib(t)
	file := filepath.Join(t.TempDir(), "file")
	mustWrite(t, file, []byte("x"))
	l.trashDir = file // exists but is not a directory
	if n, err := l.PurgeTrash(time.Minute); err == nil || n != 0 {
		t.Errorf("got %d %v", n, err)
	}
}

func TestMoveFileCopyFallback(t *testing.T) {
	swapRename(t, func(string, string) error { return errors.New("cross-device link") })
	dir := t.TempDir()

	src, dst := filepath.Join(dir, "a.bin"), filepath.Join(dir, "b.bin")
	mustWrite(t, src, []byte("payload"))
	if err := moveFile(src, dst); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(dst); string(data) != "payload" {
		t.Error("content lost in the copy")
	}
	if _, err := os.Stat(src); err == nil {
		t.Error("source should be removed after a copy")
	}

	// A failing copy removes the half-written destination.
	dirSrc := filepath.Join(dir, "adir")
	if err := os.Mkdir(dirSrc, 0o755); err != nil {
		t.Fatal(err)
	}
	failed := filepath.Join(dir, "never.bin")
	if err := moveFile(dirSrc, failed); err == nil {
		t.Error("copying a directory must fail")
	}
	if _, err := os.Stat(failed); err == nil {
		t.Error("partial destination must be cleaned up")
	}
}

func TestSaveFileRenameFailureCleansUp(t *testing.T) {
	l, root := writableLib(t)
	swapRename(t, func(string, string) error { return errors.New("disk on fire") })
	if _, _, err := l.SaveFile("uploads", "a.mp3", strings.NewReader(string(mp3)), 1<<20); err == nil {
		t.Fatal("expected an error")
	}
	es, _ := os.ReadDir(filepath.Join(root, "uploads"))
	if len(es) != 0 {
		t.Errorf("temp file left behind: %v", es)
	}
}
