package library

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"masterplayer/internal/sniff"
)

var mp3 = append([]byte("ID3\x03\x00\x00\x00\x00\x00\x00"), bytes.Repeat([]byte{0xFF, 0xFB, 0x90, 0}, 8)...)

func mustWrite(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// newLib builds:
//   Album B/02 two.mp3, Album B/01 one.flac
//   album a/track.mp3
//   Zeta.brstm, alpha.wav, notes.txt, .hidden.mp3, nested/deep/song.mp3
//   mystery (no extension, but MP3 content)
func newLib(t *testing.T) (*Library, string) {
	t.Helper()
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "Album B", "02 two.mp3"), mp3)
	mustWrite(t, filepath.Join(root, "Album B", "01 one.flac"), []byte("fLaC\x00"))
	mustWrite(t, filepath.Join(root, "album a", "track.mp3"), mp3)
	mustWrite(t, filepath.Join(root, "Zeta.brstm"), []byte("RSTM\xFE\xFF"))
	mustWrite(t, filepath.Join(root, "alpha.wav"), []byte("RIFF\x00\x00\x00\x00WAVEfmt "))
	mustWrite(t, filepath.Join(root, "notes.txt"), []byte("hello"))
	mustWrite(t, filepath.Join(root, ".hidden.mp3"), mp3)
	mustWrite(t, filepath.Join(root, "nested", "deep", "song.mp3"), mp3)
	mustWrite(t, filepath.Join(root, "mystery"), mp3)
	l, err := New(root, "", false)
	if err != nil {
		t.Fatal(err)
	}
	return l, root
}

func names(es []Entry) []string {
	var out []string
	for _, e := range es {
		out = append(out, e.Name)
	}
	return out
}

func TestNewErrors(t *testing.T) {
	if _, err := New(filepath.Join(t.TempDir(), "missing"), "", false); err == nil {
		t.Error("missing root")
	}
	f := filepath.Join(t.TempDir(), "file")
	mustWrite(t, f, []byte("x"))
	if _, err := New(f, "", false); err == nil {
		t.Error("root that is a file")
	}
}

func TestForcedReadOnlyGuards(t *testing.T) {
	_, root := newLib(t)
	ro, err := New(root, "", true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ro.Delete("alpha.wav"); !errors.Is(err, ErrReadOnly) {
		t.Errorf("delete: %v", err)
	}
	if _, err := ro.Undo("0123456789abcdef"); !errors.Is(err, ErrReadOnly) {
		t.Errorf("undo: %v", err)
	}
	if !ro.ReadOnly() {
		t.Error("ReadOnly accessor")
	}
}

func TestBrowseOrderingAndFiltering(t *testing.T) {
	l, _ := newLib(t)
	es, err := l.Browse("")
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(names(es), ",")
	want := "album a,Album B,nested,alpha.wav,mystery,notes.txt,Zeta.brstm"
	if got != want {
		t.Fatalf("order:\n got %s\nwant %s", got, want)
	}
	byName := map[string]Entry{}
	for _, e := range es {
		byName[e.Name] = e
	}
	if !byName["alpha.wav"].Playable || byName["alpha.wav"].Kind != "wav" {
		t.Errorf("wav: %+v", byName["alpha.wav"])
	}
	if byName["Zeta.brstm"].Kind != "vgm" {
		t.Errorf("brstm: %+v", byName["Zeta.brstm"])
	}
	if byName["notes.txt"].Playable || byName["notes.txt"].Kind != "" {
		t.Errorf("txt must not be playable: %+v", byName["notes.txt"])
	}
	if !byName["mystery"].Playable || byName["mystery"].Kind != "mp3" {
		t.Errorf("content sniffing for extensionless files: %+v", byName["mystery"])
	}
	if !byName["nested"].IsDir || byName["nested"].Playable {
		t.Errorf("dir: %+v", byName["nested"])
	}
	sub, err := l.Browse("Album B")
	if err != nil || strings.Join(names(sub), ",") != "01 one.flac,02 two.mp3" {
		t.Fatalf("sub: %v %v", names(sub), err)
	}
	if sub[0].Path != "Album B/01 one.flac" {
		t.Errorf("path: %q", sub[0].Path)
	}
}

func TestBrowseErrors(t *testing.T) {
	l, _ := newLib(t)
	if _, err := l.Browse("nope"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing: %v", err)
	}
	if _, err := l.Browse("alpha.wav"); err == nil {
		t.Error("browsing a file")
	}
	if _, err := l.Browse(".trash"); !errors.Is(err, ErrHidden) {
		t.Errorf("hidden: %v", err)
	}
}

func TestResolveRejectsEscapesAndHidden(t *testing.T) {
	l, root := newLib(t)
	for _, p := range []string{"../etc/passwd", "a/../../x", "..\\..\\x", "/../../x"} {
		abs, err := l.Resolve(p)
		if err != nil {
			continue // rejected outright is fine
		}
		if abs != root && !strings.HasPrefix(abs, root+string(filepath.Separator)) {
			t.Errorf("%q escaped to %q", p, abs)
		}
	}
	for _, p := range []string{".hidden.mp3", "a/.b/c", ".trash/x", "Album B/.x"} {
		if _, err := l.Resolve(p); !errors.Is(err, ErrHidden) {
			t.Errorf("%q: %v", p, err)
		}
	}
	if abs, err := l.Resolve(""); err != nil || abs != root {
		t.Errorf("root: %q %v", abs, err)
	}
}

func TestResolveRejectsSymlinkEscape(t *testing.T) {
	l, root := newLib(t)
	outside := t.TempDir()
	mustWrite(t, filepath.Join(outside, "secret.mp3"), mp3)
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	if _, err := l.Resolve("link/secret.mp3"); !errors.Is(err, ErrOutside) {
		t.Errorf("symlink escape: %v", err)
	}
	if _, err := l.Resolve("link"); !errors.Is(err, ErrOutside) {
		t.Errorf("symlinked dir: %v", err)
	}
}

func TestTracksOrderCrossesFolders(t *testing.T) {
	l, _ := newLib(t)
	tr, err := l.Tracks("")
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, e := range tr {
		paths = append(paths, e.Path)
	}
	want := "album a/track.mp3,Album B/01 one.flac,Album B/02 two.mp3,nested/deep/song.mp3,alpha.wav,mystery,Zeta.brstm"
	if strings.Join(paths, ",") != want {
		t.Fatalf("order:\n got %s\nwant %s", strings.Join(paths, ","), want)
	}
	if sub, _ := l.Tracks("nested"); len(sub) != 1 || sub[0].Path != "nested/deep/song.mp3" {
		t.Errorf("subtree: %+v", sub)
	}
	if _, err := l.Tracks("missing"); err == nil {
		t.Error("missing root dir")
	}
}

func TestTracksTerminatesOnSymlinkLoops(t *testing.T) {
	l, root := newLib(t)
	// A directory symlink loop must terminate thanks to the depth limit.
	if err := os.Symlink(root, filepath.Join(root, "nested", "loop")); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	tr, err := l.Tracks("")
	if err != nil || len(tr) == 0 {
		t.Errorf("loop: %v %d", err, len(tr))
	}
}

func TestDescribeAndKind(t *testing.T) {
	l, _ := newLib(t)
	e, err := l.Describe("alpha.wav")
	if err != nil || !e.Playable || e.Kind != "wav" || e.Name != "alpha.wav" {
		t.Fatalf("%+v %v", e, err)
	}
	if e, err = l.Describe("nested"); err != nil || !e.IsDir {
		t.Fatalf("dir: %+v %v", e, err)
	}
	if e, err = l.Describe("notes.txt"); err != nil || e.Playable {
		t.Fatalf("txt: %+v %v", e, err)
	}
	if _, err = l.Describe("nope"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing: %v", err)
	}
	if _, err = l.Describe(".trash"); !errors.Is(err, ErrHidden) {
		t.Errorf("hidden: %v", err)
	}

	k, abs, err := l.Kind("mystery")
	if err != nil || k != sniff.MP3 || filepath.Base(abs) != "mystery" {
		t.Fatalf("kind: %v %q %v", k, abs, err)
	}
	if _, _, err = l.Kind("notes.txt"); !errors.Is(err, ErrNotAudio) {
		t.Errorf("txt: %v", err)
	}
	if _, _, err = l.Kind("nested"); !errors.Is(err, ErrNotFile) {
		t.Errorf("dir: %v", err)
	}
	if _, _, err = l.Kind("missing.mp3"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing: %v", err)
	}
	if _, _, err = l.Kind("../x"); err == nil {
		t.Error("escape")
	}
}

func TestDeleteUndoRoundTrip(t *testing.T) {
	l, root := newLib(t)
	if l.ReadOnly() {
		t.Skip("read-only temp dir")
	}
	orig := filepath.Join(root, "Album B", "02 two.mp3")

	tr, err := l.Delete("Album B/02 two.mp3")
	if err != nil {
		t.Fatal(err)
	}
	if tr.Name != "02 two.mp3" || tr.Path != "Album B/02 two.mp3" || len(tr.Token) != 16 {
		t.Fatalf("trashed: %+v", tr)
	}
	if _, err := os.Stat(orig); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("file should be gone from the library")
	}
	if es, _ := l.Browse(""); contains(names(es), ".trash") {
		t.Fatal("trash folder must stay hidden")
	}

	rel, err := l.Undo(tr.Token)
	if err != nil || rel != "Album B/02 two.mp3" {
		t.Fatalf("undo: %q %v", rel, err)
	}
	if data, err := os.ReadFile(orig); err != nil || !bytes.Equal(data, mp3) {
		t.Fatalf("restored content: %v", err)
	}
	if _, err := l.Undo(tr.Token); !errors.Is(err, ErrBadToken) {
		t.Errorf("second undo: %v", err)
	}
}

func TestUndoRecreatesFolderAndRefusesOverwrite(t *testing.T) {
	l, root := newLib(t)
	if l.ReadOnly() {
		t.Skip("read-only temp dir")
	}
	tr, err := l.Delete("nested/deep/song.mp3")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(root, "nested")); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Undo(tr.Token); err != nil {
		t.Fatalf("undo into a removed folder: %v", err)
	}

	tr2, err := l.Delete("alpha.wav")
	if err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(root, "alpha.wav"), []byte("new"))
	if _, err := l.Undo(tr2.Token); !errors.Is(err, ErrExists) {
		t.Errorf("overwrite: %v", err)
	}
}

func TestDeleteErrors(t *testing.T) {
	l, _ := newLib(t)
	if l.ReadOnly() {
		t.Skip("read-only temp dir")
	}
	if _, err := l.Delete(""); !errors.Is(err, ErrBadName) {
		t.Errorf("root: %v", err)
	}
	if _, err := l.Delete("missing.mp3"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing: %v", err)
	}
	if _, err := l.Delete(".hidden.mp3"); !errors.Is(err, ErrHidden) {
		t.Errorf("hidden: %v", err)
	}
	for _, tok := range []string{"", "short", "../../etc/passwd", "ZZZZZZZZZZZZZZZZ", "0123456789abcdef"} {
		if _, err := l.Undo(tok); !errors.Is(err, ErrBadToken) {
			t.Errorf("token %q: %v", tok, err)
		}
	}
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func TestPurgeTrash(t *testing.T) {
	l, root := newLib(t)
	if l.ReadOnly() {
		t.Skip("read-only temp dir")
	}
	if n, err := l.PurgeTrash(time.Minute); n != 0 || err != nil {
		t.Fatalf("no trash yet: %d %v", n, err)
	}
	tr, err := l.Delete("alpha.wav")
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := l.PurgeTrash(time.Hour); n != 0 {
		t.Fatalf("fresh trash must be kept, purged %d", n)
	}
	mustWrite(t, filepath.Join(l.trashDir, "stray-file"), []byte("x")) // non-directories are ignored
	l.now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	if n, err := l.PurgeTrash(time.Hour); n != 1 || err != nil {
		t.Fatalf("expected 1 purge, got %d %v", n, err)
	}
	if _, err := l.Undo(tr.Token); !errors.Is(err, ErrBadToken) {
		t.Errorf("purged item must not be restorable: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "alpha.wav")); err == nil {
		t.Error("purged file must stay deleted")
	}
}

func TestMoveFileFallbackCopies(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "a.bin")
	mustWrite(t, src, []byte("payload"))
	// Renaming onto an existing directory fails, which exercises the copy path's error handling.
	if err := MoveFile(src, dir); err == nil {
		t.Error("expected error moving onto a directory")
	}
	if err := MoveFile(filepath.Join(dir, "missing"), filepath.Join(dir, "x")); err == nil {
		t.Error("missing source")
	}
	// The copy path itself, called through a destination whose rename would fail on
	// some platforms; here we only verify that a plain move works end to end.
	dst := filepath.Join(dir, "b.bin")
	if err := MoveFile(src, dst); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(dst); string(data) != "payload" {
		t.Error("content lost")
	}
}


func TestRootIsTheResolvedLibraryPath(t *testing.T) {
	l, root := newLib(t)
	if got, _ := filepath.EvalSymlinks(root); l.Root() != got {
		t.Errorf("got %q want %q", l.Root(), got)
	}
}

func TestCleanRel(t *testing.T) {
	cases := map[string]string{"": "", "/": "", "a/b": "a/b", "/a//b/": "a/b", "a/../b": "b", "../a": "a"}
	for in, want := range cases {
		if got := CleanRel(in); got != want {
			t.Errorf("%q: got %q want %q", in, got, want)
		}
	}
}

func TestDeleteAndUndoFolder(t *testing.T) {
	l, root := newLib(t)
	if l.ReadOnly() {
		t.Skip("read-only temp dir")
	}
	tr, err := l.Delete("nested")
	if err != nil || !tr.IsDir {
		t.Fatalf("delete folder: %+v %v", tr, err)
	}
	if _, err := os.Stat(filepath.Join(root, "nested")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("folder still there: %v", err)
	}
	if _, err := l.Undo(tr.Token); err != nil {
		t.Fatalf("undo folder: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "nested", "deep", "song.mp3")); err != nil {
		t.Errorf("contents not restored: %v", err)
	}
}

func TestRenameFolder(t *testing.T) {
	l, root := newLib(t)
	if l.ReadOnly() {
		t.Skip("read-only temp dir")
	}
	got, err := l.Rename("nested/deep", "deeper")
	if err != nil || got != "nested/deeper" {
		t.Fatalf("rename: %q %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(root, "nested", "deeper", "song.mp3")); err != nil {
		t.Errorf("contents not moved: %v", err)
	}
	if got, err = l.Rename("nested", "Nested2"); err != nil || got != "Nested2" {
		t.Errorf("top-level rename: %q %v", got, err)
	}
	for _, bad := range []string{"", ".", "..", ".x", "a/b", `a\`, " pad"} {
		if _, err := l.Rename("Nested2", bad); !errors.Is(err, ErrBadName) {
			t.Errorf("name %q: %v", bad, err)
		}
	}
	if _, err := l.Rename("", "x"); !errors.Is(err, ErrBadName) {
		t.Errorf("root: %v", err)
	}
	if _, err := l.Rename("alpha.wav", "x"); !errors.Is(err, ErrNotDir) {
		t.Errorf("file: %v", err)
	}
	if _, err := l.Rename("missing", "x"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing: %v", err)
	}
	if err := os.Mkdir(filepath.Join(root, "other"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Rename("other", "Nested2"); !errors.Is(err, ErrExists) {
		t.Errorf("clash: %v", err)
	}
}
