// Package upload implements chunked, resumable, checksummed uploads of large
// files and archives. It is a port of the protocol used by anime-db-stream
// (internal/library/upload.go there), adapted to a music library:
//
//   - A file is sent as a series of short requests (chunks), each carrying a
//     CRC32 the server verifies before writing.
//   - The server's own disk state is the only source of truth for progress: a
//     chunk is accepted only if its offset equals the staged file's size, so
//     any failure (dropped connection, reload, two tabs) is recovered by asking
//     for the real offset and resuming from there.
//   - A session is identified by sha256(folder, filename), so there are no
//     session tokens to hand out, store or expire.
//   - Finishing an archive is asynchronous: verify and extract run in the
//     background and the client polls for progress, so no request is ever held
//     open for the length of an extraction.
//   - A corrupt archive is localised to the bad chunk (only that chunk is
//     re-sent) or, if every chunk still matches, reported as a bad source file.
//
// Differences from the original: uploads may only land inside the configured
// upload folder; archives are extracted into a hidden scratch folder, reduced
// to audio files only (so symlinks, executables and other junk never reach the
// library) and then moved into place.
package upload

import (
	"bytes"
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"masterplayer/internal/library"
	"masterplayer/internal/sniff"
)

// StagingDirName holds the bytes of in-progress uploads. It lives inside the upload
// folder, so it is always on the same disk as the finished files (even when the upload
// folder is a separate mount): a big upload never fills another disk first and is then
// copied, and the final step is an instant rename. It is hidden, so the library never
// lists it.
const StagingDirName = ".uploads"

// MaxChunkBytes bounds a single chunk regardless of what the client sends.
var MaxChunkBytes int64 = 64 << 20

// Errors the HTTP layer maps to status codes.
var (
	ErrBadPath     = errors.New("upload folder is not allowed")
	ErrNoDest      = errors.New("upload folder not found; start a new upload")
	ErrBadName     = errors.New("invalid file name")
	ErrBadSize     = errors.New("invalid file size")
	ErrTooLarge    = errors.New("upload too large")
	ErrNoSpace     = errors.New("not enough free space")
	ErrNoSession   = errors.New("no upload session; call begin first")
	ErrOverrun     = errors.New("chunk goes past the declared file size")
	ErrIncomplete  = errors.New("upload incomplete; keep sending chunks from the current offset")
	ErrUnsupported = errors.New("7z archives are not supported on this server")
	ErrNoReport    = errors.New("there is no list of skipped files for this upload")
)

// OffsetMismatchError means the caller's offset disagrees with the bytes on
// disk. Current is the authoritative offset to resume from.
type OffsetMismatchError struct{ Current int64 }

func (e *OffsetMismatchError) Error() string {
	return fmt.Sprintf("offset mismatch: server already has %d bytes", e.Current)
}

// ChecksumMismatchError means a chunk was corrupted in transit. Nothing was written.
type ChecksumMismatchError struct{}

func (e *ChecksumMismatchError) Error() string { return "chunk does not match its CRC32" }

// CorruptUploadError means integrity checking localised damage to one chunk;
// the session was rewound to ResumeOffset so only that part is re-sent.
type CorruptUploadError struct{ ResumeOffset int64 }

func (e *CorruptUploadError) Error() string {
	return fmt.Sprintf("upload corrupted after byte %d; resume from there", e.ResumeOffset)
}

// State is where one file's completion stands.
type State string

const (
	Running   State = "running"
	Done      State = "done"
	Failed    State = "failed"
	Corrupted State = "corrupted"
)

// Status is the shared result of Complete and StatusOf. BytesWritten is live
// extraction progress while Running.
type Status struct {
	State        State  `json:"state"`
	BytesWritten int64  `json:"bytesWritten"`
	TotalBytes   int64  `json:"totalBytes"`
	Error        string `json:"error,omitempty"`
	ResumeOffset int64  `json:"resumeOffset,omitempty"`
	Tracks       int    `json:"tracks,omitempty"`  // audio files added
	Images       int    `json:"images,omitempty"`  // cover images kept (they become cover art)
	Duplicates   int    `json:"duplicates,omitempty"` // files that were already in the folder, identical, and so not added again
	Skipped      int    `json:"skipped,omitempty"` // other files dropped from an archive
	Path         string `json:"path,omitempty"`    // library path of the file or folder that was added

	// What was dropped, by file type ("jpg": 927, "txt": 66, ...), and whether the full list
	// of dropped files can be fetched with Report.
	SkippedTypes map[string]int `json:"skippedTypes,omitempty"`
	HasReport    bool           `json:"hasReport,omitempty"`
}

// ExecFunc runs an external command (7z) and returns combined output.
type ExecFunc func(name string, args ...string) ([]byte, error)

func defaultExec(name string, args ...string) ([]byte, error) {
	return exec.Command(name, args...).CombinedOutput()
}

// Manager owns the upload sessions of one library.
type Manager struct {
	root      string // real path of the library root
	uploadRel string // slash-separated folder inside root that uploads may use
	staging   string
	maxBytes  int64
	minFree   int64

	// Seams for tests.
	exec      ExecFunc
	lookPath  func(string) (string, error)
	freeBytes func(string) (uint64, error)
	move      func(src, dst string) error
	rename    func(src, dst string) error
	logf      func(format string, args ...any)

	mu          sync.Mutex
	completions map[string]*Status
	extractMu   sync.Mutex // one extraction at a time: they are disk-bound
}

// New creates a Manager. root must be the library's real root path; uploadRel
// is the upload folder inside it (for example "uploads"); maxBytes caps a
// file's declared size (and an archive's extracted size); minFree is the free
// space that must remain after an upload (0 disables the check).
func New(root, uploadRel string, maxBytes, minFree int64) *Manager {
	return &Manager{
		root: root, uploadRel: uploadRel, staging: filepath.Join(root, filepath.FromSlash(uploadRel), StagingDirName),
		maxBytes: maxBytes, minFree: minFree,
		exec: defaultExec, lookPath: exec.LookPath, freeBytes: platformFreeBytes,
		move: library.MoveFile, rename: os.Rename, logf: log.Printf,
		completions: map[string]*Status{},
	}
}

// Writable reports whether the upload folder can be written to, creating it if needed.
// It is checked on the upload folder itself, not the library root: the folder may be a
// separate mount that is writable while the rest of the library is not.
func (m *Manager) Writable() bool {
	dir := filepath.Join(m.root, filepath.FromSlash(m.uploadRel))
	_ = os.MkdirAll(dir, 0o755) // if this fails, CreateTemp below fails too
	f, err := os.CreateTemp(dir, ".write-test-*")
	if err != nil {
		return false
	}
	name := f.Name()
	f.Close()
	os.Remove(name)
	return true
}

// SevenZipAvailable reports whether .7z archives can be extracted.
func (m *Manager) SevenZipAvailable() bool {
	_, err := m.lookPath("7z")
	return err == nil
}

// IsArchive reports whether filename is an archive this package extracts.
func IsArchive(filename string) bool {
	switch strings.ToLower(filepath.Ext(filename)) {
	case ".zip", ".7z":
		return true
	}
	return false
}

// SanitizeFolderName turns a title into a safe single path segment.
func SanitizeFolderName(name string) string {
	name = strings.Map(func(r rune) rune {
		switch {
		case r < 0x20 || r == 0x7f:
			return -1
		case strings.ContainsRune(`/\:*?"<>|`, r):
			return ' '
		}
		return r
	}, name)
	name = strings.Trim(strings.Join(strings.Fields(name), " "), " .")
	if name == "" {
		return "upload"
	}
	if len(name) > 180 {
		name = strings.TrimSpace(strings.ToValidUTF8(name[:180], ""))
	}
	return name
}

// SanitizeFileName reduces an uploaded file's name to a safe base name ("" if
// nothing usable is left).
func SanitizeFileName(name string) string {
	name = strings.ReplaceAll(name, "\\", "/")
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	var b strings.Builder
	for _, r := range name {
		switch {
		case r < 0x20 || r == 0x7f:
		case strings.ContainsRune(`<>:"|?*`, r):
			b.WriteRune('_')
		default:
			b.WriteRune(r)
		}
	}
	s := strings.Trim(b.String(), " .")
	if len(s) > 200 {
		ext := filepath.Ext(s)
		if len(ext) > 20 {
			ext = ""
		}
		s = strings.ToValidUTF8(strings.TrimSuffix(s, ext)[:200-len(ext)], "") + ext
	}
	return s
}

// UniqueDestination returns dir/name, or "dir/name (2)", "(3)", ... , whichever
// does not exist yet. Uploads never overwrite or merge into existing items.
func UniqueDestination(dir, name string) string {
	candidate := filepath.Join(dir, name)
	for i := 2; ; i++ {
		// Any error (not just "does not exist") means nothing usable is there; the
		// create that follows reports a real problem, instead of looping forever on e.g.
		// "not a directory".
		if _, err := os.Lstat(candidate); err != nil {
			return candidate
		}
		candidate = filepath.Join(dir, fmt.Sprintf("%s (%d)", name, i))
	}
}

// uniqueFile is UniqueDestination for files: the counter goes before the
// extension ("a (2).mp3"), so the type is not lost.
func uniqueFile(dir, name string) string {
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	candidate := filepath.Join(dir, name)
	for i := 2; ; i++ {
		if _, err := os.Lstat(candidate); err != nil {
			return candidate
		}
		candidate = filepath.Join(dir, fmt.Sprintf("%s (%d)%s", stem, i, ext))
	}
}

// verifyDest checks that relPath is an existing folder inside the upload
// folder (never anywhere else in the library) and returns its clean relative
// path and real absolute path.
func (m *Manager) verifyDest(relPath string) (clean, full string, err error) {
	clean = library.CleanRel(relPath)
	if clean != m.uploadRel && !strings.HasPrefix(clean, m.uploadRel+"/") {
		return "", "", fmt.Errorf("%w: %q must be inside %q", ErrBadPath, relPath, m.uploadRel)
	}
	for _, seg := range strings.Split(clean, "/") {
		if strings.HasPrefix(seg, ".") {
			return "", "", fmt.Errorf("%w: hidden folders are not allowed", ErrBadPath)
		}
	}
	real, err := filepath.EvalSymlinks(filepath.Join(m.root, filepath.FromSlash(clean)))
	if err != nil {
		return "", "", fmt.Errorf("%w: %q", ErrNoDest, relPath)
	}
	if real != m.root && !strings.HasPrefix(real, m.root+string(filepath.Separator)) {
		return "", "", fmt.Errorf("%w: %q leaves the library", ErrBadPath, relPath)
	}
	if st, err := os.Stat(real); err != nil || !st.IsDir() {
		return "", "", fmt.Errorf("%w: %q", ErrNoDest, relPath)
	}
	return clean, real, nil
}

func validName(filename string) error {
	if filename == "" || len(filename) > 255 || filename == "." || filename == ".." ||
		strings.ContainsAny(filename, "/\\\x00") {
		return fmt.Errorf("%w: %q", ErrBadName, filename)
	}
	return nil
}

// Start reserves the destination of one upload batch. A non-blank title makes
// a fresh folder (never an existing one) inside the upload folder; a blank one
// uses the upload folder itself. Call once per batch, then send each file with
// Begin/Append/Complete using the returned relPath.
func (m *Manager) Start(title string) (name, relPath string, err error) {
	parent := filepath.Join(m.root, filepath.FromSlash(m.uploadRel))
	dest := parent
	if strings.TrimSpace(title) != "" {
		dest = UniqueDestination(parent, SanitizeFolderName(title))
	}
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return "", "", fmt.Errorf("creating upload folder: %w", err)
	}
	if dest == parent {
		return "", m.uploadRel, nil
	}
	name = filepath.Base(dest)
	return name, m.uploadRel + "/" + name, nil
}

// StartOrJoin is Start, except that a folder of that name which already exists is reused instead of
// a "(2)" one being made, so a later upload can add to an album already in the library. What is
// already there is never overwritten: see merge.
func (m *Manager) StartOrJoin(title string) (name, relPath string, err error) {
	if strings.TrimSpace(title) != "" {
		dest := filepath.Join(m.root, filepath.FromSlash(m.uploadRel), SanitizeFolderName(title))
		if st, err := os.Lstat(dest); err == nil && st.IsDir() { // a real folder, not a link
			name = filepath.Base(dest)
			return name, m.uploadRel + "/" + name, nil
		}
	}
	return m.Start(title)
}

// --- sessions ---

type chunk struct {
	Offset int64  `json:"offset"`
	Length int64  `json:"length"`
	CRC32  string `json:"crc32"`
}

type sessionMeta struct {
	RelPath  string  `json:"relPath"`
	Filename string  `json:"filename"`
	Size     int64   `json:"size"`
	Chunks   []chunk `json:"chunks"`
}

func (m *Manager) sessionKey(relPath, filename string) string {
	sum := sha256.Sum256([]byte(relPath + "\x00" + filename))
	return hex.EncodeToString(sum[:])
}

func (m *Manager) sessionPaths(relPath, filename string) (stagingPath, metaPath string) {
	key := m.sessionKey(relPath, filename)
	return filepath.Join(m.staging, key+".partial"), filepath.Join(m.staging, key+".json")
}

func readMeta(p string) (*sessionMeta, error) {
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	var meta sessionMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		return nil, err
	}
	return &meta, nil
}

// writeMeta is atomic (temp file + rename) so a crash never leaves a torn sidecar.
func writeMeta(p string, meta *sessionMeta) error {
	data, _ := json.Marshal(meta) // a plain struct of strings and integers cannot fail to encode
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

func (m *Manager) checkSpace(size int64) error {
	if m.minFree <= 0 {
		return nil
	}
	free, err := m.freeBytes(m.root)
	if err != nil {
		return nil // cannot tell: allow, rather than block every upload
	}
	if int64(free)-size < m.minFree {
		return fmt.Errorf("%w: this upload needs %s but only %s is free (%s must stay free)",
			ErrNoSpace, fmtSize(size), fmtSize(int64(free)), fmtSize(m.minFree))
	}
	return nil
}

func fmtSize(n int64) string {
	const gb = 1 << 30
	if n >= gb/10 {
		return fmt.Sprintf("%.2f GB", float64(n)/gb)
	}
	return fmt.Sprintf("%.0f MB", float64(n)/(1<<20))
}

// Begin opens or resumes the session for one file and returns the offset to
// resume from: 0 for a new session, or wherever an earlier attempt for the
// same folder, name and size stopped. A session with a different size is
// discarded (a different file that shares a name).
func (m *Manager) Begin(relPath, filename string, size int64) (int64, error) {
	if err := validName(filename); err != nil {
		return 0, err
	}
	if size < 0 {
		return 0, ErrBadSize
	}
	if size > m.maxBytes {
		return 0, fmt.Errorf("%w: %s exceeds the %s limit", ErrTooLarge, fmtSize(size), fmtSize(m.maxBytes))
	}
	clean, _, err := m.verifyDest(relPath)
	if err != nil {
		return 0, err
	}
	if strings.EqualFold(filepath.Ext(filename), ".7z") && !m.SevenZipAvailable() {
		return 0, ErrUnsupported
	}
	stagingPath, metaPath := m.sessionPaths(clean, filename)
	if err := os.MkdirAll(m.staging, 0o755); err != nil {
		return 0, fmt.Errorf("creating staging folder: %w", err)
	}

	// A finished outcome belongs to bytes this call is about to extend or
	// restart, so drop it. A running extraction is real and is left alone.
	key := stagingPath
	m.mu.Lock()
	if st, ok := m.completions[key]; !ok || st.State != Running {
		delete(m.completions, key)
	}
	m.mu.Unlock()

	if meta, err := readMeta(metaPath); err == nil && meta.RelPath == clean && meta.Filename == filename && meta.Size == size {
		if info, err := os.Stat(stagingPath); err == nil {
			return info.Size(), nil // resume
		}
	}

	if err := m.checkSpace(size); err != nil {
		return 0, err
	}
	if err := os.WriteFile(stagingPath, nil, 0o644); err != nil {
		return 0, fmt.Errorf("creating upload session: %w", err)
	}
	if err := writeMeta(metaPath, &sessionMeta{RelPath: clean, Filename: filename, Size: size}); err != nil {
		return 0, fmt.Errorf("creating upload session: %w", err)
	}
	return 0, nil
}

// Append writes one chunk. offset must equal the bytes already staged
// (OffsetMismatchError otherwise) and crc32Hex must match data
// (ChecksumMismatchError otherwise). It returns the new offset.
func (m *Manager) Append(relPath, filename string, offset int64, crc32Hex string, data []byte) (int64, error) {
	if err := validName(filename); err != nil {
		return 0, err
	}
	clean := library.CleanRel(relPath)
	stagingPath, metaPath := m.sessionPaths(clean, filename)
	meta, err := readMeta(metaPath)
	if err != nil {
		return 0, ErrNoSession
	}
	info, err := os.Stat(stagingPath)
	if err != nil {
		return 0, ErrNoSession
	}
	if info.Size() != offset {
		return info.Size(), &OffsetMismatchError{Current: info.Size()}
	}
	if offset+int64(len(data)) > meta.Size {
		return offset, ErrOverrun
	}
	if fmt.Sprintf("%08x", crc32.ChecksumIEEE(data)) != strings.ToLower(crc32Hex) {
		return offset, &ChecksumMismatchError{}
	}

	f, err := os.OpenFile(stagingPath, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return offset, fmt.Errorf("appending chunk: %w", err)
	}
	n, werr := f.Write(data)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return offset, fmt.Errorf("appending chunk: %w", werr)
	}

	meta.Chunks = append(meta.Chunks, chunk{Offset: offset, Length: int64(n), CRC32: strings.ToLower(crc32Hex)})
	if err := writeMeta(metaPath, meta); err != nil {
		return offset, fmt.Errorf("recording chunk: %w", err)
	}
	return offset + int64(n), nil
}

// --- completion ---

// Complete finalises a session once every chunk has landed. A loose audio file
// is placed immediately (State Done, or Failed if it is not audio). An archive
// starts verifying and extracting in the background and returns Running; poll
// StatusOf. Calling Complete again for the same file returns what is already
// tracked, so retries and reloads never start duplicate work.
func (m *Manager) Complete(relPath, filename string, size int64) (Status, error) {
	if err := validName(filename); err != nil {
		return Status{}, err
	}
	clean := library.CleanRel(relPath)
	stagingPath, metaPath := m.sessionPaths(clean, filename)
	key := stagingPath

	m.mu.Lock()
	if st, ok := m.completions[key]; ok {
		out := *st
		m.mu.Unlock()
		return out, nil
	}
	m.mu.Unlock()

	_, destDir, err := m.verifyDest(relPath)
	if err != nil {
		return Status{}, err
	}
	meta, err := readMeta(metaPath)
	if err != nil || meta.RelPath != clean || meta.Filename != filename || meta.Size != size {
		return Status{}, ErrNoSession
	}
	info, err := os.Stat(stagingPath)
	if err != nil {
		return Status{}, ErrNoSession
	}
	if info.Size() != size {
		return Status{}, fmt.Errorf("%w (received %d of %d bytes)", ErrIncomplete, info.Size(), size)
	}

	if !IsArchive(filename) {
		st := m.placeLoose(clean, destDir, stagingPath, metaPath, filename, size)
		m.setStatus(key, st)
		return st, nil
	}

	running := Status{State: Running, TotalBytes: size, Path: clean}
	m.setStatus(key, running)
	go m.runArchive(key, clean, destDir, stagingPath, metaPath, meta, size)
	return running, nil
}

func (m *Manager) setStatus(key string, st Status) {
	m.mu.Lock()
	m.completions[key] = &st
	m.mu.Unlock()
}

func discard(stagingPath, metaPath string) {
	os.Remove(stagingPath)
	os.Remove(metaPath)
}

func isAudio(p string) bool {
	f, err := os.Open(p)
	if err != nil {
		return false
	}
	defer f.Close()
	buf := make([]byte, 512)
	n, _ := io.ReadFull(f, buf)
	return sniff.Detect(buf[:n], filepath.Base(p)) != sniff.Unknown
}

// placeLoose moves a finished single file into the destination folder, but
// only if its content really is audio.
func (m *Manager) placeLoose(clean, destDir, stagingPath, metaPath, filename string, size int64) Status {
	if !isAudio(stagingPath) {
		discard(stagingPath, metaPath)
		return Status{State: Failed, TotalBytes: size, Error: fmt.Sprintf("%s is not an audio file", filename)}
	}
	name := SanitizeFileName(filename)
	if name == "" {
		name = "track"
	}
	if sameContent(stagingPath, filepath.Join(destDir, name)) {
		discard(stagingPath, metaPath)
		return Status{State: Done, BytesWritten: size, TotalBytes: size, Duplicates: 1, Path: clean + "/" + name}
	}
	target := uniqueFile(destDir, name)
	if err := m.move(stagingPath, target); err != nil {
		return Status{State: Failed, TotalBytes: size, Error: fmt.Sprintf("placing %s: %v", filename, err)}
	}
	os.Remove(metaPath)
	return Status{State: Done, BytesWritten: size, TotalBytes: size, Tracks: 1, Path: clean + "/" + filepath.Base(target)}
}

type mergeStats struct{ Tracks, Images, Duplicates int }

// tally counts what is at path (a file, or everything below a folder) as added, or as
// duplicates when dup is set.
func (s *mergeStats) tally(path string, dup bool) {
	_ = filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
		switch {
		case err != nil || d.IsDir():
		case dup:
			s.Duplicates++
		case isAudio(p):
			s.Tracks++
		default:
			s.Images++ // prune left nothing but audio and pictures
		}
		return nil
	})
}

// sameContent is true when both are regular files with identical bytes.
func sameContent(a, b string) bool {
	fa, err := os.Open(a)
	if err != nil {
		return false
	}
	defer fa.Close()
	fb, err := os.Open(b)
	if err != nil {
		return false
	}
	defer fb.Close()
	ia, errA := fa.Stat()
	ib, errB := fb.Stat()
	if errA != nil || errB != nil || !ia.Mode().IsRegular() || !ib.Mode().IsRegular() || ia.Size() != ib.Size() {
		return false
	}
	bufA, bufB := make([]byte, 64<<10), make([]byte, 64<<10)
	for {
		na, errA := io.ReadFull(fa, bufA)
		nb, errB := io.ReadFull(fb, bufB)
		if na != nb || !bytes.Equal(bufA[:na], bufB[:nb]) {
			return false
		}
		if errA != nil || errB != nil {
			return errA == errB // both ended together
		}
	}
}

// merge moves everything in src into dst without ever overwriting: a folder that already exists
// is merged into; a file that already exists with identical bytes is dropped as a duplicate
// (nothing new to add); a file or folder that clashes with something different is kept next to
// it under a numbered name; anything else is moved in whole.
func (m *Manager) merge(src, dst string, st *mergeStats) error {
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, e := range entries {
		from, to := filepath.Join(src, e.Name()), filepath.Join(dst, e.Name())
		existing, statErr := os.Lstat(to)
		switch {
		case statErr != nil:
		case e.IsDir() && existing.IsDir():
			if err := m.merge(from, to, st); err != nil {
				return err
			}
			continue
		case !e.IsDir() && sameContent(from, to):
			st.tally(from, true)
			os.Remove(from)
			continue
		case e.IsDir():
			to = UniqueDestination(dst, e.Name())
		default:
			to = uniqueFile(dst, e.Name())
		}
		st.tally(from, false)
		if err := m.rename(from, to); err != nil {
			return fmt.Errorf("adding %s to the library: %v", e.Name(), err)
		}
	}
	return nil
}

func (m *Manager) extractDir(destDir, key string) string {
	return filepath.Join(destDir, ".extract-"+filepath.Base(key)[:12])
}

// runArchive does the slow part of an archive's completion in the background.
func (m *Manager) runArchive(key, clean, destDir, stagingPath, metaPath string, meta *sessionMeta, size int64) {
	m.extractMu.Lock()
	defer m.extractMu.Unlock()
	fail := func(format string, args ...any) {
		m.setStatus(key, Status{State: Failed, TotalBytes: size, Error: fmt.Sprintf(format, args...)})
	}

	if testErr := m.testArchive(meta.Filename, stagingPath); testErr != nil {
		err := repairOrDiscard(stagingPath, metaPath, meta, testErr)
		var corrupt *CorruptUploadError
		if errors.As(err, &corrupt) {
			m.setStatus(key, Status{State: Corrupted, TotalBytes: size, ResumeOffset: corrupt.ResumeOffset, Error: err.Error()})
		} else {
			fail("%v", err)
		}
		return
	}

	// Extract into a hidden scratch folder beside the destination (same file
	// system, so the final moves are renames), so nothing partial or unwanted
	// ever appears in the library.
	tmp := m.extractDir(destDir, key) // created by the extractor itself
	os.RemoveAll(tmp)
	defer os.RemoveAll(tmp)
	var err error
	if strings.EqualFold(filepath.Ext(meta.Filename), ".zip") {
		err = extractZip(stagingPath, tmp, m.maxBytes)
	} else {
		err = m.extract7z(stagingPath, tmp)
	}
	if err != nil {
		fail("extracting %s: %v", filepath.Base(meta.Filename), err)
		return
	}

	pr := prune(tmp)
	if pr.Tracks == 0 {
		fail("%s contains no audio files", filepath.Base(meta.Filename))
		discard(stagingPath, metaPath)
		return
	}
	if !joinsExistingFolder(tmp, destDir) {
		flattenWrapper(tmp)
	}
	var added mergeStats
	if err := m.merge(tmp, destDir, &added); err != nil {
		fail("%v", err)
		return
	}

	discard(stagingPath, metaPath)
	done := Status{State: Done, BytesWritten: size, TotalBytes: size, Tracks: added.Tracks, Images: added.Images, Duplicates: added.Duplicates, Skipped: len(pr.Skipped), Path: clean}
	if len(pr.Skipped) > 0 {
		done.SkippedTypes = countTypes(pr.Skipped)
		done.HasReport = m.writeReport(m.sessionKey(clean, meta.Filename), meta.Filename, pr.Skipped) == nil
	}
	m.setStatus(key, done)
}

// StatusOf reports a tracked completion. found is false if nothing is tracked
// (never completed, or the server restarted since: extraction does not resume
// by itself, so call Complete again).
func (m *Manager) StatusOf(relPath, filename string) (Status, bool) {
	clean := library.CleanRel(relPath)
	key, _ := m.sessionPaths(clean, filename)
	m.mu.Lock()
	existing, ok := m.completions[key]
	if !ok {
		m.mu.Unlock()
		return Status{}, false
	}
	st := *existing
	m.mu.Unlock()

	if st.State == Running {
		if _, destDir, err := m.verifyDest(relPath); err == nil {
			st.BytesWritten = dirSize(m.extractDir(destDir, key))
		}
	}
	return st, true
}

// dirSize sums the sizes of regular files below dir (0 if it does not exist).
func dirSize(dir string) int64 {
	var n int64
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return nil
		}
		if info, err := d.Info(); err == nil {
			n += info.Size()
		}
		return nil
	})
	return n
}

// --- archives ---

func (m *Manager) testArchive(filename, archivePath string) error {
	if !strings.EqualFold(filepath.Ext(filename), ".zip") {
		out, err := m.exec("7z", "t", archivePath)
		if err != nil {
			return fmt.Errorf("7z integrity test failed: %w: %s", err, strings.TrimSpace(string(out)))
		}
		return nil
	}
	r, err := openZip(archivePath)
	if err != nil {
		return fmt.Errorf("opening zip: %w", err)
	}
	defer r.Close()
	for _, f := range r.File {
		if f.FileInfo().IsDir() {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return fmt.Errorf("reading zip entry %q: %w", f.Name, err)
		}
		_, copyErr := io.Copy(io.Discard, rc) // verifies the entry's CRC32
		rc.Close()
		if copyErr != nil {
			return fmt.Errorf("zip entry %q failed its checksum: %w", f.Name, copyErr)
		}
	}
	return nil
}

// openZip opens a zip. Depending on GODEBUG, Go reports ErrInsecurePath (entries
// like "../x") together with a usable reader; those entries are refused one by one
// during extraction, which gives a clearer error than "damaged archive".
func openZip(path string) (*zip.ReadCloser, error) {
	r, err := zip.OpenReader(path)
	if err != nil && !errors.Is(err, zip.ErrInsecurePath) {
		return nil, err
	}
	return r, nil
}

func (m *Manager) extract7z(archivePath, destDir string) error {
	out, err := m.exec("7z", "x", "-y", "-o"+destDir, archivePath)
	if err != nil {
		return fmt.Errorf("7z extraction failed: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// extractZip extracts archivePath into destDir, refusing entries that escape it
// (Zip Slip) and refusing to write more than budget bytes in total (zip bombs).
func extractZip(archivePath, destDir string, budget int64) error {
	r, err := openZip(archivePath)
	if err != nil {
		return fmt.Errorf("opening zip: %w", err)
	}
	defer r.Close()
	for _, f := range r.File {
		target := filepath.Join(destDir, filepath.FromSlash(f.Name))
		if target != destDir && !strings.HasPrefix(target, destDir+string(filepath.Separator)) {
			return fmt.Errorf("zip entry %q escapes the destination folder", f.Name)
		}
		if f.Mode()&os.ModeSymlink != 0 {
			continue // never materialise links: their "content" is only a target path
		}
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}
		n, err := writeZipEntry(f, target, budget)
		if err != nil {
			return err
		}
		if budget -= n; budget < 0 {
			return fmt.Errorf("%w: archive expands past the size limit", ErrTooLarge)
		}
	}
	return nil
}

// writeZipEntry writes one entry, stopping one byte past budget so a lying
// header cannot fill the disk.
func writeZipEntry(f *zip.File, target string, budget int64) (int64, error) {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return 0, err
	}
	rc, err := f.Open()
	if err != nil {
		return 0, fmt.Errorf("reading zip entry %q: %w", f.Name, err)
	}
	defer rc.Close()
	out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return 0, fmt.Errorf("writing %q: %w", f.Name, err)
	}
	n, err := io.Copy(out, io.LimitReader(rc, budget+1))
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return n, fmt.Errorf("writing %q: %w", f.Name, err)
	}
	return n, nil
}

// maxImagesPerFolder bounds the pictures kept per folder: enough for a cover (and a back
// cover), not hundreds of booklet scans.
const maxImagesPerFolder = 3

var imageTypes = map[string]bool{"image/jpeg": true, "image/png": true, "image/gif": true, "image/webp": true}

// isImage is true for a real picture: a picture extension and picture content.
func isImage(p string) bool {
	if !library.IsImageName(p) {
		return false
	}
	f, err := os.Open(p)
	if err != nil {
		return false
	}
	defer f.Close()
	buf := make([]byte, 512)
	n, _ := io.ReadFull(f, buf)
	return imageTypes[http.DetectContentType(buf[:n])]
}

type pruneResult struct {
	Tracks  int      // audio files kept
	Images  int      // pictures kept
	Skipped []string // everything dropped, as slash-separated paths relative to the folder, sorted
}

// prune reduces dir to what a music library wants: regular audio files, plus up to
// maxImagesPerFolder real pictures per folder (the best cover candidates, see
// library.RankImages) to serve as cover art. Symlinks, scripts, text files, thumbnails and
// everything else are removed, then empty folders are removed.
func prune(dir string) pruneResult {
	var res pruneResult
	var dirs []string
	pics := map[string][]string{} // folder -> names of the pictures in it
	drop := func(p string) {
		os.Remove(p)
		rel, _ := filepath.Rel(dir, p)
		res.Skipped = append(res.Skipped, filepath.ToSlash(rel))
	}
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		switch {
		case err != nil:
		case d.IsDir():
			if p != dir {
				dirs = append(dirs, p)
			}
		case !d.Type().IsRegular():
			drop(p)
		case isAudio(p):
			res.Tracks++
		case isImage(p):
			pics[filepath.Dir(p)] = append(pics[filepath.Dir(p)], d.Name())
		default:
			drop(p)
		}
		return nil
	})
	folders := make([]string, 0, len(pics))
	for f := range pics {
		folders = append(folders, f)
	}
	sort.Strings(folders)
	for _, f := range folders {
		for i, name := range library.RankImages(pics[f]) {
			if i < maxImagesPerFolder {
				res.Images++
			} else {
				drop(filepath.Join(f, name))
			}
		}
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		os.Remove(dirs[i]) // only succeeds when empty
	}
	sort.Strings(res.Skipped)
	return res
}

// typeOf names the kind of a dropped file for the summary: its extension, "no extension", or
// "other" when what follows the last dot is not a plausible extension (a folder-ish name with a
// dot in it).
func typeOf(rel string) string {
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(rel), "."))
	switch {
	case ext == "":
		return "no extension"
	case len(ext) > 6 || strings.IndexFunc(ext, func(r rune) bool { return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9') }) >= 0:
		return "other"
	}
	return ext
}

func countTypes(skipped []string) map[string]int {
	out := map[string]int{}
	for _, rel := range skipped {
		out[typeOf(rel)]++
	}
	return out
}

const maxReportLines = 20000

func (m *Manager) reportPath(key string) string { return filepath.Join(m.staging, "reports", key+".txt") }

// writeReport saves the list of files an archive upload dropped, so you can see exactly what
// was left out.
func (m *Manager) writeReport(key, filename string, skipped []string) error {
	var b strings.Builder
	fmt.Fprintf(&b, "%d files from %s were not added to the library.\n", len(skipped), filename)
	fmt.Fprintf(&b, "Only audio files and up to %d cover pictures per folder are kept.\n\n", maxImagesPerFolder)
	for i, rel := range skipped {
		if i == maxReportLines {
			fmt.Fprintf(&b, "... and %d more\n", len(skipped)-maxReportLines)
			break
		}
		b.WriteString(rel + "\n")
	}
	if err := os.MkdirAll(filepath.Dir(m.reportPath(key)), 0o755); err != nil {
		return err
	}
	return os.WriteFile(m.reportPath(key), []byte(b.String()), 0o644)
}

// Report returns the path of the saved list of files dropped from an archive upload.
func (m *Manager) Report(relPath, filename string) (string, error) {
	if err := validName(filename); err != nil {
		return "", err
	}
	p := m.reportPath(m.sessionKey(library.CleanRel(relPath), filename))
	if _, err := os.Stat(p); err != nil {
		return "", ErrNoReport
	}
	return p, nil
}

// joinsExistingFolder is true when the archive holds a single folder that already exists in the
// destination. That folder is then real content to merge into, not a wrapper to remove: without
// this, an archive with a few new files for one album would lose its folder name and land at the
// top of the destination.
func joinsExistingFolder(tmp, destDir string) bool {
	entries, err := os.ReadDir(tmp)
	if err != nil || len(entries) != 1 || !entries[0].IsDir() {
		return false
	}
	st, err := os.Lstat(filepath.Join(destDir, entries[0].Name()))
	return err == nil && st.IsDir()
}

// flattenWrapper moves a lone top-level folder's contents up into dir, for as
// long as dir holds exactly one entry and it is a folder ("Album/Disc 1/x.flac"
// archives are common).
func flattenWrapper(dir string) {
	for depth := 0; depth < 8; depth++ { // bounded: never loops forever on odd layouts
		entries, err := os.ReadDir(dir)
		if err != nil || len(entries) != 1 || !entries[0].IsDir() {
			return
		}
		wrapper := filepath.Join(dir, entries[0].Name())
		inner, _ := os.ReadDir(wrapper)
		for _, e := range inner {
			if os.Rename(filepath.Join(wrapper, e.Name()), filepath.Join(dir, e.Name())) != nil {
				return // e.g. "Album/Album": leave the nesting as it is
			}
		}
		os.Remove(wrapper)
	}
}

// repairOrDiscard runs after an archive fails its integrity test. It
// re-checksums every chunk recorded for the session against the bytes on disk:
// a chunk that no longer matches is the damage (bitrot after arrival), so the
// session is rewound to just before it and a *CorruptUploadError is returned so
// the client re-sends only from there. If every chunk still matches, a resend
// cannot help (the source file itself is bad), so the session is discarded.
func repairOrDiscard(stagingPath, metaPath string, meta *sessionMeta, testErr error) error {
	f, err := os.Open(stagingPath)
	if err != nil {
		discard(stagingPath, metaPath)
		return fmt.Errorf("archive failed its integrity test (%v) and could not be re-examined: %w", testErr, err)
	}
	defer f.Close()

	for _, c := range meta.Chunks {
		buf := make([]byte, c.Length)
		_, readErr := io.ReadFull(io.NewSectionReader(f, c.Offset, c.Length), buf)
		if readErr == nil && fmt.Sprintf("%08x", crc32.ChecksumIEEE(buf)) == c.CRC32 {
			continue
		}
		if err := os.Truncate(stagingPath, c.Offset); err != nil {
			discard(stagingPath, metaPath)
			return fmt.Errorf("archive failed its integrity test (%v), and rewinding the session failed too: %w", testErr, err)
		}
		meta.Chunks = chunksBefore(meta.Chunks, c.Offset)
		_ = writeMeta(metaPath, meta)
		return &CorruptUploadError{ResumeOffset: c.Offset}
	}

	discard(stagingPath, metaPath)
	return fmt.Errorf("the archive failed its integrity test, but every uploaded chunk matches what was verified on arrival, "+
		"so the source file itself is damaged and re-uploading it will not help: %w", testErr)
}

func chunksBefore(chunks []chunk, offset int64) []chunk {
	var out []chunk
	for _, c := range chunks {
		if c.Offset < offset {
			out = append(out, c)
		}
	}
	return out
}

// --- housekeeping ---

// PurgeExpired removes sessions whose files were last touched at least maxAge
// ago (an upload nobody resumed). maxAge <= 0 removes every session. A session
// is judged as a whole (its .partial and .json together), so it is never
// half-purged; an orphaned .partial from a crash is swept too.
func (m *Manager) PurgeExpired(maxAge time.Duration) (int, error) {
	entries, err := os.ReadDir(m.staging)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	defer m.purgeReports(maxAge)
	if err != nil {
		return 0, fmt.Errorf("reading upload staging folder: %w", err)
	}

	last := map[string]time.Time{}
	for _, e := range entries {
		key, ok := strings.CutSuffix(e.Name(), ".partial")
		if !ok {
			if key, ok = strings.CutSuffix(e.Name(), ".json"); !ok {
				continue
			}
		}
		if info, err := e.Info(); err == nil && info.ModTime().After(last[key]) {
			last[key] = info.ModTime()
		}
	}

	cutoff := time.Now().Add(-maxAge)
	purged := 0
	for key, touched := range last {
		if maxAge > 0 && touched.After(cutoff) {
			continue
		}
		os.Remove(filepath.Join(m.staging, key+".partial"))
		os.Remove(filepath.Join(m.staging, key+".json"))
		os.Remove(filepath.Join(m.staging, key+".json.tmp"))
		m.mu.Lock()
		delete(m.completions, filepath.Join(m.staging, key+".partial"))
		m.mu.Unlock()
		purged++
	}
	return purged, nil
}

// purgeReports removes saved skip lists older than maxAge (all of them if maxAge <= 0).
func (m *Manager) purgeReports(maxAge time.Duration) {
	des, _ := os.ReadDir(filepath.Join(m.staging, "reports"))
	cutoff := time.Now().Add(-maxAge)
	for _, de := range des {
		if info, err := de.Info(); err == nil && (maxAge <= 0 || !info.ModTime().After(cutoff)) {
			os.Remove(filepath.Join(m.staging, "reports", de.Name()))
		}
	}
}

// RunJanitor purges abandoned sessions older than maxAge every interval until
// ctx is cancelled (and once up front).
func (m *Manager) RunJanitor(ctx context.Context, maxAge, interval time.Duration) {
	sweep := func() {
		n, err := m.PurgeExpired(maxAge)
		switch {
		case err != nil:
			m.logf("upload janitor: %v", err)
		case n > 0:
			m.logf("upload janitor: purged %d abandoned session(s) older than %s", n, maxAge)
		}
	}
	sweep()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			sweep()
		}
	}
}
