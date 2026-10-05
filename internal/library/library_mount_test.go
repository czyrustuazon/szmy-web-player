package library

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// crossDevice makes renames into any of the given folders fail the way they do
// across mounts; every other rename is real.
func crossDevice(t *testing.T, mounts ...string) {
	t.Helper()
	swapRename(t, func(oldpath, newpath string) error {
		for _, m := range mounts {
			if strings.HasPrefix(newpath, m+string(filepath.Separator)) {
				return &os.LinkError{Op: "rename", Old: oldpath, New: newpath, Err: syscall.EXDEV}
			}
		}
		return os.Rename(oldpath, newpath)
	})
}

func TestDeleteFolderOnAnotherMountTrashesItInsideThatMount(t *testing.T) {
	l, root := writableLib(t)
	mustWrite(t, filepath.Join(root, "uploads", "music (2)", "x.mp3"), mp3)
	crossDevice(t, l.trashDir) // uploads/ is its own mount, unlike the trash

	tr, err := l.Delete("uploads/music (2)")
	if err != nil {
		t.Fatal(err)
	}
	near := filepath.Join(root, "uploads", ".trash", tr.Token)
	if _, err := os.Stat(filepath.Join(near, "f", "x.mp3")); err != nil {
		t.Fatal("the folder should be trashed inside its own mount:", err)
	}
	if es, _ := l.Browse("uploads"); len(es) != 0 {
		t.Errorf("the nearby trash stays hidden: %v", names(es))
	}

	if got, err := l.Undo(tr.Token); err != nil || got != "uploads/music (2)" {
		t.Fatalf("undo: %q %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(root, "uploads", "music (2)", "x.mp3")); err != nil {
		t.Error("restored:", err)
	}
	if _, err := os.Stat(filepath.Join(root, "uploads", ".trash")); err == nil {
		t.Error("the empty nearby trash is removed after undo")
	}

	tr, err = l.Delete("uploads/music (2)")
	if err != nil {
		t.Fatal(err)
	}
	l.now = func() time.Time { return time.Now().Add(time.Hour) }
	if n, err := l.PurgeTrash(time.Minute); err != nil || n != 1 {
		t.Fatalf("purge: %d %v", n, err)
	}
	if _, err := os.Stat(filepath.Join(root, "uploads", ".trash")); err == nil {
		t.Error("purging removes the nearby trash too")
	}
}

func TestDeleteFolderOnADeeperMountTriesEachAncestor(t *testing.T) {
	l, root := writableLib(t)
	mustWrite(t, filepath.Join(root, "a", "b", "c", "x.mp3"), mp3)
	crossDevice(t, l.trashDir, filepath.Join(root, "a", ".trash")) // a/b is the mount

	tr, err := l.Delete("a/b/c")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "a", "b", ".trash", tr.Token, "f", "x.mp3")); err != nil {
		t.Error(err)
	}
	if _, err := os.Stat(filepath.Join(root, "a", ".trash")); err == nil {
		t.Error("the failed attempt leaves nothing behind")
	}
}

func TestDeleteFolderAcrossMountsWithNowhereToGo(t *testing.T) {
	l, root := writableLib(t)
	crossDevice(t, l.trashDir, filepath.Join(root, "nested", ".trash"))
	for _, rel := range []string{"Album B", "nested/deep"} {
		if _, err := l.Delete(rel); !errors.Is(err, syscall.EXDEV) {
			t.Errorf("%s: %v", rel, err)
		}
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel))); err != nil {
			t.Errorf("%s must survive", rel)
		}
	}
	if des, _ := os.ReadDir(l.trashDir); len(des) != 0 {
		t.Errorf("no half-made trash entries: %d", len(des))
	}
}

func TestDeleteFolderAcrossMountsReportsFailures(t *testing.T) {
	skipIfWindows(t)
	l, root := writableLib(t)
	swapRand(t, fixedRand)
	crossDevice(t, l.trashDir)

	// The "at" note cannot be written.
	if err := os.MkdirAll(filepath.Join(l.trashDir, fixedToken, "at"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Delete("nested/deep"); err == nil {
		t.Error("expected an error writing the note")
	}

	// The nearby trash cannot be made.
	mustWrite(t, filepath.Join(root, "nested", ".trash"), []byte("x"))
	if _, err := l.Delete("nested/deep"); err == nil {
		t.Error("expected an error making the nearby trash")
	}
	if _, err := os.Stat(filepath.Join(root, "nested", "deep", "song.mp3")); err != nil {
		t.Error("the folder must survive")
	}
}
