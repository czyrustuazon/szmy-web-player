package upload

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------- zip archives

func TestZipExtractsOnlyAudioAndUnwrapsTheTopFolder(t *testing.T) {
	m, root := newMgr(t)
	rel := start(t, m, "Great Album")
	archive := zipOf(t, map[string][]byte{
		"Great Album/01 One.mp3":        mp3,
		"Great Album/Disc 2/02 Two.mp3": mp3,
		"Great Album/cover.jpg":         []byte("\xff\xd8\xff\xe0 jpeg"),
		"Great Album/notes.txt":         []byte("liner notes"),
		"Great Album/run.sh":            []byte("#!/bin/sh\nrm -rf /"),
	})
	send(t, m, rel, "great.zip", archive, 64)
	st := finish(t, m, rel, "great.zip", len(archive))
	if st.State != Done || st.Tracks != 2 || st.Skipped != 3 || st.Path != rel {
		t.Fatalf("status: %+v", st)
	}
	files := listTree(t, filepath.Join(root, "uploads", "Great Album"))
	want := map[string]bool{"01 One.mp3": true, "Disc 2/02 Two.mp3": true}
	if len(files) != 2 || !want[files[0]] || !want[files[1]] {
		t.Fatalf("expected the wrapper folder to be unwrapped and junk dropped, got %v", files)
	}
	// No scratch folders or staged bytes are left behind.
	entries, _ := os.ReadDir(filepath.Join(root, "uploads", "Great Album"))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			t.Errorf("hidden leftover %s", e.Name())
		}
	}
	if staged, _ := os.ReadDir(filepath.Join(root, "uploads", StagingDirName)); len(staged) != 0 {
		t.Errorf("staging should be empty: %v", staged)
	}
}

func TestZipTwoLevelsOfWrappersAndNameCollisions(t *testing.T) {
	m, root := newMgr(t)
	rel := start(t, m, "Batch")
	archive := zipOf(t, map[string][]byte{"Artist/Album/a.mp3": mp3, "Artist/Album/b.mp3": mp3})
	send(t, m, rel, "one.zip", archive, 4096)
	if st := finish(t, m, rel, "one.zip", len(archive)); st.State != Done || st.Tracks != 2 {
		t.Fatalf("first: %+v", st)
	}
	// A second archive with the same file names must not overwrite the first one's files.
	send(t, m, rel, "two.zip", archive, 4096)
	if st := finish(t, m, rel, "two.zip", len(archive)); st.State != Done {
		t.Fatalf("second: %+v", st)
	}
	files := listTree(t, filepath.Join(root, "uploads", "Batch"))
	want := map[string]bool{"a.mp3": true, "b.mp3": true, "a (2).mp3": true, "b (2).mp3": true}
	if len(files) != 4 {
		t.Fatalf("both archives' tracks must survive, got %v", files)
	}
	for _, f := range files {
		if !want[f] {
			t.Errorf("unexpected file %q (the counter must go before the extension)", f)
		}
	}
}

func TestZipSymlinkEntriesAreNeverExtracted(t *testing.T) {
	m, root := newMgr(t)
	rel := start(t, m, "x")
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	real, _ := w.CreateHeader(&zip.FileHeader{Name: "real.mp3", Method: zip.Store})
	real.Write(mp3)
	hdr := &zip.FileHeader{Name: "link.mp3", Method: zip.Store}
	hdr.SetMode(os.ModeSymlink | 0o777)
	link, _ := w.CreateHeader(hdr)
	link.Write([]byte("/etc/passwd")) // a symlink entry's data is its target path
	w.Close()
	send(t, m, rel, "links.zip", buf.Bytes(), 4096)
	st := finish(t, m, rel, "links.zip", buf.Len())
	if st.State != Done || st.Tracks != 1 {
		t.Fatalf("status: %+v", st)
	}
	if files := listTree(t, filepath.Join(root, "uploads", "x")); len(files) != 1 || files[0] != "real.mp3" {
		t.Errorf("only the real file may arrive, got %v", files)
	}
}

func TestInsecureZipPathsReachTheEscapeCheck(t *testing.T) {
	// With GODEBUG=zipinsecurepath=0 Go reports ErrInsecurePath alongside a usable reader.
	t.Setenv("GODEBUG", "zipinsecurepath=0")
	m, root := newMgr(t)
	rel := start(t, m, "x")
	archive := zipOf(t, map[string][]byte{"../../evil.mp3": mp3})
	send(t, m, rel, "evil.zip", archive, 4096)
	st := finish(t, m, rel, "evil.zip", len(archive))
	if st.State != Failed || !strings.Contains(st.Error, "escapes") {
		t.Fatalf("status: %+v", st)
	}
	if exists(filepath.Join(filepath.Dir(root), "evil.mp3")) {
		t.Fatal("a zip entry escaped the destination")
	}
}

func TestUniqueFile(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.mp3"), nil, 0o644)
	os.WriteFile(filepath.Join(dir, "a (2).mp3"), nil, 0o644)
	if got := uniqueFile(dir, "a.mp3"); got != filepath.Join(dir, "a (3).mp3") {
		t.Errorf("got %s", got)
	}
	if got := uniqueFile(dir, "fresh.flac"); got != filepath.Join(dir, "fresh.flac") {
		t.Errorf("got %s", got)
	}
	if got := uniqueFile(dir, "noext"); got != filepath.Join(dir, "noext") {
		t.Errorf("got %s", got)
	}
}

func TestZipSlipIsRefused(t *testing.T) {
	m, root := newMgr(t)
	rel := start(t, m, "x")
	archive := zipOf(t, map[string][]byte{"../../evil.mp3": mp3})
	send(t, m, rel, "evil.zip", archive, 4096)
	st := finish(t, m, rel, "evil.zip", len(archive))
	if st.State != Failed || !strings.Contains(st.Error, "escapes") {
		t.Fatalf("status: %+v", st)
	}
	if exists(filepath.Join(root, "evil.mp3")) || exists(filepath.Join(filepath.Dir(root), "evil.mp3")) {
		t.Fatal("a zip entry escaped the destination")
	}
}

func TestArchivesWithoutAudioFail(t *testing.T) {
	m, root := newMgr(t)
	rel := start(t, m, "x")
	for name, archive := range map[string][]byte{
		"text.zip":  zipOf(t, map[string][]byte{"readme.txt": []byte("hi"), "dir/more.txt": []byte("hi")}),
		"empty.zip": zipOf(t, map[string][]byte{}),
	} {
		send(t, m, rel, name, archive, 4096)
		st := finish(t, m, rel, name, len(archive))
		if st.State != Failed || !strings.Contains(st.Error, "no audio files") {
			t.Errorf("%s: %+v", name, st)
		}
	}
	if files := listTree(t, filepath.Join(root, "uploads", "x")); len(files) != 0 {
		t.Errorf("nothing should be added: %v", files)
	}
}

func TestZipThatExpandsPastTheLimitIsRefused(t *testing.T) {
	m, root := newMgr(t)
	m.maxBytes = 4000
	rel := start(t, m, "x")
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	f, _ := w.Create("big.mp3") // deflated: tiny on the wire, 20 kB once extracted
	f.Write(append(append([]byte{}, mp3...), bytes.Repeat([]byte{0}, 20000)...))
	w.Close()
	if buf.Len() > 3000 {
		t.Fatalf("test archive too large to be a bomb: %d", buf.Len())
	}
	send(t, m, rel, "bomb.zip", buf.Bytes(), 4096)
	st := finish(t, m, rel, "bomb.zip", buf.Len())
	if st.State != Failed || !strings.Contains(st.Error, "size limit") {
		t.Fatalf("status: %+v", st)
	}
	if files := listTree(t, filepath.Join(root, "uploads", "x")); len(files) != 0 {
		t.Errorf("partial extraction must not be published: %v", files)
	}
}

func TestPublishingFailureIsReported(t *testing.T) {
	m, _ := newMgr(t)
	rel := start(t, m, "x")
	m.rename = func(string, string) error { return errors.New("cannot rename") }
	archive := zipOf(t, map[string][]byte{"a.mp3": mp3})
	send(t, m, rel, "a.zip", archive, 4096)
	st := finish(t, m, rel, "a.zip", len(archive))
	if st.State != Failed || !strings.Contains(st.Error, "cannot rename") {
		t.Fatalf("status: %+v", st)
	}
}

// ---------------------------------------------------------------- integrity and recovery

func TestCorruptedChunkIsLocalisedAndOnlyThatPartIsResent(t *testing.T) {
	m, _ := newMgr(t)
	rel := start(t, m, "x")
	content := append([]byte{}, mp3...)
	content = append(content, bytes.Repeat([]byte("0123456789"), 40)...) // 400 bytes of audio-ish data
	archive := zipOf(t, map[string][]byte{"song.mp3": content})
	const chunk = 100
	if len(archive) < 4*chunk {
		t.Fatalf("archive too small for the scenario: %d", len(archive))
	}
	send(t, m, rel, "song.zip", archive, chunk)

	// Bitrot after arrival: flip a byte inside the second chunk (bytes 100..199).
	stagingPath, _ := m.sessionPaths(rel, "song.zip")
	staged, _ := os.ReadFile(stagingPath)
	staged[150] ^= 0xFF
	os.WriteFile(stagingPath, staged, 0o644)

	st := finish(t, m, rel, "song.zip", len(archive))
	if st.State != Corrupted || st.ResumeOffset != 100 || !strings.Contains(st.Error, "100") {
		t.Fatalf("expected a corruption verdict pointing at byte 100, got %+v", st)
	}
	if info, _ := os.Stat(stagingPath); info.Size() != 100 {
		t.Fatalf("the session should be rewound to 100, is %d", info.Size())
	}

	// The client asks where to resume and re-sends only the damaged part onwards.
	off, err := m.Begin(rel, "song.zip", int64(len(archive)))
	if err != nil || off != 100 {
		t.Fatalf("resume point: %d %v", off, err)
	}
	for off < int64(len(archive)) {
		part := archive[off:min(off+chunk, int64(len(archive)))]
		if off, err = m.Append(rel, "song.zip", off, crc(part), part); err != nil {
			t.Fatal(err)
		}
	}
	if st = finish(t, m, rel, "song.zip", len(archive)); st.State != Done || st.Tracks != 1 {
		t.Fatalf("after recovery: %+v", st)
	}
}

func TestADamagedSourceFileIsDiscardedNotRetriedForever(t *testing.T) {
	m, root := newMgr(t)
	rel := start(t, m, "x")
	garbage := bytes.Repeat([]byte("this is not a zip file. "), 10)
	send(t, m, rel, "bad.zip", garbage, 50)
	st := finish(t, m, rel, "bad.zip", len(garbage))
	if st.State != Failed || !strings.Contains(st.Error, "source file itself is damaged") {
		t.Fatalf("status: %+v", st)
	}
	if entries, _ := os.ReadDir(filepath.Join(root, "uploads", StagingDirName)); len(entries) != 0 {
		t.Errorf("a doomed session must not linger: %v", entries)
	}
}

func TestRepairOrDiscardEdgeCases(t *testing.T) {
	dir := t.TempDir()
	meta := &sessionMeta{Chunks: []chunk{{Offset: 0, Length: 3, CRC32: crc([]byte("abc"))}}}

	// The staged file is missing altogether.
	err := repairOrDiscard(filepath.Join(dir, "missing.partial"), filepath.Join(dir, "m.json"), meta, errors.New("bad zip"))
	if err == nil || !strings.Contains(err.Error(), "could not be re-examined") {
		t.Errorf("missing staging: %v", err)
	}

	// A chunk cannot be read back and the rewind fails too (a directory stands in for the file).
	asDir := filepath.Join(dir, "dir.partial")
	os.Mkdir(asDir, 0o755)
	err = repairOrDiscard(asDir, filepath.Join(dir, "m.json"), meta, errors.New("bad zip"))
	if err == nil || !strings.Contains(err.Error(), "rewinding the session failed") {
		t.Errorf("rewind failure: %v", err)
	}
	if got := (&CorruptUploadError{ResumeOffset: 7}).Error(); !strings.Contains(got, "7") {
		t.Errorf("message: %q", got)
	}
	if chunksBefore(nil, 5) != nil || len(chunksBefore([]chunk{{Offset: 0}, {Offset: 9}}, 5)) != 1 {
		t.Error("chunksBefore")
	}
}

func TestArchiveTestingAndExtractionErrors(t *testing.T) {
	m, _ := newMgr(t)
	dir := t.TempDir()

	notZip := filepath.Join(dir, "x.zip")
	os.WriteFile(notZip, []byte("nope"), 0o644)
	if err := m.testArchive("x.zip", notZip); err == nil || !strings.Contains(err.Error(), "opening zip") {
		t.Errorf("not a zip: %v", err)
	}
	if err := extractZip(notZip, dir, 1<<20); err == nil || !strings.Contains(err.Error(), "opening zip") {
		t.Errorf("extract a non-zip: %v", err)
	}

	// An entry that uses a compression method nobody implements cannot be read.
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	if _, err := w.CreateHeader(&zip.FileHeader{Name: "dir/", Method: zip.Store}); err != nil { // a folder entry, skipped by the test
		t.Fatal(err)
	}
	raw, err := w.CreateRaw(&zip.FileHeader{Name: "weird.mp3", Method: 99, CompressedSize64: 3, UncompressedSize64: 3})
	if err != nil {
		t.Fatal(err)
	}
	raw.Write([]byte("xyz"))
	w.Close()
	weird := filepath.Join(dir, "weird.zip")
	os.WriteFile(weird, buf.Bytes(), 0o644)
	if err := m.testArchive("weird.zip", weird); err == nil || !strings.Contains(err.Error(), "reading zip entry") {
		t.Errorf("unreadable entry (test): %v", err)
	}
	if err := extractZip(weird, filepath.Join(dir, "out1"), 1<<20); err == nil || !strings.Contains(err.Error(), "reading zip entry") {
		t.Errorf("unreadable entry (extract): %v", err)
	}

	// A zip whose entry data is corrupt fails its checksum while being read.
	good := zipOf(t, map[string][]byte{"a.mp3": append(append([]byte{}, mp3...), bytes.Repeat([]byte("x"), 200)...)})
	good[60] ^= 0xFF
	broken := filepath.Join(dir, "broken.zip")
	os.WriteFile(broken, good, 0o644)
	if err := m.testArchive("broken.zip", broken); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Errorf("corrupt entry (test): %v", err)
	}
	if err := extractZip(broken, filepath.Join(dir, "out2"), 1<<20); err == nil || !strings.Contains(err.Error(), "writing") {
		t.Errorf("corrupt entry (extract): %v", err)
	}
}

func TestExtractZipFileSystemFailures(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	os.WriteFile(blocker, []byte("x"), 0o644)

	// A folder entry where a file already sits.
	folder := zipOf(t, map[string][]byte{"blocker/": nil})
	p := filepath.Join(dir, "folder.zip")
	os.WriteFile(p, folder, 0o644)
	if err := extractZip(p, dir, 1<<20); err == nil {
		t.Error("cannot create a folder over a file")
	}

	// A file whose parent folder cannot be created.
	nested := zipOf(t, map[string][]byte{"blocker/a.mp3": mp3})
	os.WriteFile(p, nested, 0o644)
	if err := extractZip(p, dir, 1<<20); err == nil {
		t.Error("cannot create a parent folder over a file")
	}

	// A file whose target is already a folder.
	os.Mkdir(filepath.Join(dir, "taken.mp3"), 0o755)
	clash := zipOf(t, map[string][]byte{"taken.mp3": mp3})
	os.WriteFile(p, clash, 0o644)
	if err := extractZip(p, dir, 1<<20); err == nil || !strings.Contains(err.Error(), "writing") {
		t.Errorf("target is a folder: %v", err)
	}
}

// ---------------------------------------------------------------- 7z (through a fake binary)

func fake7z(files map[string][]byte, failOn string, calls *[]string) ExecFunc {
	return func(name string, args ...string) ([]byte, error) {
		*calls = append(*calls, name+" "+args[0])
		switch {
		case args[0] == "t" && failOn == "t":
			return []byte("CRC Failed"), errors.New("exit status 2")
		case args[0] == "x" && failOn == "x":
			return []byte("disk full"), errors.New("exit status 2")
		case args[0] == "x":
			out := strings.TrimPrefix(args[2], "-o")
			for rel, data := range files {
				p := filepath.Join(out, rel)
				os.MkdirAll(filepath.Dir(p), 0o755)
				os.WriteFile(p, data, 0o644)
			}
		}
		return nil, nil
	}
}

func TestSevenZipUpload(t *testing.T) {
	m, root := newMgr(t)
	rel := start(t, m, "Packed")
	var calls []string
	m.exec = fake7z(map[string][]byte{"Packed/01.mp3": mp3, "Packed/art.png": []byte("png")}, "", &calls)
	payload := []byte("pretend this is a 7z archive")
	send(t, m, rel, "packed.7z", payload, 10)
	st := finish(t, m, rel, "packed.7z", len(payload))
	if st.State != Done || st.Tracks != 1 || st.Skipped != 1 {
		t.Fatalf("status: %+v", st)
	}
	if strings.Join(calls, ",") != "7z t,7z x" {
		t.Errorf("expected an integrity test then an extraction, got %v", calls)
	}
	if files := listTree(t, filepath.Join(root, "uploads", "Packed")); len(files) != 1 || files[0] != "01.mp3" {
		t.Errorf("files: %v", files)
	}
}

func TestSevenZipFailures(t *testing.T) {
	payload := []byte("pretend this is a 7z archive")
	cases := map[string]string{
		"t": "source file itself is damaged",
		"x": "7z extraction failed",
	}
	for failOn, want := range cases {
		m, _ := newMgr(t)
		rel := start(t, m, "x")
		var calls []string
		m.exec = fake7z(nil, failOn, &calls)
		send(t, m, rel, "a.7z", payload, 100)
		st := finish(t, m, rel, "a.7z", len(payload))
		if st.State != Failed || !strings.Contains(st.Error, want) {
			t.Errorf("fail on %s: %+v", failOn, st)
		}
	}
	if out, err := defaultExec("definitely-not-a-real-binary-xyz", "t"); err == nil {
		t.Errorf("the default runner must report a missing binary, got %q", out)
	}
}

// ---------------------------------------------------------------- live progress

func TestStatusOfReportsLiveExtractionProgress(t *testing.T) {
	m, _ := newMgr(t)
	rel := start(t, m, "x")
	archive := zipOf(t, map[string][]byte{"a.mp3": mp3})
	send(t, m, rel, "a.zip", archive, 4096)

	m.extractMu.Lock() // hold the extractor so the background job stays "running"
	st, err := m.Complete(rel, "a.zip", int64(len(archive)))
	if err != nil || st.State != Running || st.TotalBytes != int64(len(archive)) {
		m.extractMu.Unlock()
		t.Fatalf("complete: %+v %v", st, err)
	}
	clean, dest, _ := m.verifyDest(rel)
	key, _ := m.sessionPaths(clean, "a.zip")
	scratch := m.extractDir(dest, key)
	os.MkdirAll(scratch, 0o755)
	os.WriteFile(filepath.Join(scratch, "half.bin"), bytes.Repeat([]byte{1}, 77), 0o644)

	live, found := m.StatusOf(rel, "a.zip")
	if !found || live.State != Running || live.BytesWritten != 77 {
		m.extractMu.Unlock()
		t.Fatalf("live status: %+v %v", live, found)
	}
	// If the destination disappears mid-run the status simply has no live figure.
	os.RemoveAll(dest)
	if gone, _ := m.StatusOf(rel, "a.zip"); gone.State != Running || gone.BytesWritten != 0 {
		t.Errorf("destination gone: %+v", gone)
	}
	m.extractMu.Unlock()
	if final := awaitFinished(t, m, rel, "a.zip"); final.State == Running {
		t.Error("should finish once the extractor is free")
	}
}

func TestDirSize(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "a", "b"), 0o755)
	os.WriteFile(filepath.Join(dir, "x"), make([]byte, 10), 0o644)
	os.WriteFile(filepath.Join(dir, "a", "b", "y"), make([]byte, 5), 0o644)
	os.Symlink(filepath.Join(dir, "x"), filepath.Join(dir, "link")) // ignored if supported
	if got := dirSize(dir); got != 15 {
		t.Errorf("got %d", got)
	}
	if got := dirSize(filepath.Join(dir, "missing")); got != 0 {
		t.Errorf("missing dir: %d", got)
	}
}

// ---------------------------------------------------------------- pruning and flattening

func TestPruneKeepsOnlyRegularAudioFiles(t *testing.T) {
	dir := t.TempDir()
	for p, data := range map[string][]byte{
		"keep/a.mp3":        mp3,
		"keep/deep/b.flac":  []byte("fLaC\x00"),
		"drop/readme.txt":   []byte("x"),
		"drop/sub/c.png":    []byte("\x89PNG"),
		"odd.mp3":           []byte("not really audio, but the extension says it is"), // judged like the rest of the app: extension counts
		"disguised":         mp3, // no extension, but real audio content
	} {
		os.MkdirAll(filepath.Join(dir, filepath.Dir(p)), 0o755)
		os.WriteFile(filepath.Join(dir, p), data, 0o644)
	}
	outside := filepath.Join(t.TempDir(), "secret.mp3")
	os.WriteFile(outside, mp3, 0o644)
	linked := os.Symlink(outside, filepath.Join(dir, "link.mp3")) == nil // a symlink to real audio

	kept, skipped := prune(dir)
	wantKept, wantSkipped := 4, 3 // a.mp3, b.flac, odd.mp3, disguised | readme.txt, c.png, link.mp3
	if !linked {
		wantSkipped--
	}
	if kept != wantKept || skipped != wantSkipped {
		t.Errorf("kept %d skipped %d, want %d %d", kept, skipped, wantKept, wantSkipped)
	}
	if exists(filepath.Join(dir, "drop")) {
		t.Error("folders emptied by pruning must be removed")
	}
	if linked && exists(filepath.Join(dir, "link.mp3")) {
		t.Error("symlinks must never survive: they could point outside the library")
	}
	if !exists(outside) {
		t.Error("pruning must not follow a symlink and delete its target")
	}
	if kept, skipped := prune(filepath.Join(dir, "does-not-exist")); kept != 0 || skipped != 0 {
		t.Errorf("missing dir: %d %d", kept, skipped)
	}
}

func TestFlattenWrapper(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "A", "B"), 0o755)
	os.WriteFile(filepath.Join(dir, "A", "B", "x.mp3"), mp3, 0o644)
	os.WriteFile(filepath.Join(dir, "A", "B", "y.mp3"), mp3, 0o644)
	flattenWrapper(dir)
	if files := listTree(t, dir); len(files) != 2 || files[0] != "x.mp3" || files[1] != "y.mp3" {
		t.Errorf("two wrappers should be removed: %v", files)
	}

	// Several entries at the top: nothing to unwrap.
	multi := t.TempDir()
	os.MkdirAll(filepath.Join(multi, "A"), 0o755)
	os.WriteFile(filepath.Join(multi, "A", "x.mp3"), mp3, 0o644)
	os.WriteFile(filepath.Join(multi, "y.mp3"), mp3, 0o644)
	flattenWrapper(multi)
	if !exists(filepath.Join(multi, "A", "x.mp3")) {
		t.Error("a folder next to a file is not a wrapper")
	}

	// "Album/Album": the inner folder would collide with its own wrapper, so it is left alone.
	clash := t.TempDir()
	os.MkdirAll(filepath.Join(clash, "Album", "Album"), 0o755)
	os.WriteFile(filepath.Join(clash, "Album", "Album", "z.mp3"), mp3, 0o644)
	flattenWrapper(clash)
	if !exists(filepath.Join(clash, "Album", "Album", "z.mp3")) && !exists(filepath.Join(clash, "Album", "z.mp3")) {
		t.Error("files must never be lost while flattening")
	}

	// A missing directory and a file are no-ops.
	flattenWrapper(filepath.Join(dir, "missing"))
	f := filepath.Join(t.TempDir(), "file")
	os.WriteFile(f, nil, 0o644)
	flattenWrapper(f)
}

// ---------------------------------------------------------------- housekeeping

func backdate(t *testing.T, path string, age time.Duration) {
	t.Helper()
	old := time.Now().Add(-age)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
}

func TestPurgeExpiredRemovesOnlyAbandonedSessions(t *testing.T) {
	m, root := newMgr(t)
	rel := start(t, m, "x")
	m.Begin(rel, "old.mp3", 10)
	m.Begin(rel, "fresh.mp3", 10)
	oldPartial, oldMeta := m.sessionPaths(rel, "old.mp3")
	freshPartial, freshMeta := m.sessionPaths(rel, "fresh.mp3")
	backdate(t, oldPartial, 72*time.Hour)
	backdate(t, oldMeta, 72*time.Hour)
	m.setStatus(oldPartial, Status{State: Corrupted})

	// An orphaned partial from a crash, a stray temp sidecar and an unrelated file.
	orphan := filepath.Join(root, "uploads", StagingDirName, strings.Repeat("a", 64)+".partial")
	os.WriteFile(orphan, []byte("x"), 0o644)
	backdate(t, orphan, 72*time.Hour)
	tmp := filepath.Join(root, "uploads", StagingDirName, strings.Repeat("b", 64)+".json.tmp")
	os.WriteFile(tmp, []byte("x"), 0o644)
	unrelated := filepath.Join(root, "uploads", StagingDirName, "notes.txt")
	os.WriteFile(unrelated, []byte("x"), 0o644)

	n, err := m.PurgeExpired(48 * time.Hour)
	if err != nil || n != 2 {
		t.Fatalf("purged %d (%v), want the old session and the orphan", n, err)
	}
	if exists(oldPartial) || exists(oldMeta) || exists(orphan) {
		t.Error("abandoned files must be gone")
	}
	if !exists(freshPartial) || !exists(freshMeta) || !exists(unrelated) {
		t.Error("recent sessions and unrelated files must stay")
	}
	if _, found := m.StatusOf(rel, "old.mp3"); found {
		t.Error("the tracked outcome of a purged session must be dropped too")
	}

	// maxAge <= 0 purges everything, including the fresh session.
	if n, _ = m.PurgeExpired(0); n != 1 || exists(freshPartial) {
		t.Errorf("force purge: %d", n)
	}
}

func TestPurgeExpiredOnMissingOrBrokenStagingFolder(t *testing.T) {
	m, root := newMgr(t)
	if n, err := m.PurgeExpired(time.Hour); n != 0 || err != nil {
		t.Errorf("nothing staged yet: %d %v", n, err)
	}
	os.MkdirAll(filepath.Join(root, "uploads"), 0o755)
	os.WriteFile(filepath.Join(root, "uploads", StagingDirName), []byte("x"), 0o644) // a file where the folder should be
	if _, err := m.PurgeExpired(time.Hour); err == nil {
		t.Error("an unreadable staging folder is an error")
	}
}

func TestJanitorSweepsAndStops(t *testing.T) {
	m, _ := newMgr(t)
	var mu sync.Mutex
	var logs []string
	m.logf = func(f string, a ...any) {
		mu.Lock()
		logs = append(logs, fmt.Sprintf(f, a...))
		mu.Unlock()
	}
	rel := start(t, m, "x")
	m.Begin(rel, "abandoned.mp3", 10)
	p, meta := m.sessionPaths(rel, "abandoned.mp3")
	backdate(t, p, 72*time.Hour)
	backdate(t, meta, 72*time.Hour)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { m.RunJanitor(ctx, 48*time.Hour, 5*time.Millisecond); close(done) }()
	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(logs) > 0 })
	time.Sleep(40 * time.Millisecond) // a few ticks with nothing left to purge
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("janitor did not stop")
	}
	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(logs[0], "purged 1") || len(logs) != 1 {
		t.Errorf("logs: %v", logs)
	}
	if exists(p) {
		t.Error("the abandoned session should be gone")
	}
}

func TestJanitorReportsErrors(t *testing.T) {
	m, root := newMgr(t)
	var mu sync.Mutex
	var logs []string
	m.logf = func(f string, a ...any) { mu.Lock(); logs = append(logs, fmt.Sprintf(f, a...)); mu.Unlock() }
	os.MkdirAll(filepath.Join(root, "uploads"), 0o755)
	os.WriteFile(filepath.Join(root, "uploads", StagingDirName), []byte("x"), 0o644)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { m.RunJanitor(ctx, time.Hour, time.Hour); close(done) }()
	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(logs) > 0 })
	cancel()
	<-done
	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(logs[0], "upload janitor:") {
		t.Errorf("logs: %v", logs)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in time")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestPlatformFreeBytes(t *testing.T) {
	if runtime.GOOS == "windows" {
		if _, err := platformFreeBytes("."); err == nil {
			t.Error("not implemented on Windows")
		}
		return
	}
	if n, err := platformFreeBytes(t.TempDir()); err != nil || n == 0 {
		t.Errorf("free bytes: %d %v", n, err)
	}
	if _, err := platformFreeBytes(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("a missing path is an error")
	}
}

func TestFormatSize(t *testing.T) {
	if got := fmtSize(3 << 30); got != "3.00 GB" {
		t.Errorf("%s", got)
	}
	if got := fmtSize(5 << 20); got != "5 MB" {
		t.Errorf("%s", got)
	}
}
