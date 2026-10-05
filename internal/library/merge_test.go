package library

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func tree(t *testing.T, dir string) string {
	t.Helper()
	var out []string
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && p != dir {
			rel, _ := filepath.Rel(dir, p)
			if d.IsDir() {
				rel += "/"
			}
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	return strings.Join(out, " ")
}

func TestMergeFoldersMovesWhatIsNewAndNeverOverwrites(t *testing.T) {
	root := t.TempDir()
	l, err := New(root, "", false)
	if err != nil {
		t.Fatal(err)
	}
	w := func(rel string, data []byte) { mustWrite(t, filepath.Join(root, filepath.FromSlash(rel)), data) }
	changed := append(append([]byte{}, mp3...), 1, 2, 3)

	w("A/01.mp3", mp3) // identical to B's
	w("A/02.mp3", changed)
	w("A/sub/z.mp3", mp3)
	w("A/cover.jpg", []byte("picture"))
	w("A/Extras/e.mp3", mp3)
	w("A/.secret", []byte("hidden"))
	linked := os.Symlink("/etc/passwd", filepath.Join(root, "A", "link.mp3")) == nil
	w("B/01.mp3", mp3)
	w("B/02.mp3", mp3)
	w("B/sub/old.mp3", mp3)
	os.MkdirAll(filepath.Join(root, "B", "cover.jpg"), 0o755) // a folder where A has a file
	w("B/Extras", []byte("a file where A has a folder"))

	res, err := l.Merge("A/", "B")
	if err != nil {
		t.Fatal(err)
	}
	if res.Path != "B" || res.Moved != 4 || res.Duplicates != 1 {
		t.Errorf("result: %+v", res)
	}
	wantSkipped := 2 // the hidden file and the link
	if !linked {
		wantSkipped--
	}
	if res.Skipped != wantSkipped {
		t.Errorf("skipped %d, want %d", res.Skipped, wantSkipped)
	}
	wantMoves := map[string]string{
		"A/01.mp3": "B/01.mp3", "A/02.mp3": "B/02 (2).mp3", "A/sub/z.mp3": "B/sub/z.mp3",
		"A/cover.jpg": "B/cover (2).jpg", "A/Extras/e.mp3": "B/Extras (2)/e.mp3",
	}
	if len(res.Moves) != len(wantMoves) {
		t.Errorf("moves: %v", res.Moves)
	}
	for from, to := range wantMoves {
		if res.Moves[from] != to {
			t.Errorf("%s moved to %q, want %q", from, res.Moves[from], to)
		}
	}
	got := tree(t, filepath.Join(root, "B"))
	want := "01.mp3 02 (2).mp3 02.mp3 Extras Extras (2)/ Extras (2)/e.mp3 cover (2).jpg cover.jpg/ sub/ sub/old.mp3 sub/z.mp3"
	if got != want {
		t.Errorf("merged tree:\n got %s\nwant %s", got, want)
	}
	// What could not be moved stays in the source folder, which is therefore kept.
	left := tree(t, filepath.Join(root, "A"))
	if !strings.Contains(left, ".secret") || strings.Contains(left, "sub") || strings.Contains(left, "Extras") {
		t.Errorf("left behind: %s", left)
	}
}

func TestMergeRemovesTheEmptiedSourceFolder(t *testing.T) {
	root := t.TempDir()
	l, _ := New(root, "", false)
	mustWrite(t, filepath.Join(root, "Top", "A", "x.mp3"), mp3)
	os.MkdirAll(filepath.Join(root, "Top", "B"), 0o755)
	res, err := l.Merge("Top/A", "Top/B")
	if err != nil || res.Moved != 1 || res.Moves["Top/A/x.mp3"] != "Top/B/x.mp3" {
		t.Fatalf("%+v %v", res, err)
	}
	if exists(filepath.Join(root, "Top", "A")) {
		t.Error("the source folder is removed once empty")
	}
}

func exists(p string) bool { _, err := os.Lstat(p); return err == nil }

func TestMergeRefusals(t *testing.T) {
	root := t.TempDir()
	l, _ := New(root, "", false)
	mustWrite(t, filepath.Join(root, "A", "sub", "x.mp3"), mp3)
	mustWrite(t, filepath.Join(root, "A2", "y.mp3"), mp3)
	mustWrite(t, filepath.Join(root, "file.mp3"), mp3)
	for name, c := range map[string]struct {
		src, dst string
		want     error
	}{
		"root as source":      {"", "A", ErrBadName},
		"root as destination": {"A", "/", ErrBadName},
		"into itself":         {"A", "A", ErrNested},
		"into a child":        {"A", "A/sub", ErrNested},
		"into its parent":     {"A/sub", "A", ErrNested},
		"hidden":              {"A", ".trash", ErrHidden},
		"a file":              {"file.mp3", "A", ErrNotDir},
		"into a file":         {"A", "file.mp3", ErrNotDir},
		"missing source":      {"nope", "A", fs.ErrNotExist},
		"missing destination": {"A", "nope", fs.ErrNotExist},
	} {
		if _, err := l.Merge(c.src, c.dst); !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", name, err, c.want)
		}
	}
	// Names that merely start alike are not nested.
	if _, err := l.Merge("A", "A2"); err != nil {
		t.Errorf("A and A2 are siblings: %v", err)
	}
	ro, _ := New(root, "", true)
	if _, err := ro.Merge("A2", "A"); !errors.Is(err, ErrReadOnly) {
		t.Errorf("read-only: %v", err)
	}
}

func TestMergeReportsFailuresHalfWay(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("relies on the 255 byte file name limit")
	}
	long := strings.Repeat("n", 252) // "(2)" would push the name over the limit
	check := func(name string, setup func(w func(string, []byte)), src, dst string) {
		root := t.TempDir()
		l, _ := New(root, "", false)
		setup(func(rel string, data []byte) { mustWrite(t, filepath.Join(root, filepath.FromSlash(rel)), data) })
		if _, err := l.Merge(src, dst); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
	check("file that cannot be renamed", func(w func(string, []byte)) {
		w("A/"+long, mp3)
		w("B/"+long, []byte("different"))
	}, "A", "B")
	check("folder that cannot be created", func(w func(string, []byte)) {
		w("A/"+long+"/x.mp3", mp3)
		w("B/"+long, []byte("a file in the way"))
	}, "A", "B")
	check("a failure inside a merged folder", func(w func(string, []byte)) {
		w("A/d/"+long, mp3)
		w("B/d/"+long, []byte("different"))
	}, "A", "B")
	if err := mergeDir(filepath.Join(t.TempDir(), "gone"), t.TempDir(), "a", "b", &MergeResult{}); err == nil {
		t.Error("an unreadable source folder is an error")
	}
}

func TestSameContent(t *testing.T) {
	dir := t.TempDir()
	big := bytes.Repeat([]byte("0123456789abcdef"), 10000) // more than one 64 KiB block
	exact := bytes.Repeat([]byte("x"), 64<<10)             // ends exactly on a block boundary
	altered := append([]byte{}, big...)
	altered[len(altered)-1] ^= 1
	earlier := append([]byte{}, big...)
	earlier[5] ^= 1
	for name, data := range map[string][]byte{
		"a": big, "b": big, "c": altered, "d": earlier, "e": big[:100], "x1": exact, "x2": exact, "empty1": nil, "empty2": nil,
	} {
		os.WriteFile(filepath.Join(dir, name), data, 0o644)
	}
	os.Mkdir(filepath.Join(dir, "folder"), 0o755)
	p := func(n string) string { return filepath.Join(dir, n) }
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"a", "b", true}, {"a", "c", false}, {"a", "d", false}, {"a", "e", false}, {"x1", "x2", true}, {"empty1", "empty2", true},
		{"a", "missing", false}, {"missing", "a", false}, {"folder", "a", false}, {"a", "folder", false},
	} {
		if got := SameContent(p(c.a), p(c.b)); got != c.want {
			t.Errorf("SameContent(%s, %s) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}
