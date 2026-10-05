// Package api is the HTTP layer: JSON endpoints, range streaming, uploads
// and the embedded web app.
package api

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"masterplayer/internal/access"
	"masterplayer/internal/auth"
	"masterplayer/internal/config"
	"masterplayer/internal/errlog"
	"masterplayer/internal/google"
	"masterplayer/internal/library"
	"masterplayer/internal/meta"
	"masterplayer/internal/sniff"
	"masterplayer/internal/store"
	"masterplayer/internal/transcode"
	"masterplayer/internal/upload"
)

const (
	cookieName   = "mp_session"
	oauthCookie  = "mp_oauth"
	csrfHeader   = "X-Requested-With"
	csrfValue    = "masterplayer"
	sessionTTL   = 30 * 24 * time.Hour
	undoKeepTime = time.Hour
)

func init() {
	_ = mime.AddExtensionType(".webmanifest", "application/manifest+json")
}

// Deps are the collaborators of the server.
type Deps struct {
	Cfg    config.Config
	Lib    *library.Library
	Store  *store.Store
	Auth   *auth.Auth
	TX     *transcode.Service // nil when vgmstream-cli is not installed
	FF     *transcode.Service // nil when ffmpeg is not installed
	Up     *upload.Manager
	Log    *errlog.Logger
	Static fs.FS
	Access *access.Policy // who may connect at all; nil allows every address
	Google *google.Client // Sign in with Google; nil when it is not configured
}

type undoRec struct {
	wasFav bool
	favs   []string // favorites inside a deleted folder
	at     time.Time
}

// Server serves the API and the web app.
type Server struct {
	Deps
	failDelay  time.Duration
	now        func() time.Time
	newSession func() (string, bool) // a seam for tests: issues a session without a password

	undoMu sync.Mutex
	undo   map[string]undoRec
}

// New builds a Server.
func New(d Deps) *Server {
	return &Server{Deps: d, failDelay: 400 * time.Millisecond, now: time.Now, newSession: d.Auth.NewSession, undo: map[string]undoRec{}}
}

// Handler returns the full HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		io.WriteString(w, "ok")
	})
	mux.HandleFunc("GET /api/session", s.session)
	mux.HandleFunc("POST /api/login", s.login)
	mux.HandleFunc("POST /api/logout", s.protect(s.logout))
	if s.Google != nil {
		mux.HandleFunc("GET /auth/google/start", s.googleStart)
		mux.HandleFunc("GET /auth/google/callback", s.googleCallback)
	}
	mux.HandleFunc("GET /api/browse", s.protect(s.browse))
	mux.HandleFunc("GET /api/tracks", s.protect(s.tracks))
	mux.HandleFunc("GET /api/favorites", s.protect(s.favorites))
	mux.HandleFunc("POST /api/favorite", s.protect(s.setFavorite))
	mux.HandleFunc("GET /api/meta", s.protect(s.meta))
	mux.HandleFunc("POST /api/played", s.protect(s.played))
	mux.HandleFunc("GET /api/art", s.protect(s.art))
	mux.HandleFunc("GET /api/stream", s.protect(s.stream))
	mux.HandleFunc("POST /api/prefetch", s.protect(s.prefetch))
	mux.HandleFunc("DELETE /api/track", s.protect(s.deleteTrack))
	mux.HandleFunc("POST /api/rename", s.protect(s.renameFolder))
	mux.HandleFunc("POST /api/undo", s.protect(s.undoDelete))
	mux.HandleFunc("POST /api/upload/start", s.protect(s.uploadStart))
	mux.HandleFunc("POST /api/upload/begin", s.protect(s.uploadBegin))
	mux.HandleFunc("POST /api/upload/chunk", s.protect(s.uploadChunk))
	mux.HandleFunc("POST /api/upload/complete", s.protect(s.uploadComplete))
	mux.HandleFunc("GET /api/upload/status", s.protect(s.uploadStatus))
	mux.HandleFunc("GET /api/upload/report", s.protect(s.uploadReport))
	mux.HandleFunc("GET /api/settings", s.protect(s.getSettings))
	mux.HandleFunc("PUT /api/settings", s.protect(s.putSettings))
	mux.HandleFunc("GET /api/resume", s.protect(s.getResume))
	mux.HandleFunc("PUT /api/resume", s.protect(s.putResume))
	mux.HandleFunc("GET /api/errors", s.protect(s.errorLines))
	mux.Handle("/", s.static())
	h := s.secure(mux)
	if s.Access != nil {
		h = s.Access.Middleware(h) // outermost: a refused address never reaches anything, not even the login or static files
	}
	return h
}

// secure adds security headers and CSRF protection: every state-changing
// request must carry a custom header, which cross-site forms cannot set.
func (s *Server) secure(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data: blob:; media-src 'self' blob:; style-src 'self' 'unsafe-inline'; script-src 'self'; connect-src 'self'; worker-src 'self'; manifest-src 'self'; frame-ancestors 'none'")
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
		default:
			if r.Header.Get(csrfHeader) != csrfValue {
				writeErr(w, http.StatusForbidden, "missing request header")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) protect(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.authed(r) {
			writeErr(w, http.StatusUnauthorized, "login required")
			return
		}
		h(w, r)
	}
}

func (s *Server) authed(r *http.Request) bool {
	if s.Auth.Disabled() {
		return true
	}
	c, err := r.Cookie(cookieName)
	return err == nil && s.Auth.Valid(c.Value)
}

func (s *Server) static() http.Handler {
	fsrv := http.FileServerFS(s.Static)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		fsrv.ServeHTTP(w, r)
	})
}

// ---------------------------------------------------------------- helpers

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return false
	}
	return true
}

// fail maps an error to a status code; internal details stay in the log.
func (s *Server) fail(w http.ResponseWriter, err error, site, p string) {
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, fs.ErrNotExist):
		status = http.StatusNotFound
	case errors.Is(err, library.ErrReadOnly), errors.Is(err, library.ErrHidden), errors.Is(err, library.ErrOutside), errors.Is(err, fs.ErrPermission):
		status = http.StatusForbidden
	case errors.Is(err, library.ErrNotAudio):
		status = http.StatusUnsupportedMediaType
		s.Log.Append(errlog.CodeUnsupported, site, p, "")
	case errors.Is(err, library.ErrNotFile), errors.Is(err, library.ErrBadToken), errors.Is(err, library.ErrBadName), errors.Is(err, library.ErrNotDir):
		status = http.StatusBadRequest
	case errors.Is(err, library.ErrExists):
		status = http.StatusConflict
	case errors.Is(err, upload.ErrTooLarge):
		status = http.StatusRequestEntityTooLarge
	case errors.Is(err, upload.ErrNoSpace):
		status = http.StatusInsufficientStorage
	case errors.Is(err, upload.ErrUnsupported):
		status = http.StatusUnsupportedMediaType
	case errors.Is(err, upload.ErrNoDest), errors.Is(err, upload.ErrNoReport):
		status = http.StatusNotFound
	case errors.Is(err, upload.ErrBadPath), errors.Is(err, upload.ErrBadName), errors.Is(err, upload.ErrBadSize),
		errors.Is(err, upload.ErrNoSession), errors.Is(err, upload.ErrOverrun), errors.Is(err, upload.ErrIncomplete):
		status = http.StatusBadRequest
	}
	msg := err.Error()
	if status >= 500 {
		s.Log.Append(errlog.CodeIO, site, p, err.Error())
		msg = "internal error"
	}
	writeErr(w, status, msg)
}

type entryDTO struct {
	library.Entry
	Fav bool `json:"fav"`
}

func (s *Server) dtos(es []library.Entry) []entryDTO {
	favs := map[string]bool{}
	for _, f := range s.Store.Favorites() {
		favs[f.Path] = true
	}
	out := make([]entryDTO, 0, len(es))
	for _, e := range es {
		out = append(out, entryDTO{Entry: e, Fav: favs[e.Path]})
	}
	return out
}

// ---------------------------------------------------------------- session

func (s *Server) session(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"authenticated": s.authed(r),
		"authRequired":  !s.Auth.Disabled(),
		"google":        s.Google != nil,
		"password":      s.Google == nil || strings.TrimSpace(s.Cfg.AdminPassword) != "", // false: Google is the only way in
		"canDelete":     !s.Lib.ReadOnly(),
		"canUpload":     s.canUpload(),
		"vgmstream":     s.TX != nil,
		"ffmpeg":        s.FF != nil,
		"uploadDir":     s.Cfg.UploadSubdir,
		"maxUploadMB":   s.Cfg.MaxUploadMB,
		"sevenZip":      s.Up.SevenZipAvailable(),
	})
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Password string `json:"password"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	token, ok := s.Auth.Login(body.Password)
	if !ok {
		time.Sleep(s.failDelay) // slows down guessing
		writeErr(w, http.StatusUnauthorized, "wrong password")
		return
	}
	s.setSession(w, token)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) setSession(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: token, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteStrictMode, Secure: s.Cfg.CookieSecure,
		MaxAge: int(sessionTTL.Seconds()),
	})
}

// googleStart sends the browser to Google. The state is also kept in a cookie so the callback
// can tell that the browser finishing the sign-in is the one that started it.
func (s *Server) googleStart(w http.ResponseWriter, r *http.Request) {
	state, to, err := s.Google.Start()
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, google.ErrBusy) {
			status = http.StatusTooManyRequests
		}
		http.Error(w, err.Error(), status)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: oauthCookie, Value: state, Path: "/auth/google/", HttpOnly: true,
		SameSite: http.SameSiteLaxMode, Secure: s.Cfg.CookieSecure, MaxAge: 600, // Lax: Google sends the browser back
	})
	http.Redirect(w, r, to, http.StatusFound)
}

func (s *Server) googleCallback(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: oauthCookie, Value: "", Path: "/auth/google/", MaxAge: -1, HttpOnly: true})
	q := r.URL.Query()
	state := q.Get("state")
	c, err := r.Cookie(oauthCookie)
	if err != nil || state == "" || subtle.ConstantTimeCompare([]byte(c.Value), []byte(state)) != 1 {
		http.Error(w, google.ErrState.Error(), http.StatusBadRequest)
		return
	}
	if q.Get("error") != "" { // the user said no at Google, or Google refused
		http.Error(w, "sign-in was cancelled", http.StatusUnauthorized)
		return
	}
	email, err := s.Google.Finish(r.Context(), state, q.Get("code"))
	switch {
	case errors.Is(err, google.ErrDenied):
		s.Log.Append(errlog.CodeIO, "google", email, "sign-in refused: not on the allowlist")
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	case errors.Is(err, google.ErrState):
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	case err != nil:
		s.Log.Append(errlog.CodeIO, "google", "", err.Error())
		http.Error(w, "sign-in failed", http.StatusBadGateway)
		return
	}
	token, ok := s.newSession()
	if !ok {
		s.Log.Append(errlog.CodeIO, "google", email, "could not create a session")
		http.Error(w, "sign-in failed", http.StatusInternalServerError)
		return
	}
	s.setSession(w, token)
	http.Redirect(w, r, "/", http.StatusFound)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(cookieName); err == nil {
		s.Auth.Logout(c.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ---------------------------------------------------------------- library

func (s *Server) browse(w http.ResponseWriter, r *http.Request) {
	dir := r.URL.Query().Get("dir")
	es, err := s.Lib.Browse(dir)
	if err != nil {
		s.fail(w, err, "browse", dir)
		return
	}
	clean := library.CleanRel(dir)
	parent := ""
	if clean != "" {
		if parent = path.Dir(clean); parent == "." {
			parent = ""
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"dir": clean, "parent": parent, "hasParent": clean != "", "entries": s.dtos(es),
	})
}

func (s *Server) tracks(w http.ResponseWriter, r *http.Request) {
	dir := r.URL.Query().Get("dir")
	es, err := s.Lib.Tracks(dir)
	if err != nil {
		s.fail(w, err, "tracks", dir)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tracks": s.dtos(es)})
}

func (s *Server) favorites(w http.ResponseWriter, r *http.Request) {
	var es []library.Entry
	for _, f := range s.Store.Favorites() {
		if e, err := s.Lib.Describe(f.Path); err == nil && e.Playable {
			es = append(es, e)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"tracks": s.dtos(es)})
}

func (s *Server) setFavorite(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Path string `json:"path"`
		On   bool   `json:"on"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	p := library.CleanRel(body.Path)
	if body.On {
		e, err := s.Lib.Describe(p)
		if err != nil {
			s.fail(w, err, "favorite", p)
			return
		}
		if !e.Playable {
			s.fail(w, library.ErrNotAudio, "favorite", p)
			return
		}
	}
	if err := s.Store.SetFavorite(p, body.On); err != nil {
		s.fail(w, err, "favorite", p)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"path": p, "on": body.On})
}

type metaResp struct {
	Path       string     `json:"path"`
	Kind       string     `json:"kind"`
	Native     bool       `json:"native"`
	Title      string     `json:"title"`
	Artist     string     `json:"artist"`
	Album      string     `json:"album"`
	Genre      string     `json:"genre"`
	Year       string     `json:"year"`
	Track      string     `json:"track"`
	HasArt     bool       `json:"hasArt"`
	SampleRate int        `json:"sampleRate"`
	Loop       *meta.Loop `json:"loop"`
	Fav        bool       `json:"fav"`
	Plays      int        `json:"plays"`
}

// played counts one finished listen of a track.
func (s *Server) played(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Path string `json:"path"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	p := library.CleanRel(body.Path)
	if _, _, err := s.Lib.Kind(p); err != nil {
		s.fail(w, err, "played", p)
		return
	}
	n, err := s.Store.RecordPlay(p)
	if err != nil {
		s.fail(w, err, "played", p)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"path": p, "plays": n})
}

func (s *Server) meta(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Query().Get("p")
	kind, abs, err := s.Lib.Kind(p)
	if err != nil {
		s.fail(w, err, "meta", p)
		return
	}
	clean := library.CleanRel(p)
	tags, err := meta.Read(abs, kind)
	if err != nil {
		s.Log.Append(errlog.CodeMeta, "meta", clean, err.Error())
	}
	resp := metaResp{
		Path: clean, Kind: kind.String(), Native: kind.Native(),
		Title: tags.Title, Artist: tags.Artist, Album: tags.Album, Genre: tags.Genre,
		Year: tags.Year, Track: tags.Track, HasArt: len(tags.Art) > 0,
		SampleRate: tags.SampleRate, Loop: tags.Loop, Fav: s.Store.IsFavorite(clean),
		Plays: s.Store.Plays(clean),
	}
	if !resp.HasArt {
		_, resp.HasArt = s.Lib.FolderArt(clean)
	}
	if kind == sniff.VGM || kind == sniff.FFmpeg {
		svc, missing := s.renderer(kind, false)
		if svc == nil {
			s.Log.Append(errlog.CodeDecode, "meta", clean, missing)
			writeErr(w, http.StatusServiceUnavailable, missing)
			return
		}
		info, err := svc.Info(r.Context(), abs)
		if err != nil {
			if r.Context().Err() != nil {
				return
			}
			s.Log.Append(errlog.CodeDecode, "meta", clean, err.Error())
			writeErr(w, http.StatusUnprocessableEntity, "this file could not be decoded")
			return
		}
		resp.SampleRate = info.SampleRate
		for dst, src := range map[*string]string{
			&resp.Title: info.Title, &resp.Artist: info.Artist, &resp.Album: info.Album,
			&resp.Genre: info.Genre, &resp.Year: info.Year, &resp.Track: info.Track,
		} {
			if src != "" {
				*dst = src
			}
		}
		if info.HasLoop && kind == sniff.VGM {
			resp.Loop = &meta.Loop{Start: info.LoopStart, End: info.LoopEnd, SampleRate: info.SampleRate}
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

var artTypes = map[string]bool{"image/jpeg": true, "image/png": true, "image/gif": true, "image/webp": true}

func (s *Server) art(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Query().Get("p")
	kind, abs, err := s.Lib.Kind(p)
	if err != nil {
		s.fail(w, err, "art", p)
		return
	}
	tags, _ := meta.Read(abs, kind)
	// Trust the bytes, not the declared type, and never serve SVG or HTML.
	ct := http.DetectContentType(tags.Art)
	if len(tags.Art) > 0 && artTypes[ct] {
		artHeaders(w, ct)
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(tags.Art))
		return
	}
	// No embedded picture: use one from the album folder (cover.jpg, folder.png, Scans/...).
	if pic, ok := s.Lib.FolderArt(library.CleanRel(p)); ok {
		if data, err := os.ReadFile(pic); err == nil {
			if ct := http.DetectContentType(data); artTypes[ct] {
				artHeaders(w, ct)
				http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(data))
				return
			}
		}
	}
	writeErr(w, http.StatusNotFound, "no cover art")
}

func artHeaders(w http.ResponseWriter, ct string) {
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	w.Header().Set("Cache-Control", "private, max-age=3600")
}

// renderer picks the converter for a file: ffmpeg formats go to ffmpeg, game audio to
// vgmstream. A browser-incompatible native file (transcode=1) prefers ffmpeg. When the needed
// tool is missing it returns nil and the message to show.
func (s *Server) renderer(kind sniff.Kind, preferFF bool) (*transcode.Service, string) {
	const noFF, noVGM = "ffmpeg is not installed on the server", "vgmstream-cli is not installed on the server"
	switch {
	case kind == sniff.FFmpeg || (preferFF && kind != sniff.VGM && s.FF != nil):
		if s.FF == nil {
			return nil, noFF
		}
		return s.FF, ""
	case s.TX == nil && kind != sniff.VGM:
		return nil, noFF
	case s.TX == nil:
		return nil, noVGM
	}
	return s.TX, ""
}

func serveFile(w http.ResponseWriter, r *http.Request, p, contentType string) {
	f, err := os.Open(p)
	if err != nil {
		writeErr(w, http.StatusNotFound, "file not found")
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.IsDir() {
		writeErr(w, http.StatusNotFound, "file not found")
		return
	}
	w.Header().Set("Content-Type", contentType)
	http.ServeContent(w, r, "", st.ModTime(), f) // handles Range requests
}

func (s *Server) stream(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Query().Get("p")
	kind, abs, err := s.Lib.Kind(p)
	if err != nil {
		s.fail(w, err, "stream", p)
		return
	}
	convert := kind == sniff.VGM || kind == sniff.FFmpeg
	if !convert && r.URL.Query().Get("transcode") != "1" {
		serveFile(w, r, abs, kind.MIME())
		return
	}
	svc, missing := s.renderer(kind, true)
	if svc == nil {
		writeErr(w, http.StatusServiceUnavailable, missing)
		return
	}
	out, err := svc.Render(r.Context(), abs)
	if err != nil {
		if r.Context().Err() != nil {
			return
		}
		s.Log.Append(errlog.CodeTranscode, "stream", library.CleanRel(p), err.Error())
		writeErr(w, http.StatusUnprocessableEntity, "this file could not be decoded")
		return
	}
	serveFile(w, r, out, svc.MIME())
}

func (s *Server) prefetch(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Path string `json:"path"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	if kind, abs, err := s.Lib.Kind(body.Path); err == nil && (kind == sniff.VGM || kind == sniff.FFmpeg) {
		if svc, _ := s.renderer(kind, false); svc != nil {
			svc.Prefetch(abs)
		}
	}
	w.WriteHeader(http.StatusAccepted)
}

// ---------------------------------------------------------------- delete / undo

func (s *Server) deleteTrack(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Query().Get("p")
	clean := library.CleanRel(p)
	wasFav := s.Store.IsFavorite(clean)
	favs := s.Store.FavoritesUnder(clean)
	tr, err := s.Lib.Delete(p)
	if err != nil {
		s.fail(w, err, "delete", clean)
		return
	}
	if err := s.Store.SetFavorite(clean, false); err != nil {
		s.Log.Append(errlog.CodeIO, "delete", clean, err.Error())
	}
	if tr.IsDir {
		if err := s.Store.MoveFavorites(clean, ""); err != nil {
			s.Log.Append(errlog.CodeIO, "delete", clean, err.Error())
		}
	}
	s.rememberUndo(tr.Token, wasFav, favs)
	out := map[string]any{"token": tr.Token, "name": tr.Name, "path": tr.Path}
	if tr.IsDir {
		out["isDir"] = true
	}
	writeJSON(w, http.StatusOK, out)
}

// renameFolder renames a folder in place; favorites inside it follow.
func (s *Server) renameFolder(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Path string `json:"path"`
		Name string `json:"name"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	clean := library.CleanRel(body.Path)
	dest, err := s.Lib.Rename(clean, body.Name)
	if err != nil {
		s.fail(w, err, "rename", clean)
		return
	}
	if err := s.Store.MoveFavorites(clean, dest); err != nil {
		s.Log.Append(errlog.CodeIO, "rename", clean, err.Error())
	}
	if err := s.Store.MovePlays(clean, dest); err != nil {
		s.Log.Append(errlog.CodeIO, "rename", clean, err.Error())
	}
	writeJSON(w, http.StatusOK, map[string]string{"path": dest, "name": body.Name})
}

func (s *Server) rememberUndo(token string, wasFav bool, favs []string) {
	s.undoMu.Lock()
	defer s.undoMu.Unlock()
	now := s.now()
	for t, rec := range s.undo {
		if now.Sub(rec.at) > undoKeepTime {
			delete(s.undo, t)
		}
	}
	s.undo[token] = undoRec{wasFav: wasFav, favs: favs, at: now}
}

func (s *Server) undoDelete(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Token string `json:"token"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	p, err := s.Lib.Undo(body.Token)
	if err != nil {
		s.fail(w, err, "undo", body.Token)
		return
	}
	s.undoMu.Lock()
	rec := s.undo[body.Token]
	delete(s.undo, body.Token)
	s.undoMu.Unlock()
	if rec.wasFav {
		if err := s.Store.SetFavorite(p, true); err != nil {
			s.Log.Append(errlog.CodeIO, "undo", p, err.Error())
		}
	}
	for _, f := range rec.favs {
		if err := s.Store.SetFavorite(f, true); err != nil {
			s.Log.Append(errlog.CodeIO, "undo", f, err.Error())
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"path": p, "fav": rec.wasFav})
}

// ---------------------------------------------------------------- settings / resume / log

func (s *Server) getSettings(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.Store.Settings())
}

func (s *Server) putSettings(w http.ResponseWriter, r *http.Request) {
	var v store.Settings
	if !readJSON(w, r, &v) {
		return
	}
	out, err := s.Store.SetSettings(v)
	if err != nil {
		s.fail(w, err, "settings", "")
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) getResume(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.Store.Resume())
}

func (s *Server) putResume(w http.ResponseWriter, r *http.Request) {
	var v store.Resume
	if !readJSON(w, r, &v) {
		return
	}
	v.Path = library.CleanRel(v.Path)
	if err := s.Store.SetResume(v); err != nil {
		s.fail(w, err, "resume", v.Path)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) errorLines(w http.ResponseWriter, r *http.Request) {
	lines, err := s.Log.Recent(100)
	if err != nil {
		s.fail(w, err, "errors", "")
		return
	}
	if lines == nil {
		lines = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"lines": lines})
}
