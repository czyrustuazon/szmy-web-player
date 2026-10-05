package api

import (
	"archive/zip"
	"bytes"
	"fmt"
	"hash/crc32"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"masterplayer/internal/upload"
)

func crcHex(b []byte) string { return fmt.Sprintf("%08x", crc32.ChecksumIEEE(b)) }

func (e *env) chunk(rel, name string, offset int64, data []byte, crc string) *httptest.ResponseRecorder {
	e.t.Helper()
	q := url.Values{"relPath": {rel}, "filename": {name}, "offset": {fmt.Sprint(offset)}}
	req := httptest.NewRequest("POST", "/api/upload/chunk?"+q.Encode(), bytes.NewReader(data))
	req.Header.Set(csrfHeader, csrfValue)
	if crc != "" {
		req.Header.Set("X-Chunk-CRC32", crc)
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec
}

// uploadFile runs the whole protocol over HTTP and returns the final status.
func (e *env) uploadFile(rel, name string, data []byte, chunkSize int) upload.Status {
	e.t.Helper()
	var begin map[string]int64
	decode(e.t, e.do("POST", "/api/upload/begin", map[string]any{"relPath": rel, "filename": name, "size": len(data)}, nil), &begin)
	off := begin["offset"]
	for off < int64(len(data)) {
		part := data[off:min(off+int64(chunkSize), int64(len(data)))]
		rec := e.chunk(rel, name, off, part, crcHex(part))
		wantStatus(e.t, rec, 200)
		var out map[string]int64
		decode(e.t, rec, &out)
		off = out["offset"]
	}
	var st upload.Status
	decode(e.t, e.do("POST", "/api/upload/complete", map[string]any{"relPath": rel, "filename": name, "size": len(data)}, nil), &st)
	for deadline := time.Now().Add(10 * time.Second); st.State == upload.Running && time.Now().Before(deadline); {
		time.Sleep(5 * time.Millisecond)
		q := url.Values{"relPath": {rel}, "filename": {name}}
		decode(e.t, e.do("GET", "/api/upload/status?"+q.Encode(), nil, nil), &st)
	}
	return st
}

func (e *env) startBatch(title string) string {
	e.t.Helper()
	var out map[string]string
	decode(e.t, e.do("POST", "/api/upload/start", map[string]string{"title": title}, nil), &out)
	return out["relPath"]
}

func zipBytes(t *testing.T, entries map[string][]byte) []byte {
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
	w.Close()
	return buf.Bytes()
}

func TestChunkedUploadOverHTTP(t *testing.T) {
	e := newEnv(t, false, true)
	c := e.login()
	rec := e.do("POST", "/api/upload/start", map[string]string{"title": "My Album"}, c)
	wantStatus(t, rec, 200)
	var started map[string]string
	decode(t, rec, &started)
	if started["name"] != "My Album" || started["relPath"] != "uploads/My Album" {
		t.Fatalf("start: %v", started)
	}

	// Cookie-authenticated flow, in small chunks.
	data := mp3With("Uploaded", png)
	var begin map[string]int64
	decode(t, e.do("POST", "/api/upload/begin", map[string]any{"relPath": started["relPath"], "filename": "My Song.mp3", "size": len(data)}, c), &begin)
	if begin["offset"] != 0 {
		t.Fatalf("fresh offset: %v", begin)
	}
	for off := int64(0); off < int64(len(data)); {
		part := data[off:min(off+40, int64(len(data)))]
		req := httptest.NewRequest("POST", fmt.Sprintf("/api/upload/chunk?relPath=%s&filename=%s&offset=%d", "uploads/My%20Album", "My%20Song.mp3", off), bytes.NewReader(part))
		req.Header.Set(csrfHeader, csrfValue)
		req.Header.Set("X-Chunk-CRC32", crcHex(part))
		req.AddCookie(c)
		rr := httptest.NewRecorder()
		e.h.ServeHTTP(rr, req)
		wantStatus(t, rr, 200)
		var out map[string]int64
		decode(t, rr, &out)
		off = out["offset"]
	}
	var st upload.Status
	decode(t, e.do("POST", "/api/upload/complete", map[string]any{"relPath": started["relPath"], "filename": "My Song.mp3", "size": len(data)}, c), &st)
	if st.State != upload.Done || st.Tracks != 1 || st.Path != "uploads/My Album/My Song.mp3" {
		t.Fatalf("complete: %+v", st)
	}
	if got, _ := os.ReadFile(filepath.Join(e.root, "uploads", "My Album", "My Song.mp3")); !bytes.Equal(got, data) {
		t.Error("uploaded content differs")
	}
	// The new track is immediately visible and playable.
	var m metaResp
	decode(t, e.do("GET", "/api/meta?p=uploads/My%20Album/My%20Song.mp3", nil, c), &m)
	if m.Title != "Uploaded" || !m.HasArt {
		t.Errorf("meta: %+v", m)
	}
	// Status of a finished file is still answerable (a reload can ask).
	var again upload.Status
	decode(t, e.do("GET", "/api/upload/status?relPath=uploads/My%20Album&filename=My%20Song.mp3", nil, c), &again)
	if !reflect.DeepEqual(again, st) {
		t.Errorf("status: %+v", again)
	}
}

func TestUploadEndpointsRequireLoginAndTheCSRFHeader(t *testing.T) {
	e := newEnv(t, false, true)
	for _, u := range []string{"/api/upload/start", "/api/upload/begin", "/api/upload/chunk?offset=0", "/api/upload/complete"} {
		wantStatus(t, e.do("POST", u, "{}", nil), 401)
		wantStatus(t, e.do("POST", u, "{}", e.login(), func(r *http.Request) { r.Header.Del(csrfHeader) }), 403)
	}
	wantStatus(t, e.do("GET", "/api/upload/status?relPath=a&filename=b", nil, nil), 401)
}

func TestStartCanJoinAnExistingFolder(t *testing.T) {
	e := newEnv(t, false, false)
	start := func(body map[string]any) string {
		var out map[string]string
		decode(t, e.do("POST", "/api/upload/start", body, nil), &out)
		return out["relPath"]
	}
	first := start(map[string]any{"title": "Album"})
	if again := start(map[string]any{"title": "Album", "merge": true}); again != first {
		t.Errorf("merge joins the folder: %s vs %s", again, first)
	}
	if other := start(map[string]any{"title": "Album"}); other == first {
		t.Errorf("without merge a new folder is made: %s", other)
	}
}

func TestUploadResumesAfterAnInterruption(t *testing.T) {
	e := newEnv(t, false, false)
	rel := e.startBatch("Resumable")
	data := mp3With("Big", nil)
	data = append(data, bytes.Repeat([]byte{0xFF, 0xFB, 0x90, 0}, 100)...)

	var begin map[string]int64
	decode(t, e.do("POST", "/api/upload/begin", map[string]any{"relPath": rel, "filename": "big.mp3", "size": len(data)}, nil), &begin)
	wantStatus(t, e.chunk(rel, "big.mp3", 0, data[:200], crcHex(data[:200])), 200)

	// "Reload": begin again reports where the server really is; a stale chunk is refused with that offset.
	decode(t, e.do("POST", "/api/upload/begin", map[string]any{"relPath": rel, "filename": "big.mp3", "size": len(data)}, nil), &begin)
	if begin["offset"] != 200 {
		t.Fatalf("resume offset: %v", begin)
	}
	rec := e.chunk(rel, "big.mp3", 0, data[:200], crcHex(data[:200]))
	wantStatus(t, rec, 409)
	var conflict map[string]int64
	decode(t, rec, &conflict)
	if conflict["offset"] != 200 {
		t.Errorf("conflict body: %v", conflict)
	}
	if st := e.uploadFile(rel, "big.mp3", data, 300); st.State != upload.Done {
		t.Fatalf("finishing from the resume point: %+v", st)
	}
}

func TestArchiveUploadExtractsAudioOnly(t *testing.T) {
	e := newEnv(t, false, false)
	rel := e.startBatch("From Zip")
	archive := zipBytes(t, map[string][]byte{
		"Album/01.mp3": mp3With("One", nil), "Album/02.mp3": mp3With("Two", nil), "Album/cover.jpg": []byte("jpg"),
	})
	st := e.uploadFile(rel, "album.zip", archive, 128)
	if st.State != upload.Done || st.Tracks != 2 || st.Skipped != 1 {
		t.Fatalf("status: %+v", st)
	}
	var tr struct {
		Tracks []entryDTO `json:"tracks"`
	}
	decode(t, e.do("GET", "/api/tracks?dir=uploads", nil, nil), &tr)
	if len(tr.Tracks) != 2 || !strings.HasPrefix(tr.Tracks[0].Path, "uploads/From Zip/") {
		t.Errorf("tracks: %+v", tr.Tracks)
	}
}

func TestFailedUploadsAreReportedAndLogged(t *testing.T) {
	e := newEnv(t, false, false)
	rel := e.startBatch("Bad")

	// A loose file that is not audio fails immediately, at complete.
	text := []byte("plain text")
	if st := e.uploadFile(rel, "notes.txt", text, 100); st.State != upload.Failed || !strings.Contains(st.Error, "not an audio file") {
		t.Fatalf("loose text: %+v", st)
	}
	// An archive with no audio fails in the background and is seen by polling status.
	archive := zipBytes(t, map[string][]byte{"readme.txt": []byte("x")})
	if st := e.uploadFile(rel, "empty.zip", archive, 100); st.State != upload.Failed || !strings.Contains(st.Error, "no audio files") {
		t.Fatalf("archive: %+v", st)
	}
	lines, _ := e.logs.Recent(20)
	all := strings.Join(lines, "\n")
	if !strings.Contains(all, "site=upload-complete") || !strings.Contains(all, "site=upload-status") {
		t.Errorf("failures should be logged from both endpoints:\n%s", all)
	}
}

func TestUploadErrorStatusCodes(t *testing.T) {
	e := newEnv(t, false, false)
	rel := e.startBatch("x")
	begin := func(relPath, name string, size int64) *httptest.ResponseRecorder {
		return e.do("POST", "/api/upload/begin", map[string]any{"relPath": relPath, "filename": name, "size": size}, nil)
	}
	wantStatus(t, begin("music/elsewhere", "a.mp3", 1), 400) // outside the upload folder
	wantStatus(t, begin("uploads/missing", "a.mp3", 1), 404) // no such folder
	wantStatus(t, begin(rel, "a/b.mp3", 1), 400)             // bad name
	wantStatus(t, begin(rel, "a.mp3", -1), 400)              // bad size
	wantStatus(t, e.do("POST", "/api/upload/begin", "{", nil), 400)
	wantStatus(t, e.do("POST", "/api/upload/start", "{", nil), 400)
	wantStatus(t, e.do("POST", "/api/upload/complete", "{", nil), 400)
	wantStatus(t, e.do("POST", "/api/upload/complete", map[string]any{"relPath": rel, "filename": "never.mp3", "size": 1}, nil), 400)

	// Too large: a manager with a tiny limit.
	e.srv.Up = upload.New(e.srv.Lib.Root(), "uploads", 100, 0)
	wantStatus(t, begin(rel, "a.mp3", 101), 413)

	// Not enough free space: demand more free space than any disk has.
	if runtime.GOOS != "windows" {
		e.srv.Up = upload.New(e.srv.Lib.Root(), "uploads", 1<<40, 1<<62)
		wantStatus(t, begin(rel, "a.mp3", 10), 507)
	}
	// 7z needs the 7z binary; absent in the test container.
	e.srv.Up = upload.New(e.srv.Lib.Root(), "uploads", 1<<30, 0)
	if !e.srv.Up.SevenZipAvailable() {
		wantStatus(t, begin(rel, "a.7z", 10), 415)
	}
	// Upload folder cannot be created: a file is in the way.
	os.RemoveAll(filepath.Join(e.root, "uploads"))
	os.WriteFile(filepath.Join(e.root, "uploads"), []byte("x"), 0o644)
	wantStatus(t, e.do("POST", "/api/upload/start", map[string]string{"title": "y"}, nil), 500)
}

func TestUploadChunkRejectsMalformedRequests(t *testing.T) {
	e := newEnv(t, false, false)
	rel := e.startBatch("x")
	e.do("POST", "/api/upload/begin", map[string]any{"relPath": rel, "filename": "a.mp3", "size": 10}, nil)
	good := []byte("abc")

	wantStatus(t, e.chunk(rel, "a.mp3", 0, good, ""), 400)                      // no CRC header
	wantStatus(t, e.chunk(rel, "a.mp3", 0, good, "zzzzzzzz"), 400)              // not hex
	wantStatus(t, e.chunk(rel, "a.mp3", 0, good, "abcd"), 400)                  // wrong length
	wantStatus(t, e.chunk(rel, "a.mp3", 0, good, "deadbeef"), 400)              // wrong checksum
	wantStatus(t, e.chunk(rel, "a.mp3", 0, bytes.Repeat([]byte{1}, 11), "abcd1234"), 400) // past the declared size
	wantStatus(t, e.chunk(rel, "nosession.mp3", 0, good, crcHex(good)), 400)    // never begun
	rec := e.chunk(rel, "a.mp3", 0, good, crcHex(good))
	wantStatus(t, rec, 200)

	// A bad or missing offset.
	req := httptest.NewRequest("POST", "/api/upload/chunk?relPath=uploads&filename=a.mp3&offset=-1", bytes.NewReader(good))
	req.Header.Set(csrfHeader, csrfValue)
	rr := httptest.NewRecorder()
	e.h.ServeHTTP(rr, req)
	wantStatus(t, rr, 400)
	req = httptest.NewRequest("POST", "/api/upload/chunk?relPath=uploads&filename=a.mp3", bytes.NewReader(good))
	req.Header.Set(csrfHeader, csrfValue)
	rr = httptest.NewRecorder()
	e.h.ServeHTTP(rr, req)
	wantStatus(t, rr, 400)

	// A chunk bigger than the server cap is refused before it is stored.
	old := upload.MaxChunkBytes
	upload.MaxChunkBytes = 4
	t.Cleanup(func() { upload.MaxChunkBytes = old })
	wantStatus(t, e.chunk(rel, "a.mp3", 3, bytes.Repeat([]byte{1}, 5), crcHex(bytes.Repeat([]byte{1}, 5))), 400)
}

func TestUploadStatusForUnknownFileIs404(t *testing.T) {
	e := newEnv(t, false, false)
	wantStatus(t, e.do("GET", "/api/upload/status?relPath=uploads&filename=nothing.zip", nil, nil), 404)
}

func TestSessionAdvertisesUploadLimits(t *testing.T) {
	e := newEnv(t, false, false)
	e.srv.Cfg.MaxUploadMB = 123
	var sess map[string]any
	decode(t, e.do("GET", "/api/session", nil, nil), &sess)
	if sess["maxUploadMB"] != float64(123) || sess["uploadDir"] != "uploads" {
		t.Errorf("session: %v", sess)
	}
	if _, ok := sess["sevenZip"].(bool); !ok {
		t.Errorf("sevenZip flag missing: %v", sess)
	}
}
