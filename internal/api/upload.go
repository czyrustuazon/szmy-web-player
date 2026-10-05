package api

import (
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"strconv"

	"masterplayer/internal/errlog"
	"masterplayer/internal/library"
	"masterplayer/internal/upload"
)

// The upload endpoints implement the chunked, resumable protocol described in
// package upload (the same one anime-db-stream uses):
//
//	POST /api/upload/start    {title, merge?}                  -> {name, relPath}
//	POST /api/upload/begin    {relPath, filename, size}        -> {offset}
//	POST /api/upload/chunk    ?relPath=&filename=&offset=      -> {offset}
//	                          header X-Chunk-CRC32, raw body
//	POST /api/upload/complete {relPath, filename, size}        -> status
//	GET  /api/upload/status   ?relPath=&filename=              -> status
//	GET  /api/upload/report   ?relPath=&filename=              -> text: files left out of an archive
//
// complete and status always answer 200 with a "state" of running, done,
// failed or corrupted; a chunk at the wrong offset answers 409 with the
// server's real offset.

// canUpload is what the UI is told: uploads need the upload folder to be writable. That is
// checked on the folder itself, because it can be a separate mount (UPLOADS_HOST_PATH) that is
// writable even when the rest of the library is not.
func (s *Server) canUpload() bool {
	return !s.Cfg.ReadOnly && s.Up.Writable()
}

// uploadsEnabled refuses uploads when read-only mode is forced. An unwritable folder simply makes
// the operation fail (the UI does not offer uploads then); a permission error is reported as 403.
func (s *Server) uploadsEnabled(w http.ResponseWriter) bool {
	if s.Cfg.ReadOnly {
		writeErr(w, http.StatusForbidden, library.ErrReadOnly.Error())
		return false
	}
	return true
}

func (s *Server) uploadStart(w http.ResponseWriter, r *http.Request) {
	if !s.uploadsEnabled(w) {
		return
	}
	var body struct {
		Title string `json:"title"`
		Merge bool   `json:"merge"` // add to the folder of that name if it exists
	}
	if !readJSON(w, r, &body) {
		return
	}
	start := s.Up.Start
	if body.Merge {
		start = s.Up.StartOrJoin
	}
	name, relPath, err := start(body.Title)
	if err != nil {
		s.fail(w, err, "upload-start", body.Title)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"name": name, "relPath": relPath})
}

type uploadFileRequest struct {
	RelPath  string `json:"relPath"`
	Filename string `json:"filename"`
	Size     int64  `json:"size"`
}

func (s *Server) uploadBegin(w http.ResponseWriter, r *http.Request) {
	if !s.uploadsEnabled(w) {
		return
	}
	var req uploadFileRequest
	if !readJSON(w, r, &req) {
		return
	}
	offset, err := s.Up.Begin(req.RelPath, req.Filename, req.Size)
	if err != nil {
		s.fail(w, err, "upload-begin", req.Filename)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int64{"offset": offset})
}

func (s *Server) uploadChunk(w http.ResponseWriter, r *http.Request) {
	if !s.uploadsEnabled(w) {
		return
	}
	q := r.URL.Query()
	relPath, filename := q.Get("relPath"), q.Get("filename")
	offset, err := strconv.ParseInt(q.Get("offset"), 10, 64)
	if err != nil || offset < 0 {
		writeErr(w, http.StatusBadRequest, "expected ?offset= (a non-negative byte count)")
		return
	}
	crc := r.Header.Get("X-Chunk-CRC32")
	if raw, err := hex.DecodeString(crc); err != nil || len(raw) != 4 {
		writeErr(w, http.StatusBadRequest, `expected header "X-Chunk-CRC32" (8 hex characters)`)
		return
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, upload.MaxChunkBytes))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "could not read the chunk body")
		return
	}

	newOffset, err := s.Up.Append(relPath, filename, offset, crc, data)
	var mismatch *upload.OffsetMismatchError
	if errors.As(err, &mismatch) {
		writeJSON(w, http.StatusConflict, map[string]int64{"offset": mismatch.Current})
		return
	}
	var bad *upload.ChecksumMismatchError
	if errors.As(err, &bad) {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err != nil {
		s.fail(w, err, "upload-chunk", filename)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int64{"offset": newOffset})
}

func (s *Server) uploadComplete(w http.ResponseWriter, r *http.Request) {
	if !s.uploadsEnabled(w) {
		return
	}
	var req uploadFileRequest
	if !readJSON(w, r, &req) {
		return
	}
	st, err := s.Up.Complete(req.RelPath, req.Filename, req.Size)
	if err != nil {
		s.fail(w, err, "upload-complete", req.Filename)
		return
	}
	if st.State == upload.Failed {
		s.Log.Append(errlog.CodeUpload, "upload-complete", req.Filename, st.Error)
	}
	writeJSON(w, http.StatusOK, st)
}

// uploadReport serves the list of files an archive upload left out, as plain text.
func (s *Server) uploadReport(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	p, err := s.Up.Report(q.Get("relPath"), q.Get("filename"))
	if err != nil {
		s.fail(w, err, "upload-report", q.Get("filename"))
		return
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	serveFile(w, r, p, "text/plain; charset=utf-8")
}

func (s *Server) uploadStatus(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	st, found := s.Up.StatusOf(q.Get("relPath"), q.Get("filename"))
	if !found {
		writeErr(w, http.StatusNotFound, "nothing is tracked for this file; call complete first")
		return
	}
	if st.State == upload.Failed {
		s.Log.Append(errlog.CodeUpload, "upload-status", q.Get("filename"), st.Error)
	}
	writeJSON(w, http.StatusOK, st)
}
