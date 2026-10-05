package upload

import (
	"archive/zip"
	"bytes"
	"errors"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// The protocol itself is tested in media-kit (resumable, unpack). These tests cover what the
// music library adds: only audio gets in, archives keep audio plus a few cover pictures, and
// the outcome is counted in tracks and images.

// ---------------------------------------------------------------- helpers

var (
	mp3       = append([]byte("ID3\x03\x00\x00\x00\x00\x00\x00"), bytes.Repeat([]byte{0xFF, 0xFB, 0x90, 0}, 16)...)
	jpegBytes = []byte("\xff\xd8\xff\xe0 jpeg")
	pngBytes  = []byte("\x89PNG\r\n\x1a\n rest of a picture")
)

func crc(b []byte) string { return fmt.Sprintf("%08x", crc32.ChecksumIEEE(b)) }

func newMgr(t *testing.T) (*Manager, string) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return New(root, "uploads", 1<<30, 0), root
}

func start(t *testing.T, m *Manager, title string) string {
	t.Helper()
	_, rel, err := m.Start(title)
	if err != nil {
		t.Fatal(err)
	}
	return rel
}

// upload sends data in one chunk and waits for the outcome.
func upload(t *testing.T, m *Manager, rel, name string, data []byte) Status {
	t.Helper()
	if _, err := m.Begin(rel, name, int64(len(data))); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Append(rel, name, 0, crc(data), data); err != nil {
		t.Fatal(err)
	}
	st, err := m.Complete(rel, name, int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); st.State == Running && time.Now().Before(deadline); {
		time.Sleep(5 * time.Millisecond)
		st, _ = m.StatusOf(rel, name)
	}
	return st
}

func zipOf(t *testing.T, entries map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	for name, data := range entries {
		f, err := w.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		f.Write(data)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func exists(p string) bool { _, err := os.Lstat(p); return err == nil }

func listTree(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			rel, _ := filepath.Rel(dir, p)
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	return out
}

func put(t *testing.T, dir string, files map[string][]byte) {
	t.Helper()
	for p, data := range files {
		os.MkdirAll(filepath.Join(dir, filepath.Dir(p)), 0o755)
		if err := os.WriteFile(filepath.Join(dir, p), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// ---------------------------------------------------------------- what counts as what

func TestIsAudio(t *testing.T) {
	dir := t.TempDir()
	put(t, dir, map[string][]byte{"x": mp3, "junk": []byte("hello")})
	for _, c := range []struct {
		file, name string
		want       bool
	}{
		{"x", "x", true},               // audio content, whatever the name
		{"junk", "junk", false},        // neither
		{"junk", "song.mp3", true},     // the name counts, as everywhere else in the app
		{"missing", "song.mp3", false}, // unreadable
	} {
		if got := isAudio(filepath.Join(dir, c.file), c.name); got != c.want {
			t.Errorf("isAudio(%s as %s) = %v, want %v", c.file, c.name, got, c.want)
		}
	}
	if err := acceptAudio(filepath.Join(dir, "junk"), "notes.txt"); !errors.Is(err, errNotAudio) {
		t.Errorf("accept junk: %v", err)
	}
	if err := acceptAudio(filepath.Join(dir, "x"), "x.partial"); err != nil {
		t.Errorf("accept audio: %v", err)
	}
}

func TestIsImageNeedsPictureNameAndContent(t *testing.T) {
	dir := t.TempDir()
	put(t, dir, map[string][]byte{"ok.jpg": jpegBytes, "ok.png": pngBytes, "fake.jpg": []byte("hello"), "pic.txt": jpegBytes})
	for name, want := range map[string]bool{"ok.jpg": true, "ok.png": true, "fake.jpg": false, "pic.txt": false, "missing.jpg": false} {
		if got := isImage(filepath.Join(dir, name)); got != want {
			t.Errorf("isImage(%s) = %v, want %v", name, got, want)
		}
	}
}

func TestClassify(t *testing.T) {
	dir := t.TempDir()
	put(t, dir, map[string][]byte{"a.flac": []byte("fLaC\x00"), "cover.jpg": jpegBytes})
	if classify(filepath.Join(dir, "a.flac")) != kindAudio || classify(filepath.Join(dir, "cover.jpg")) != kindImage {
		t.Error("audio is audio, and what else prune leaves is a picture")
	}
}

func TestPruneKeepsAudioAndTheBestFewPicturesPerFolder(t *testing.T) {
	dir := t.TempDir()
	put(t, dir, map[string][]byte{
		"A/01.mp3": mp3, "A/cover.jpg": jpegBytes, "A/folder.png": pngBytes, "A/back.jpg": jpegBytes,
		"A/scan1.jpg": jpegBytes, "A/scan2.jpg": jpegBytes, "A/fake.jpg": []byte("not a picture"),
		"A/Scans/page.jpg": jpegBytes, "B/02.mp3": mp3, // A/Scans is another folder, with its own allowance
		"B/Thumbs.db": []byte("x"), "B/pic.bmp": []byte("BM"), "drop/readme.txt": []byte("x"),
		"odd.mp3": []byte("not really audio, but the extension says it is"), "disguised": mp3, // no extension, but audio content
	})
	skipped, err := prune(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := "A/fake.jpg A/scan1.jpg A/scan2.jpg B/Thumbs.db B/pic.bmp drop/readme.txt"
	if got := strings.Join(skipped, " "); got != want {
		t.Errorf("skipped %q, want %q", got, want)
	}
	for _, kept := range []string{"A/01.mp3", "A/cover.jpg", "A/folder.png", "A/back.jpg", "A/Scans/page.jpg", "B/02.mp3", "odd.mp3", "disguised"} {
		if !exists(filepath.Join(dir, kept)) {
			t.Errorf("%s should be kept (cover names first, three per folder)", kept)
		}
	}
	for _, gone := range append(skipped, "drop") {
		if exists(filepath.Join(dir, gone)) {
			t.Errorf("%s should have been removed", gone)
		}
	}
}

func TestPruneRefusesAnArchiveWithoutAudio(t *testing.T) {
	dir := t.TempDir()
	put(t, dir, map[string][]byte{"readme.txt": []byte("hi"), "cover.jpg": jpegBytes})
	if _, err := prune(dir); !errors.Is(err, errNoAudio) {
		t.Errorf("got %v", err)
	}
}

// ---------------------------------------------------------------- through the protocol

func TestLooseFiles(t *testing.T) {
	m, root := newMgr(t)
	rel := start(t, m, "Singles")
	st := upload(t, m, rel, "My Song.mp3", mp3)
	if st.State != Done || st.Tracks != 1 || st.Images != 0 || st.Path != "uploads/Singles/My Song.mp3" || st.BytesWritten != int64(len(mp3)) {
		t.Fatalf("audio: %+v", st)
	}
	if again, found := m.StatusOf(rel, "My Song.mp3"); !found || !reflect.DeepEqual(again, st) {
		t.Errorf("status: %+v %v", again, found)
	}
	if st := upload(t, m, rel, "My Song.mp3", mp3); st.State != Done || st.Tracks != 0 || st.Duplicates != 1 {
		t.Errorf("the same file again: %+v", st)
	}
	if st := upload(t, m, rel, "notes.txt", []byte("definitely not audio")); st.State != Failed || !strings.Contains(st.Error, "not an audio file") {
		t.Errorf("not audio: %+v", st)
	}
	if exists(filepath.Join(root, "uploads", "Singles", "notes.txt")) {
		t.Error("a non-audio file must not enter the library")
	}
	if st := upload(t, m, rel, "...", mp3[:40]); st.State != Done || !strings.HasSuffix(st.Path, "/track") {
		t.Errorf("a name that sanitises to nothing: %+v", st)
	}
	if _, found := m.StatusOf(rel, "never.mp3"); found {
		t.Error("nothing is tracked for a file never sent")
	}
	if st, err := m.Complete(rel, "../x", 1); !errors.Is(err, ErrBadName) || !reflect.DeepEqual(st, Status{}) {
		t.Errorf("errors pass through: %+v %v", st, err)
	}
}

func TestArchiveKeepsAudioAndCoversAndReportsTheRest(t *testing.T) {
	m, root := newMgr(t)
	rel := start(t, m, "Great Album")
	archive := zipOf(t, map[string][]byte{
		"Great Album/01 One.mp3":        mp3,
		"Great Album/Disc 2/02 Two.mp3": mp3,
		"Great Album/cover.jpg":         jpegBytes,
		"Great Album/notes.txt":         []byte("liner notes"),
		"Great Album/run.sh":            []byte("#!/bin/sh\nrm -rf /"),
	})
	st := upload(t, m, rel, "great.zip", archive)
	if st.State != Done || st.Tracks != 2 || st.Images != 1 || st.Skipped != 2 || st.Path != rel || !st.HasReport ||
		!reflect.DeepEqual(st.SkippedTypes, map[string]int{"txt": 1, "sh": 1}) {
		t.Fatalf("status: %+v", st)
	}
	if got := strings.Join(listTree(t, filepath.Join(root, "uploads", "Great Album")), ","); got != "01 One.mp3,Disc 2/02 Two.mp3,cover.jpg" {
		t.Fatalf("the wrapper is unwrapped, the cover kept and junk dropped, got %s", got)
	}
	rp, err := m.Report(rel, "great.zip")
	if err != nil {
		t.Fatal(err)
	}
	text, _ := os.ReadFile(rp)
	if !strings.HasPrefix(string(text), "2 files from great.zip were left out.\nOnly audio files and up to 3 cover pictures per folder are kept.\n\nnotes.txt\nrun.sh\n") {
		t.Errorf("report:\n%s", text)
	}

	// Adding to the same folder counts what was already there.
	_, rel2, _ := m.StartOrJoin("Great Album")
	again := zipOf(t, map[string][]byte{"01 One.mp3": mp3, "cover.jpg": jpegBytes, "03 Three.mp3": append(append([]byte{}, mp3...), 1)})
	if st := upload(t, m, rel2, "more.zip", again); st.State != Done || st.Tracks != 1 || st.Images != 0 || st.Duplicates != 2 {
		t.Errorf("second archive: %+v", st)
	}

	if st := upload(t, m, rel, "text.zip", zipOf(t, map[string][]byte{"readme.txt": []byte("hi")})); st.State != Failed || st.Error != "text.zip: contains no audio files" {
		t.Errorf("no audio: %+v", st)
	}
}
