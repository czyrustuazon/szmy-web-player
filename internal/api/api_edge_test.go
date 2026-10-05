package api

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"masterplayer/internal/access"
	"masterplayer/internal/errlog"
	"masterplayer/internal/transcode"
)

func TestReadOnlyLibraryDisablesWrites(t *testing.T) {
	e := newEnvOpts(t, false, false, true)
	var sess map[string]any
	decode(t, e.do("GET", "/api/session", nil, nil), &sess)
	if sess["canDelete"] != false || sess["canUpload"] != false {
		t.Errorf("session should advertise read-only: %v", sess)
	}
	wantStatus(t, e.do("DELETE", "/api/track?p=a.mp3", nil, nil), 403)
	wantStatus(t, e.do("POST", "/api/undo", map[string]string{"token": "0123456789abcdef"}, nil), 403)

	// Every step of the chunked upload is refused, and nothing is created.
	wantStatus(t, e.do("POST", "/api/upload/start", map[string]string{"title": "x"}, nil), 403)
	wantStatus(t, e.do("POST", "/api/upload/begin", map[string]any{"relPath": "uploads", "filename": "a.mp3", "size": 1}, nil), 403)
	wantStatus(t, e.do("POST", "/api/upload/chunk?relPath=uploads&filename=a.mp3&offset=0", "x", nil), 403)
	wantStatus(t, e.do("POST", "/api/upload/complete", map[string]any{"relPath": "uploads", "filename": "a.mp3", "size": 1}, nil), 403)
	if _, err := os.Stat(filepath.Join(e.root, "uploads")); err == nil {
		t.Error("a read-only library must not gain an upload folder")
	}

	// Reading is unaffected.
	wantStatus(t, e.do("GET", "/api/browse", nil, nil), 200)
	if _, err := os.Stat(filepath.Join(e.root, "a.mp3")); err != nil {
		t.Error("nothing may be deleted in read-only mode")
	}
}

func TestStoreWriteFailuresAreSurfaced(t *testing.T) {
	e := newEnv(t, false, false)
	wantStatus(t, e.do("POST", "/api/favorite", map[string]any{"path": "a.mp3", "on": true}, nil), 200)

	// A directory squatting on the store's temp file name makes every later save fail.
	if err := os.Mkdir(filepath.Join(e.data, "state.json.tmp"), 0o755); err != nil {
		t.Fatal(err)
	}
	wantStatus(t, e.do("POST", "/api/favorite", map[string]any{"path": "b.mp3", "on": true}, nil), 500)
	wantStatus(t, e.do("PUT", "/api/settings", map[string]any{"repeat": "all"}, nil), 500)
	wantStatus(t, e.do("PUT", "/api/resume", map[string]any{"path": "a.mp3"}, nil), 500)

	// Deleting and undoing still work; the failed favorite bookkeeping is only logged.
	rec := e.do("DELETE", "/api/track?p=a.mp3", nil, nil)
	wantStatus(t, rec, 200)
	var del map[string]string
	decode(t, rec, &del)
	wantStatus(t, e.do("POST", "/api/undo", map[string]string{"token": del["token"]}, nil), 200)

	lines, _ := e.logs.Recent(50)
	all := strings.Join(lines, "\n")
	if !strings.Contains(all, "site=delete") || !strings.Contains(all, "site=undo") {
		t.Errorf("bookkeeping failures should be logged:\n%s", all)
	}
}

func TestUnreadableTagsAreLoggedButStillPlayable(t *testing.T) {
	e := newEnv(t, false, false)
	// ID3v2.5 does not exist, so the tag reader reports an error.
	write(t, filepath.Join(e.root, "badtag.mp3"), append([]byte{'I', 'D', '3', 5, 0, 0, 0, 0, 0, 0}, 0xFF, 0xFB, 0x90, 0))
	var m metaResp
	decode(t, e.do("GET", "/api/meta?p=badtag.mp3", nil, nil), &m)
	if m.Title != "badtag" || m.Kind != "mp3" {
		t.Fatalf("falls back to the file name: %+v", m)
	}
	lines, _ := e.logs.Recent(10)
	if len(lines) != 1 || !strings.Contains(lines[0], "site=meta") || !strings.Contains(lines[0], "ID3v2.5") {
		t.Fatalf("tag error should be logged: %v", lines)
	}
}

func TestCancelledRequestsAreNotReportedAsFailures(t *testing.T) {
	e := newEnv(t, true, false)
	e.runner.fail = true
	e.srv.TX, _ = transcode.New(e.runner, filepath.Join(t.TempDir(), "cache"), 1, 0)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the client has already gone away

	for _, url := range []string{"/api/meta?p=game.brstm", "/api/stream?p=game.brstm"} {
		req := httptest.NewRequest("GET", url, nil).WithContext(ctx)
		rec := httptest.NewRecorder()
		e.h.ServeHTTP(rec, req)
		if rec.Body.Len() != 0 {
			t.Errorf("%s: nothing should be written to a client that left: %q", url, rec.Body)
		}
	}
	if lines, _ := e.logs.Recent(10); len(lines) != 0 {
		t.Errorf("a client disconnect is not an error: %v", lines)
	}
}

func TestServeFileRefusesMissingFilesAndDirectories(t *testing.T) {
	for name, p := range map[string]string{
		"missing":   filepath.Join(t.TempDir(), "nope.mp3"),
		"directory": t.TempDir(),
	} {
		rec := httptest.NewRecorder()
		serveFile(rec, httptest.NewRequest("GET", "/x", nil), p, "audio/mpeg")
		if rec.Code != 404 {
			t.Errorf("%s: status %d", name, rec.Code)
		}
	}
}

func TestErrorLogEndpointReportsReadFailures(t *testing.T) {
	e := newEnv(t, false, false)
	e.srv.Log = errlog.New(e.data, 0, nil) // a directory cannot be read as a log file
	wantStatus(t, e.do("GET", "/api/errors", nil, nil), 500)
}

func TestUploadsFollowTheUploadFolderNotTheLibraryRoot(t *testing.T) {
	e := newEnv(t, false, false)
	var sess map[string]any
	decode(t, e.do("GET", "/api/session", nil, nil), &sess)
	if sess["canUpload"] != true || sess["canDelete"] != true {
		t.Fatalf("session: %v", sess)
	}

	// The upload folder is unusable (a file sits there): uploads are off, deleting still works.
	os.RemoveAll(filepath.Join(e.root, "uploads")) // the probe above created it
	os.WriteFile(filepath.Join(e.root, "uploads"), []byte("x"), 0o644)
	decode(t, e.do("GET", "/api/session", nil, nil), &sess)
	if sess["canUpload"] != false || sess["canDelete"] != true {
		t.Errorf("an unusable upload folder only disables uploads: %v", sess)
	}
	// Bypassing the UI gets a plain failure (nothing can be created there), not a crash.
	wantStatus(t, e.do("POST", "/api/upload/start", map[string]string{"title": "x"}, nil), 500)
}

func TestAccessPolicyGuardsEverythingIncludingStaticFilesAndHealth(t *testing.T) {
	e := newEnv(t, false, false)
	nets, err := access.ParseNets("tailscale,192.168.1.0/24")
	if err != nil {
		t.Fatal(err)
	}
	e.srv.Access = access.New(nets, nil, nil, func(string, ...any) {})
	h := e.srv.Handler()

	get := func(path, remote string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", path, nil)
		req.RemoteAddr = remote
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	// httptest's default peer is 192.0.2.1, a public (documentation) address.
	for _, path := range []string{"/", "/api/session", "/api/browse", "/api/stream?p=a.mp3", "/healthz", "/sw.js"} {
		if rec := get(path, "203.0.113.50:5555"); rec.Code != 403 {
			t.Errorf("%s from the public internet: %d", path, rec.Code)
		}
	}
	for _, remote := range []string{"100.114.200.30:1", "192.168.1.20:1", "127.0.0.1:1"} {
		if rec := get("/api/session", remote); rec.Code != 200 {
			t.Errorf("%s should be let in: %d", remote, rec.Code)
		}
	}
	if rec := get("/api/session", "192.168.2.20:1"); rec.Code != 403 {
		t.Errorf("a home subnet that was not listed: %d", rec.Code)
	}
	// Writes are refused as well, before the CSRF check or anything else runs.
	req := httptest.NewRequest("DELETE", "/api/track?p=a.mp3", nil)
	req.RemoteAddr = "203.0.113.50:1"
	req.Header.Set(csrfHeader, csrfValue)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 403 {
		t.Errorf("delete from outside: %d", rec.Code)
	}
	if _, err := os.Stat(filepath.Join(e.root, "a.mp3")); err != nil {
		t.Error("nothing may be deleted by a refused client")
	}
}
