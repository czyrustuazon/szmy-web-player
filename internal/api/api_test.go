package api

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io/fs"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"masterplayer/internal/auth"
	"masterplayer/internal/config"
	"masterplayer/internal/errlog"
	"masterplayer/internal/library"
	"masterplayer/internal/store"
	"masterplayer/internal/transcode"
	"masterplayer/internal/upload"
)

// ---------------------------------------------------------------- fixtures

var png = append([]byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}, bytes.Repeat([]byte{7}, 32)...)

func ssBytes(n int) []byte {
	return []byte{byte(n>>21) & 0x7f, byte(n>>14) & 0x7f, byte(n>>7) & 0x7f, byte(n) & 0x7f}
}

func frame(id string, data []byte) []byte {
	size := make([]byte, 4)
	binary.BigEndian.PutUint32(size, uint32(len(data)))
	return append(append(append([]byte(id), size...), 0, 0), data...)
}

// mp3With builds a tiny MP3 with an ID3v2.3 title and optional cover.
func mp3With(title string, art []byte) []byte {
	frames := frame("TIT2", append([]byte{0}, title...))
	if art != nil {
		d := append([]byte{0}, "image/png"...)
		d = append(d, 0, 3, 0)
		frames = append(frames, frame("APIC", append(d, art...))...)
	}
	out := append([]byte{'I', 'D', '3', 3, 0, 0}, ssBytes(len(frames))...)
	out = append(out, frames...)
	return append(out, bytes.Repeat([]byte{0xFF, 0xFB, 0x90, 0}, 64)...)
}

type fakeRunner struct {
	decodes int32
	info    transcode.Info
	fail    bool
}

func (f *fakeRunner) Metadata(context.Context, string) (transcode.Info, error) {
	if f.fail {
		return transcode.Info{}, errors.New("cannot parse")
	}
	return f.info, nil
}

func (f *fakeRunner) Decode(_ context.Context, _, dst string) error {
	atomic.AddInt32(&f.decodes, 1)
	if f.fail {
		return errors.New("cannot decode")
	}
	return os.WriteFile(dst, []byte("RIFFfakewavdata"), 0o644)
}

type env struct {
	t      *testing.T
	srv    *Server
	h      http.Handler
	root   string
	data   string
	runner *fakeRunner
	logs   *errlog.Logger
}

func write(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func newEnv(t *testing.T, withTX, authOn bool) *env {
	t.Helper()
	return newEnvOpts(t, withTX, authOn, false)
}

func newEnvOpts(t *testing.T, withTX, authOn, readOnly bool) *env {
	t.Helper()
	root := filepath.Join(t.TempDir(), "music")
	write(t, filepath.Join(root, "a.mp3"), mp3With("Song A", png))
	write(t, filepath.Join(root, "b.mp3"), mp3With("Song B", nil))
	write(t, filepath.Join(root, "game.brstm"), []byte("RSTM\xFE\xFF"))
	write(t, filepath.Join(root, "notes.txt"), []byte("hello"))
	write(t, filepath.Join(root, "sub", "c.mp3"), mp3With("Song C", []byte("<svg onload=alert(1)>")))

	lib, err := library.New(root, "", readOnly)
	if err != nil {
		t.Fatal(err)
	}
	data := t.TempDir()
	st, err := store.Open(filepath.Join(data, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	salt := []byte("saltsaltsaltsalt")
	a := auth.New(salt, auth.Hash(salt, "pw"), !authOn, time.Hour)
	logs := errlog.New(filepath.Join(data, "error.log"), 0, nil)

	e := &env{t: t, root: root, data: data, logs: logs, runner: &fakeRunner{info: transcode.Info{
		SampleRate: 32000, Channels: 2, HasLoop: true, LoopStart: 100, LoopEnd: 900, Title: "Fight",
	}}}
	var tx *transcode.Service
	if withTX {
		tx, err = transcode.New(e.runner, filepath.Join(data, "cache"), 2, 0)
		if err != nil {
			t.Fatal(err)
		}
	}
	e.srv = New(Deps{
		Cfg:   config.Config{UploadSubdir: "uploads", MaxUploadMB: 1, ReadOnly: readOnly},
		Lib:   lib, Store: st, Auth: a, TX: tx, Log: logs, Up: upload.New(lib.Root(), "uploads", 1<<30, 0),
		Static: fstest.MapFS{"index.html": {Data: []byte("<h1>app</h1>")}, "sw.js": {Data: []byte("//sw")}},
	})
	e.srv.failDelay = 0
	e.h = e.srv.Handler()
	return e
}

func (e *env) do(method, url string, body any, cookie *http.Cookie, mutate ...func(*http.Request)) *httptest.ResponseRecorder {
	e.t.Helper()
	var rdr io.Reader
	if s, ok := body.(string); ok {
		rdr = strings.NewReader(s)
	} else if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			e.t.Fatal(err)
		}
		rdr = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, url, rdr)
	if method != http.MethodGet && method != http.MethodHead {
		req.Header.Set(csrfHeader, csrfValue)
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	for _, m := range mutate {
		m(req)
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec
}

func (e *env) login() *http.Cookie {
	e.t.Helper()
	rec := e.do("POST", "/api/login", map[string]string{"password": "pw"}, nil)
	if rec.Code != 200 {
		e.t.Fatalf("login: %d %s", rec.Code, rec.Body)
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == cookieName {
			return c
		}
	}
	e.t.Fatal("no session cookie")
	return nil
}

func decode(t *testing.T, rec *httptest.ResponseRecorder, v any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), v); err != nil {
		t.Fatalf("bad JSON %q: %v", rec.Body, err)
	}
}

func wantStatus(t *testing.T, rec *httptest.ResponseRecorder, code int) {
	t.Helper()
	if rec.Code != code {
		t.Fatalf("status %d, want %d; body: %s", rec.Code, code, rec.Body)
	}
}

// ---------------------------------------------------------------- tests

func TestHealthStaticAndSecurityHeaders(t *testing.T) {
	e := newEnv(t, false, true)
	rec := e.do("GET", "/healthz", nil, nil)
	wantStatus(t, rec, 200)
	if rec.Body.String() != "ok" {
		t.Error("healthz body")
	}
	rec = e.do("GET", "/", nil, nil)
	wantStatus(t, rec, 200)
	if !strings.Contains(rec.Body.String(), "<h1>app</h1>") || rec.Header().Get("Cache-Control") != "no-cache" {
		t.Errorf("static: %q %v", rec.Body, rec.Header())
	}
	h := rec.Header()
	if h.Get("X-Content-Type-Options") != "nosniff" || h.Get("X-Frame-Options") != "DENY" ||
		!strings.Contains(h.Get("Content-Security-Policy"), "default-src 'self'") || h.Get("Referrer-Policy") != "no-referrer" {
		t.Errorf("missing security headers: %v", h)
	}
	wantStatus(t, e.do("GET", "/nope.js", nil, nil), 404)
}

func TestAuthFlowAndCSRF(t *testing.T) {
	e := newEnv(t, false, true)
	wantStatus(t, e.do("GET", "/api/browse", nil, nil), 401)
	wantStatus(t, e.do("GET", "/api/stream?p=a.mp3", nil, nil), 401)

	var sess map[string]any
	rec := e.do("GET", "/api/session", nil, nil)
	decode(t, rec, &sess)
	if sess["authenticated"] != false || sess["authRequired"] != true || sess["password"] != true {
		t.Errorf("session: %v", sess)
	}
	for _, k := range []string{"canDelete", "canUpload", "vgmstream", "ffmpeg", "uploadDir", "maxUploadMB", "sevenZip"} {
		if _, leaked := sess[k]; leaked {
			t.Errorf("a visitor who is not signed in must not learn %s: %v", k, sess)
		}
	}
	sess = nil
	decode(t, e.do("GET", "/api/session", nil, e.login()), &sess)
	if sess["authenticated"] != true || sess["canDelete"] != true || sess["vgmstream"] != false {
		t.Errorf("signed-in session: %v", sess)
	}

	wantStatus(t, e.do("POST", "/api/login", map[string]string{"password": "nope"}, nil), 401)
	wantStatus(t, e.do("POST", "/api/login", "not json", nil), 400)
	// CSRF: state-changing requests without the custom header are refused before anything else.
	rec = e.do("POST", "/api/login", map[string]string{"password": "pw"}, nil, func(r *http.Request) { r.Header.Del(csrfHeader) })
	wantStatus(t, rec, 403)

	c := e.login()
	if !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.MaxAge <= 0 {
		t.Errorf("cookie flags: %+v", c)
	}
	wantStatus(t, e.do("GET", "/api/browse", nil, c), 200)
	rec = e.do("GET", "/api/session", nil, c)
	decode(t, rec, &sess)
	if sess["authenticated"] != true {
		t.Errorf("session after login: %v", sess)
	}
	wantStatus(t, e.do("POST", "/api/logout", nil, c, func(r *http.Request) { r.Header.Del(csrfHeader) }), 403)
	wantStatus(t, e.do("POST", "/api/logout", map[string]string{}, c), 200)
	wantStatus(t, e.do("GET", "/api/browse", nil, c), 401)
	// logout without a cookie still needs auth
	wantStatus(t, e.do("POST", "/api/logout", map[string]string{}, nil), 401)
}

func TestSecureCookieFlag(t *testing.T) {
	e := newEnv(t, false, true)
	e.srv.Cfg.CookieSecure = true
	if c := e.login(); !c.Secure {
		t.Error("cookie should be Secure when configured")
	}
}

func TestAuthDisabled(t *testing.T) {
	e := newEnv(t, false, false)
	wantStatus(t, e.do("GET", "/api/browse", nil, nil), 200)
	var sess map[string]any
	decode(t, e.do("GET", "/api/session", nil, nil), &sess)
	if sess["authRequired"] != false || sess["authenticated"] != true {
		t.Errorf("session: %v", sess)
	}
}

func TestLoginIsRateLimitedPerClient(t *testing.T) {
	e := newEnv(t, false, true)
	from := func(ip string) func(*http.Request) {
		return func(r *http.Request) { r.RemoteAddr = ip + ":1234" }
	}
	for i := 0; i < 10; i++ {
		wantStatus(t, e.do("POST", "/api/login", map[string]string{"password": "nope"}, nil, from("203.0.113.7")), 401)
	}
	rec := e.do("POST", "/api/login", map[string]string{"password": "pw"}, nil, from("203.0.113.7"))
	wantStatus(t, rec, 429)
	if !strings.Contains(rec.Body.String(), "too many") {
		t.Errorf("body: %s", rec.Body)
	}
	// Someone else is not locked out, and a success clears the count.
	wantStatus(t, e.do("POST", "/api/login", map[string]string{"password": "nope"}, nil, from("198.51.100.1")), 401)
	wantStatus(t, e.do("POST", "/api/login", map[string]string{"password": "pw"}, nil, from("198.51.100.1")), 200)
	if !e.srv.logins.Allowed("198.51.100.1") {
		t.Error("success clears the failures")
	}
}

func TestLoginWaitsForAHashingSlot(t *testing.T) {
	e := newEnv(t, false, true)
	for i := 0; i < cap(e.srv.hashing); i++ {
		e.srv.hashing <- struct{}{}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rec := e.do("POST", "/api/login", map[string]string{"password": "pw"}, nil, func(r *http.Request) { *r = *r.WithContext(ctx) })
	if len(rec.Result().Cookies()) != 0 {
		t.Error("a request given up while waiting must not sign in")
	}
	for i := 0; i < cap(e.srv.hashing); i++ {
		<-e.srv.hashing
	}
	wantStatus(t, e.do("POST", "/api/login", map[string]string{"password": "pw"}, nil), 200)
}

func TestNoLoginRefusesProxiedRequests(t *testing.T) {
	// Without a login, a tunnel on this machine would make the whole internet look local.
	e := newEnv(t, false, false)
	for _, h := range []string{"CF-Connecting-IP", "X-Forwarded-For", "Forwarded"} {
		for _, url := range []string{"/", "/api/browse", "/api/stream?path=a.mp3"} {
			rec := e.do("GET", url, nil, nil, func(r *http.Request) { r.Header.Set(h, "203.0.113.7") })
			wantStatus(t, rec, 403)
			if !strings.Contains(rec.Body.String(), "login is required") {
				t.Errorf("%s %s: %s", h, url, rec.Body.String())
			}
		}
	}
	// With a login the request goes on to the login check as usual.
	on := newEnv(t, false, true)
	wantStatus(t, on.do("GET", "/api/browse", nil, nil, func(r *http.Request) { r.Header.Set("CF-Connecting-IP", "203.0.113.7") }), 401)
	wantStatus(t, on.do("GET", "/api/browse", nil, on.login(), func(r *http.Request) { r.Header.Set("CF-Connecting-IP", "203.0.113.7") }), 200)
}

func TestBrowseAndTracks(t *testing.T) {
	e := newEnv(t, false, false)
	var br struct {
		Dir       string     `json:"dir"`
		Parent    string     `json:"parent"`
		HasParent bool       `json:"hasParent"`
		Entries   []entryDTO `json:"entries"`
	}
	decode(t, e.do("GET", "/api/browse", nil, nil), &br)
	var names []string
	for _, en := range br.Entries {
		names = append(names, en.Name)
	}
	if strings.Join(names, ",") != "sub,a.mp3,b.mp3,game.brstm,notes.txt" || br.HasParent {
		t.Fatalf("browse: %v %+v", names, br)
	}
	if !br.Entries[0].IsDir || !br.Entries[1].Playable || br.Entries[4].Playable {
		t.Errorf("flags: %+v", br.Entries)
	}

	decode(t, e.do("GET", "/api/browse?dir=sub", nil, nil), &br)
	if br.Dir != "sub" || !br.HasParent || br.Parent != "" || len(br.Entries) != 1 || br.Entries[0].Path != "sub/c.mp3" {
		t.Errorf("sub: %+v", br)
	}
	decode(t, e.do("GET", "/api/browse?dir=/sub/", nil, nil), &br)
	if br.Dir != "sub" {
		t.Errorf("dir should be normalised: %q", br.Dir)
	}

	var tr struct {
		Tracks []entryDTO `json:"tracks"`
	}
	decode(t, e.do("GET", "/api/tracks", nil, nil), &tr)
	var paths []string
	for _, x := range tr.Tracks {
		paths = append(paths, x.Path)
	}
	if strings.Join(paths, ",") != "sub/c.mp3,a.mp3,b.mp3,game.brstm" {
		t.Errorf("tracks: %v", paths)
	}

	wantStatus(t, e.do("GET", "/api/browse?dir=missing", nil, nil), 404)
	wantStatus(t, e.do("GET", "/api/browse?dir=.trash", nil, nil), 403)
	wantStatus(t, e.do("GET", "/api/tracks?dir=missing", nil, nil), 404)
	wantStatus(t, e.do("GET", "/api/browse?dir=a.mp3", nil, nil), 500) // not a directory is an internal error, not a leak
}

func TestFavorites(t *testing.T) {
	e := newEnv(t, false, false)
	wantStatus(t, e.do("POST", "/api/favorite", map[string]any{"path": "a.mp3", "on": true}, nil), 200)
	time.Sleep(5 * time.Millisecond) // favorites are ordered by timestamp; keep them apart on coarse clocks
	wantStatus(t, e.do("POST", "/api/favorite", map[string]any{"path": "sub/c.mp3", "on": true}, nil), 200)
	wantStatus(t, e.do("POST", "/api/favorite", map[string]any{"path": "notes.txt", "on": true}, nil), 415)
	wantStatus(t, e.do("POST", "/api/favorite", map[string]any{"path": "missing.mp3", "on": true}, nil), 404)
	wantStatus(t, e.do("POST", "/api/favorite", "{bad", nil), 400)

	var fav struct {
		Tracks []entryDTO `json:"tracks"`
	}
	decode(t, e.do("GET", "/api/favorites", nil, nil), &fav)
	if len(fav.Tracks) != 2 || fav.Tracks[0].Path != "sub/c.mp3" || !fav.Tracks[0].Fav {
		t.Fatalf("favorites newest first: %+v", fav.Tracks)
	}

	var br struct {
		Entries []entryDTO `json:"entries"`
	}
	decode(t, e.do("GET", "/api/browse", nil, nil), &br)
	for _, en := range br.Entries {
		if en.Name == "a.mp3" && !en.Fav {
			t.Error("browse should flag favorites")
		}
		if en.Name == "b.mp3" && en.Fav {
			t.Error("b.mp3 is not a favorite")
		}
	}

	// Removing works even when the file is gone.
	if err := os.Remove(filepath.Join(e.root, "a.mp3")); err != nil {
		t.Fatal(err)
	}
	decode(t, e.do("GET", "/api/favorites", nil, nil), &fav)
	if len(fav.Tracks) != 1 {
		t.Errorf("missing files are omitted from the list: %+v", fav.Tracks)
	}
	wantStatus(t, e.do("POST", "/api/favorite", map[string]any{"path": "a.mp3", "on": false}, nil), 200)
}

func TestTalk(t *testing.T) {
	e := newEnv(t, false, false)
	wantStatus(t, e.do("POST", "/api/talk", map[string]any{"path": "a.mp3", "on": true}, nil), 200)
	wantStatus(t, e.do("POST", "/api/talk", map[string]any{"path": "notes.txt", "on": true}, nil), 415)
	wantStatus(t, e.do("POST", "/api/talk", map[string]any{"path": "missing.mp3", "on": true}, nil), 404)
	wantStatus(t, e.do("POST", "/api/talk", "{bad", nil), 400)

	var talk struct {
		Tracks []entryDTO `json:"tracks"`
	}
	decode(t, e.do("GET", "/api/talk", nil, nil), &talk)
	if len(talk.Tracks) != 1 || talk.Tracks[0].Path != "a.mp3" || !talk.Tracks[0].Talk || talk.Tracks[0].Fav {
		t.Fatalf("talk list: %+v", talk.Tracks)
	}
	var fav struct {
		Tracks []entryDTO `json:"tracks"`
	}
	decode(t, e.do("GET", "/api/favorites", nil, nil), &fav)
	if len(fav.Tracks) != 0 {
		t.Fatalf("talk tracks are not favorites: %+v", fav.Tracks)
	}
	var m metaResp
	decode(t, e.do("GET", "/api/meta?p=a.mp3", nil, nil), &m)
	if !m.Talk || m.Fav {
		t.Fatalf("meta should flag talk: %+v", m)
	}
	wantStatus(t, e.do("POST", "/api/talk", map[string]any{"path": "a.mp3", "on": false}, nil), 200)
	decode(t, e.do("GET", "/api/talk", nil, nil), &talk)
	if len(talk.Tracks) != 0 {
		t.Fatalf("unmarked: %+v", talk.Tracks)
	}
}

func TestMeta(t *testing.T) {
	e := newEnv(t, true, false)
	var m metaResp
	decode(t, e.do("GET", "/api/meta?p=a.mp3", nil, nil), &m)
	if m.Title != "Song A" || m.Kind != "mp3" || !m.Native || !m.HasArt || m.Loop != nil || m.Path != "a.mp3" {
		t.Fatalf("mp3 meta: %+v", m)
	}
	decode(t, e.do("GET", "/api/meta?p=b.mp3", nil, nil), &m)
	if m.HasArt || m.Title != "Song B" {
		t.Fatalf("no-art meta: %+v", m)
	}
	decode(t, e.do("GET", "/api/meta?p=game.brstm", nil, nil), &m)
	if m.Kind != "vgm" || m.Native || m.Title != "Fight" || m.SampleRate != 32000 || m.Loop == nil || m.Loop.Start != 100 || m.Loop.End != 900 || m.Loop.SampleRate != 32000 {
		t.Fatalf("vgm meta: %+v", m)
	}
	wantStatus(t, e.do("GET", "/api/meta?p=notes.txt", nil, nil), 415)
	wantStatus(t, e.do("GET", "/api/meta?p=missing.mp3", nil, nil), 404)
	wantStatus(t, e.do("GET", "/api/meta?p=.trash/x", nil, nil), 403)
	wantStatus(t, e.do("GET", "/api/meta?p=sub", nil, nil), 400)

	e.runner.fail = true
	e.srv.TX, _ = transcode.New(e.runner, filepath.Join(t.TempDir(), "c"), 1, 0)
	wantStatus(t, e.do("GET", "/api/meta?p=game.brstm", nil, nil), 422)
}

func TestMetaVGMWithoutVgmstream(t *testing.T) {
	e := newEnv(t, false, false)
	rec := e.do("GET", "/api/meta?p=game.brstm", nil, nil)
	wantStatus(t, rec, 503)
	if !strings.Contains(rec.Body.String(), "vgmstream-cli") {
		t.Errorf("message should say what is missing: %s", rec.Body)
	}
	wantStatus(t, e.do("GET", "/api/stream?p=game.brstm", nil, nil), 503)
	wantStatus(t, e.do("GET", "/api/stream?p=a.mp3&transcode=1", nil, nil), 503)
}

func TestArt(t *testing.T) {
	e := newEnv(t, false, false)
	rec := e.do("GET", "/api/art?p=a.mp3", nil, nil)
	wantStatus(t, rec, 200)
	if rec.Header().Get("Content-Type") != "image/png" || !bytes.Equal(rec.Body.Bytes(), png) {
		t.Errorf("art: %v", rec.Header())
	}
	if !strings.Contains(rec.Header().Get("Content-Security-Policy"), "sandbox") {
		t.Error("art must be served sandboxed")
	}
	wantStatus(t, e.do("GET", "/api/art?p=b.mp3", nil, nil), 404)
	wantStatus(t, e.do("GET", "/api/art?p=sub/c.mp3", nil, nil), 404) // "image" that is really markup is never served
	wantStatus(t, e.do("GET", "/api/art?p=notes.txt", nil, nil), 415)
}

func TestStream(t *testing.T) {
	e := newEnv(t, true, false)
	want, _ := os.ReadFile(filepath.Join(e.root, "a.mp3"))

	rec := e.do("GET", "/api/stream?p=a.mp3", nil, nil)
	wantStatus(t, rec, 200)
	if !bytes.Equal(rec.Body.Bytes(), want) || rec.Header().Get("Content-Type") != "audio/mpeg" {
		t.Errorf("stream: %v", rec.Header())
	}
	rec = e.do("GET", "/api/stream?p=a.mp3", nil, nil, func(r *http.Request) { r.Header.Set("Range", "bytes=2-5") })
	wantStatus(t, rec, 206)
	if !bytes.Equal(rec.Body.Bytes(), want[2:6]) || !strings.HasPrefix(rec.Header().Get("Content-Range"), "bytes 2-5/") {
		t.Errorf("range: %q %v", rec.Body.Bytes(), rec.Header())
	}

	rec = e.do("GET", "/api/stream?p=game.brstm", nil, nil)
	wantStatus(t, rec, 200)
	if rec.Body.String() != "RIFFfakewavdata" || rec.Header().Get("Content-Type") != "audio/wav" {
		t.Errorf("transcoded: %q %v", rec.Body, rec.Header())
	}
	rec = e.do("GET", "/api/stream?p=game.brstm", nil, nil, func(r *http.Request) { r.Header.Set("Range", "bytes=0-3") })
	wantStatus(t, rec, 206)
	if rec.Body.String() != "RIFF" {
		t.Errorf("transcoded range: %q", rec.Body)
	}
	if n := atomic.LoadInt32(&e.runner.decodes); n != 1 {
		t.Errorf("transcode should be cached, decodes=%d", n)
	}
	wantStatus(t, e.do("GET", "/api/stream?p=a.mp3&transcode=1", nil, nil), 200)
	wantStatus(t, e.do("GET", "/api/stream?p=notes.txt", nil, nil), 415)
	wantStatus(t, e.do("GET", "/api/stream?p=missing.mp3", nil, nil), 404)

	e.runner.fail = true
	e.srv.TX, _ = transcode.New(e.runner, filepath.Join(t.TempDir(), "c"), 1, 0)
	wantStatus(t, e.do("GET", "/api/stream?p=game.brstm", nil, nil), 422)
}

func TestPathTraversalCannotEscape(t *testing.T) {
	e := newEnv(t, false, false)
	secret := filepath.Join(filepath.Dir(e.root), "secret.mp3")
	write(t, secret, mp3With("secret", nil))
	for _, p := range []string{"../secret.mp3", "..%2Fsecret.mp3", "sub/../../secret.mp3", "/../secret.mp3"} {
		rec := e.do("GET", "/api/stream?p="+p, nil, nil)
		if rec.Code == 200 {
			t.Errorf("%q escaped the library", p)
		}
	}
	wantStatus(t, e.do("GET", "/api/stream?p=.trash/x", nil, nil), 403)
}

func TestDeleteAndUndo(t *testing.T) {
	e := newEnv(t, false, false)
	wantStatus(t, e.do("POST", "/api/favorite", map[string]any{"path": "a.mp3", "on": true}, nil), 200)
	wantStatus(t, e.do("POST", "/api/talk", map[string]any{"path": "a.mp3", "on": true}, nil), 200)

	rec := e.do("DELETE", "/api/track?p=a.mp3", nil, nil)
	wantStatus(t, rec, 200)
	var del map[string]string
	decode(t, rec, &del)
	if len(del["token"]) != 16 || del["name"] != "a.mp3" || del["path"] != "a.mp3" {
		t.Fatalf("delete response: %v", del)
	}
	if _, err := os.Stat(filepath.Join(e.root, "a.mp3")); err == nil {
		t.Fatal("file should be gone")
	}
	if e.srv.Store.IsFavorite("a.mp3") || e.srv.Store.IsTalk("a.mp3") {
		t.Fatal("deleting must drop the favorite and the talk mark")
	}
	wantStatus(t, e.do("GET", "/api/meta?p=a.mp3", nil, nil), 404)

	rec = e.do("POST", "/api/undo", map[string]string{"token": del["token"]}, nil)
	wantStatus(t, rec, 200)
	var und struct {
		Path string `json:"path"`
		Fav  bool   `json:"fav"`
		Talk bool   `json:"talk"`
	}
	decode(t, rec, &und)
	if und.Path != "a.mp3" || !und.Fav || !und.Talk {
		t.Fatalf("undo response: %+v", und)
	}
	if !e.srv.Store.IsFavorite("a.mp3") || !e.srv.Store.IsTalk("a.mp3") {
		t.Error("undo must restore the favorite and the talk mark")
	}
	wantStatus(t, e.do("GET", "/api/meta?p=a.mp3", nil, nil), 200)
	wantStatus(t, e.do("POST", "/api/undo", map[string]string{"token": del["token"]}, nil), 400)
}

func TestDeleteErrorsAndUndoBookkeeping(t *testing.T) {
	e := newEnv(t, false, false)
	wantStatus(t, e.do("DELETE", "/api/track?p=missing.mp3", nil, nil), 404)
	wantStatus(t, e.do("DELETE", "/api/track?p=.trash/x", nil, nil), 403)
	wantStatus(t, e.do("POST", "/api/undo", map[string]string{"token": "../../x"}, nil), 400)
	wantStatus(t, e.do("POST", "/api/undo", "{", nil), 400)
	wantStatus(t, e.do("DELETE", "/api/track?p=b.mp3", nil, nil, func(r *http.Request) { r.Header.Del(csrfHeader) }), 403)

	// Old undo records are forgotten, but the file itself stays restorable.
	now := time.Now()
	e.srv.now = func() time.Time { return now }
	rec := e.do("DELETE", "/api/track?p=b.mp3", nil, nil)
	var del map[string]string
	decode(t, rec, &del)
	now = now.Add(2 * time.Hour)
	e.srv.rememberUndo("0000000000000000", undoRec{})
	e.srv.undoMu.Lock()
	_, kept := e.srv.undo[del["token"]]
	e.srv.undoMu.Unlock()
	if kept {
		t.Error("expired undo record should be pruned")
	}
	wantStatus(t, e.do("POST", "/api/undo", map[string]string{"token": del["token"]}, nil), 200)
}

func TestSettingsAndResume(t *testing.T) {
	e := newEnv(t, false, false)
	var s store.Settings
	decode(t, e.do("GET", "/api/settings", nil, nil), &s)
	if s != store.DefaultSettings() {
		t.Fatalf("defaults: %+v", s)
	}
	rec := e.do("PUT", "/api/settings", store.Settings{Shuffle: true, Repeat: "all", Volume: 3, LoopMode: "forever", LoopCount: 4, FadeSeconds: 5, VizMode: "scope"}, nil)
	wantStatus(t, rec, 200)
	decode(t, rec, &s)
	if !s.Shuffle || s.Repeat != "all" || s.Volume != 1 || s.LoopMode != "forever" || s.VizMode != "scope" {
		t.Fatalf("normalised: %+v", s)
	}
	wantStatus(t, e.do("PUT", "/api/settings", "[", nil), 400)

	var r store.Resume
	decode(t, e.do("GET", "/api/resume", nil, nil), &r)
	if r.Path != "" {
		t.Fatalf("empty resume: %+v", r)
	}
	wantStatus(t, e.do("PUT", "/api/resume", store.Resume{Path: "/sub//c.mp3", Position: 42.5, Source: "favorites"}, nil), 204)
	decode(t, e.do("GET", "/api/resume", nil, nil), &r)
	if r.Path != "sub/c.mp3" || r.Position != 42.5 || r.Source != "favorites" {
		t.Fatalf("resume: %+v", r)
	}
	wantStatus(t, e.do("PUT", "/api/resume", "nope", nil), 400)
}

func TestErrorLogEndpoint(t *testing.T) {
	e := newEnv(t, false, false)
	var out struct {
		Lines []string `json:"lines"`
	}
	decode(t, e.do("GET", "/api/errors", nil, nil), &out)
	if out.Lines == nil || len(out.Lines) != 0 {
		t.Fatalf("empty log should be an empty list: %#v", out.Lines)
	}
	wantStatus(t, e.do("GET", "/api/meta?p=notes.txt", nil, nil), 415) // logs an "unsupported" entry
	decode(t, e.do("GET", "/api/errors", nil, nil), &out)
	if len(out.Lines) != 1 || !strings.Contains(out.Lines[0], "code=1") || !strings.Contains(out.Lines[0], "site=meta") {
		t.Fatalf("log lines: %v", out.Lines)
	}
}

func TestPrefetchWarmsTranscodeCache(t *testing.T) {
	e := newEnv(t, true, false)
	wantStatus(t, e.do("POST", "/api/prefetch", map[string]string{"path": "game.brstm"}, nil), 202)
	wantStatus(t, e.do("POST", "/api/prefetch", map[string]string{"path": "a.mp3"}, nil), 202)    // native: nothing to do
	wantStatus(t, e.do("POST", "/api/prefetch", map[string]string{"path": "missing"}, nil), 202) // best effort
	wantStatus(t, e.do("POST", "/api/prefetch", "{", nil), 400)
	deadline := time.Now().Add(2 * time.Second)
	for atomic.LoadInt32(&e.runner.decodes) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if atomic.LoadInt32(&e.runner.decodes) != 1 {
		t.Fatal("prefetch should have started exactly one decode")
	}
	e.srv.TX = nil
	wantStatus(t, e.do("POST", "/api/prefetch", map[string]string{"path": "game.brstm"}, nil), 202)
}

func TestFailMapsInternalErrorsWithoutLeaking(t *testing.T) {
	e := newEnv(t, false, false)
	rec := httptest.NewRecorder()
	e.srv.fail(rec, errors.New("open /secret/path: boom"), "site", "p")
	if rec.Code != 500 || strings.Contains(rec.Body.String(), "secret") {
		t.Errorf("internal errors must be generic: %d %s", rec.Code, rec.Body)
	}
	lines, _ := e.logs.Recent(5)
	if len(lines) != 1 || !strings.Contains(lines[0], "/secret/path") {
		t.Errorf("details belong in the log: %v", lines)
	}
	for err, code := range map[error]int{
		library.ErrExists: 409, upload.ErrTooLarge: 413, upload.ErrBadName: 400, upload.ErrNoSpace: 507,
		upload.ErrUnsupported: 415, upload.ErrNoDest: 404, upload.ErrOverrun: 400, upload.ErrIncomplete: 400,
		library.ErrOutside: 403, library.ErrReadOnly: 403, library.ErrNotAudio: 415, fs.ErrPermission: 403,
	} {
		rec = httptest.NewRecorder()
		e.srv.fail(rec, err, "s", "p")
		if rec.Code != code {
			t.Errorf("%v: got %d want %d", err, rec.Code, code)
		}
	}
}

func TestRenameAndDeleteFolder(t *testing.T) {
	e := newEnv(t, false, false)
	write(t, filepath.Join(e.root, "dir", "s.mp3"), mp3With("s", nil))
	wantStatus(t, e.do("POST", "/api/favorite", map[string]any{"path": "dir/s.mp3", "on": true}, nil), 200)
	wantStatus(t, e.do("POST", "/api/talk", map[string]any{"path": "dir/s.mp3", "on": true}, nil), 200)

	rec := e.do("POST", "/api/rename", map[string]string{"path": "dir", "name": "renamed"}, nil)
	wantStatus(t, rec, 200)
	if !e.srv.Store.IsFavorite("renamed/s.mp3") || e.srv.Store.IsFavorite("dir/s.mp3") || !e.srv.Store.IsTalk("renamed/s.mp3") {
		t.Fatal("favorites and talk marks must follow a renamed folder")
	}
	wantStatus(t, e.do("POST", "/api/rename", map[string]string{"path": "renamed", "name": "../x"}, nil), 400)
	wantStatus(t, e.do("POST", "/api/rename", map[string]string{"path": "missing", "name": "x"}, nil), 404)
	wantStatus(t, e.do("POST", "/api/rename", "{", nil), 400)

	rec = e.do("DELETE", "/api/track?p=renamed", nil, nil)
	wantStatus(t, rec, 200)
	var del map[string]any
	decode(t, rec, &del)
	if del["isDir"] != true {
		t.Fatalf("delete response: %v", del)
	}
	if e.srv.Store.IsFavorite("renamed/s.mp3") || e.srv.Store.IsTalk("renamed/s.mp3") {
		t.Fatal("deleting a folder must drop the favorites and talk marks inside it")
	}
	wantStatus(t, e.do("POST", "/api/undo", map[string]string{"token": del["token"].(string)}, nil), 200)
	if !e.srv.Store.IsFavorite("renamed/s.mp3") || !e.srv.Store.IsTalk("renamed/s.mp3") {
		t.Error("undo must restore favorites and talk marks inside the folder")
	}
	wantStatus(t, e.do("GET", "/api/meta?p=renamed/s.mp3", nil, nil), 200)
}
