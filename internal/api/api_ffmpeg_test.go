package api

import (
	"bytes"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"masterplayer/internal/transcode"
)

var jpeg = []byte("\xff\xd8\xff\xe0 a small picture")

// withFFmpeg adds an ffmpeg service backed by a fake runner to the test server.
func withFFmpeg(t *testing.T, e *env) *fakeRunner {
	t.Helper()
	r := &fakeRunner{info: transcode.Info{
		SampleRate: 44100, Channels: 2, Title: "Windows Song", Artist: "The Artist", Album: "The Album",
		Genre: "Pop", Year: "1999", Track: "3", HasLoop: true, LoopStart: 5, LoopEnd: 50, // loops are a vgmstream thing
	}}
	svc, err := transcode.New(r, filepath.Join(t.TempDir(), "ffcache"), 1, 0, transcode.WithExt(".flac"))
	if err != nil {
		t.Fatal(err)
	}
	e.srv.FF = svc
	write(t, filepath.Join(e.root, "song.wma"), []byte("not really a wma"))
	return r
}

func TestFFmpegFormatsPlayThroughFLAC(t *testing.T) {
	e := newEnv(t, true, false)
	r := withFFmpeg(t, e)

	var sess map[string]any
	decode(t, e.do("GET", "/api/session", nil, nil), &sess)
	if sess["ffmpeg"] != true || sess["vgmstream"] != true {
		t.Errorf("session should advertise both converters: %v", sess)
	}

	var m metaResp
	decode(t, e.do("GET", "/api/meta?p=song.wma", nil, nil), &m)
	if m.Kind != "ffmpeg" || m.Native || m.Title != "Windows Song" || m.Artist != "The Artist" || m.Album != "The Album" ||
		m.Genre != "Pop" || m.Year != "1999" || m.Track != "3" || m.SampleRate != 44100 || m.Loop != nil {
		t.Fatalf("ffmpeg meta: %+v", m)
	}

	rec := e.do("GET", "/api/stream?p=song.wma", nil, nil)
	wantStatus(t, rec, 200)
	if rec.Body.String() != "RIFFfakewavdata" || rec.Header().Get("Content-Type") != "audio/flac" {
		t.Errorf("converted stream: %q %v", rec.Body, rec.Header())
	}
	rec = e.do("GET", "/api/stream?p=song.wma", nil, nil, func(req *http.Request) { req.Header.Set("Range", "bytes=0-3") })
	wantStatus(t, rec, 206)
	if rec.Body.String() != "RIFF" || atomic.LoadInt32(&r.decodes) != 1 {
		t.Errorf("range of the cached conversion: %q decodes=%d", rec.Body, r.decodes)
	}

	// Game formats still go to vgmstream, never to ffmpeg, even when the browser asks for a conversion.
	rec = e.do("GET", "/api/stream?p=game.brstm&transcode=1", nil, nil)
	if rec.Header().Get("Content-Type") != "audio/wav" || atomic.LoadInt32(&e.runner.decodes) != 1 {
		t.Errorf("vgm: %v decodes=%d", rec.Header(), e.runner.decodes)
	}
	// A browser that cannot play a native file (Opus on old Safari) gets ffmpeg's FLAC instead.
	rec = e.do("GET", "/api/stream?p=a.mp3&transcode=1", nil, nil)
	if rec.Header().Get("Content-Type") != "audio/flac" || atomic.LoadInt32(&r.decodes) != 2 {
		t.Errorf("transcode=1: %v decodes=%d", rec.Header(), r.decodes)
	}
	// Without ffmpeg the same request falls back to vgmstream's converter.
	ff := e.srv.FF
	e.srv.FF = nil
	rec = e.do("GET", "/api/stream?p=b.mp3&transcode=1", nil, nil)
	if rec.Header().Get("Content-Type") != "audio/wav" {
		t.Errorf("fallback converter: %v", rec.Header())
	}
	e.srv.FF = ff

	// Prefetch warms the right cache.
	write(t, filepath.Join(e.root, "next.wma"), []byte("x"))
	wantStatus(t, e.do("POST", "/api/prefetch", map[string]string{"path": "next.wma"}, nil), 202)
	for deadline := time.Now().Add(2 * time.Second); atomic.LoadInt32(&r.decodes) < 3 && time.Now().Before(deadline); {
		time.Sleep(5 * time.Millisecond)
	}
	if atomic.LoadInt32(&r.decodes) != 3 {
		t.Errorf("prefetch should convert next.wma, decodes=%d", r.decodes)
	}

	// Broken files are reported as undecodable, not as server errors.
	svc, _ := transcode.New(&fakeRunner{fail: true}, filepath.Join(t.TempDir(), "bad"), 1, 0, transcode.WithExt(".flac"))
	e.srv.FF = svc
	wantStatus(t, e.do("GET", "/api/meta?p=song.wma", nil, nil), 422)
	wantStatus(t, e.do("GET", "/api/stream?p=song.wma", nil, nil), 422)
}

func TestFFmpegFormatsWithoutFFmpeg(t *testing.T) {
	e := newEnv(t, true, false)
	write(t, filepath.Join(e.root, "song.wma"), []byte("x"))
	for _, p := range []string{"/api/meta?p=song.wma", "/api/stream?p=song.wma"} {
		rec := e.do("GET", p, nil, nil)
		wantStatus(t, rec, 503)
		if !strings.Contains(rec.Body.String(), "ffmpeg is not installed") {
			t.Errorf("%s should say what is missing: %s", p, rec.Body)
		}
	}
	wantStatus(t, e.do("POST", "/api/prefetch", map[string]string{"path": "song.wma"}, nil), 202)
	var sess map[string]any
	decode(t, e.do("GET", "/api/session", nil, nil), &sess)
	if sess["ffmpeg"] != false {
		t.Errorf("session: %v", sess)
	}
}

func TestArtFallsBackToThePictureInTheAlbumFolder(t *testing.T) {
	e := newEnv(t, false, false)
	write(t, filepath.Join(e.root, "Album", "01.mp3"), mp3With("One", nil))
	write(t, filepath.Join(e.root, "Album", "cover.jpg"), jpeg)
	write(t, filepath.Join(e.root, "Fake", "01.mp3"), mp3With("Two", nil))
	write(t, filepath.Join(e.root, "Fake", "cover.jpg"), []byte("<svg onload=alert(1)>"))
	write(t, filepath.Join(e.root, "Embedded", "01.mp3"), mp3With("Three", png))
	write(t, filepath.Join(e.root, "Embedded", "cover.jpg"), jpeg)

	rec := e.do("GET", "/api/art?p=Album/01.mp3", nil, nil)
	wantStatus(t, rec, 200)
	if rec.Header().Get("Content-Type") != "image/jpeg" || !bytes.Equal(rec.Body.Bytes(), jpeg) ||
		!strings.Contains(rec.Header().Get("Content-Security-Policy"), "sandbox") {
		t.Errorf("folder art: %v", rec.Header())
	}
	var m metaResp
	decode(t, e.do("GET", "/api/meta?p=Album/01.mp3", nil, nil), &m)
	if !m.HasArt {
		t.Error("a track with a folder picture has art")
	}
	// Embedded art still wins, and a "picture" that is really markup is never served.
	rec = e.do("GET", "/api/art?p=Embedded/01.mp3", nil, nil)
	if rec.Header().Get("Content-Type") != "image/png" {
		t.Errorf("embedded art should win: %v", rec.Header())
	}
	wantStatus(t, e.do("GET", "/api/art?p=Fake/01.mp3", nil, nil), 404)
	wantStatus(t, e.do("GET", "/api/art?p=b.mp3", nil, nil), 404)
}

func TestUploadReportListsTheSkippedFiles(t *testing.T) {
	e := newEnv(t, false, false)
	rel := e.startBatch("Packed")
	archive := zipBytes(t, map[string][]byte{
		"Album/01.mp3": mp3With("One", nil), "Album/cover.jpg": jpeg,
		"Album/notes.txt": []byte("liner notes"), "Album/scan.log": []byte("EAC"),
	})
	st := e.uploadFile(rel, "packed.zip", archive, 256)
	if st.Tracks != 1 || st.Images != 1 || st.Skipped != 2 || !st.HasReport || st.SkippedTypes["txt"] != 1 || st.SkippedTypes["log"] != 1 {
		t.Fatalf("status: %+v", st)
	}
	q := url.Values{"relPath": {rel}, "filename": {"packed.zip"}}.Encode()
	rec := e.do("GET", "/api/upload/report?"+q, nil, nil)
	wantStatus(t, rec, 200)
	body := rec.Body.String()
	if !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/plain") || rec.Header().Get("X-Content-Type-Options") != "nosniff" ||
		!strings.Contains(body, "notes.txt") || !strings.Contains(body, "scan.log") || strings.Contains(body, "cover.jpg") {
		t.Errorf("report: %v\n%s", rec.Header(), body)
	}
	wantStatus(t, e.do("GET", "/api/upload/report?relPath="+url.QueryEscape(rel)+"&filename=other.zip", nil, nil), 404)
	wantStatus(t, e.do("GET", "/api/upload/report?relPath="+url.QueryEscape(rel)+"&filename=..%2Fx", nil, nil), 400)

	// The picture became the album's cover, and the listing hides it.
	var tr struct {
		Tracks []entryDTO `json:"tracks"`
	}
	decode(t, e.do("GET", "/api/tracks?dir=uploads", nil, nil), &tr)
	if len(tr.Tracks) != 1 {
		t.Fatalf("tracks: %+v", tr.Tracks)
	}
	wantStatus(t, e.do("GET", "/api/art?p="+url.QueryEscape(tr.Tracks[0].Path), nil, nil), 200)
}
