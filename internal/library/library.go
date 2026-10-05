// Package library exposes the music folder: browsing, safe path handling,
// delete-with-undo and uploads. Every caller-supplied path is a slash
// separated path relative to the library root and goes through Resolve.
package library

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"masterplayer/internal/sniff"
)

var (
	ErrOutside  = errors.New("path is outside the library")
	ErrHidden   = errors.New("hidden paths are not accessible")
	ErrReadOnly = errors.New("library is read-only")
	ErrNotAudio = errors.New("not an audio file")
	ErrNotFile  = errors.New("not a regular file")
	ErrExists   = errors.New("destination already exists")
	ErrTooLarge = errors.New("file too large")
	ErrBadToken = errors.New("invalid undo token")
	ErrBadName  = errors.New("invalid file name")
)

const maxDepth = 32

// Entry is one row in a folder listing.
type Entry struct {
	Name     string `json:"name"`
	Path     string `json:"path"`
	IsDir    bool   `json:"isDir"`
	Kind     string `json:"kind,omitempty"`
	Playable bool   `json:"playable"`
	Size     int64  `json:"size"`
}

// Trashed describes a file moved to the trash, restorable with Undo.
type Trashed struct {
	Token string
	Path  string
	Name  string
}

// Library is a music folder.
type Library struct {
	root     string
	trashDir string
	readOnly bool
	now      func() time.Time
}

// Seams for tests: a real file system rarely fails at exactly these points.
var (
	randRead   = rand.Read
	renameFile = os.Rename
)

// maxTracks caps how many tracks Tracks returns (a variable so tests can lower it).
var maxTracks = 50000

// New opens root. trashDir defaults to <root>/.trash (keep it on the same
// filesystem so deletes are atomic renames). forceReadOnly disables delete and
// upload even when the directory is writable.
func New(root, trashDir string, forceReadOnly bool) (*Library, error) {
	real, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, fmt.Errorf("music directory: %w", err)
	}
	if st, err := os.Stat(real); err != nil || !st.IsDir() {
		return nil, fmt.Errorf("music directory %q is not a directory", root)
	}
	if trashDir == "" {
		trashDir = filepath.Join(real, ".trash")
	}
	return &Library{root: real, trashDir: trashDir, readOnly: forceReadOnly || !writable(real), now: time.Now}, nil
}

func writable(dir string) bool {
	f, err := os.CreateTemp(dir, ".mp-write-test-*")
	if err != nil {
		return false
	}
	name := f.Name()
	f.Close()
	os.Remove(name)
	return true
}

// ReadOnly is true when the music directory cannot be written (delete and
// upload are then disabled).
func (l *Library) ReadOnly() bool { return l.readOnly }

// CleanRel normalises a user path to "a/b/c" ("" is the root).
func CleanRel(rel string) string {
	c := path.Clean("/" + filepath.ToSlash(rel))
	return strings.TrimPrefix(c, "/")
}

func relJoin(dir, name string) string {
	if dir == "" {
		return name
	}
	return dir + "/" + name
}

// Resolve maps a relative path to an absolute one, refusing hidden segments
// and anything that escapes the root (including through symlinks).
func (l *Library) Resolve(rel string) (string, error) {
	clean := CleanRel(rel)
	if clean != "" {
		for _, seg := range strings.Split(clean, "/") {
			if strings.HasPrefix(seg, ".") {
				return "", ErrHidden
			}
		}
	}
	abs := filepath.Join(l.root, filepath.FromSlash(clean))
	real, err := filepath.EvalSymlinks(abs)
	switch {
	case err == nil:
		if real != l.root && !strings.HasPrefix(real, l.root+string(filepath.Separator)) {
			return "", ErrOutside
		}
	case errors.Is(err, fs.ErrNotExist):
		// Not there yet (upload target, undo destination): lexical check above is enough.
	default:
		return "", err
	}
	return abs, nil
}

func readHeader(abs string) []byte {
	f, err := os.Open(abs)
	if err != nil {
		return nil
	}
	defer f.Close()
	buf := make([]byte, 512)
	n, _ := io.ReadFull(f, buf)
	return buf[:n]
}

// kindOf is the cheap classification used for listings: extension first,
// content second.
func kindOf(abs, name string) sniff.Kind {
	if k := sniff.FromExt(name); k != sniff.Unknown {
		return k
	}
	return sniff.Detect(readHeader(abs), name)
}

func entryFor(dir, absDir string, de fs.DirEntry) (Entry, bool) {
	name := de.Name()
	if strings.HasPrefix(name, ".") {
		return Entry{}, false
	}
	abs := filepath.Join(absDir, name)
	info, err := os.Stat(abs) // follows symlinks
	if err != nil {
		return Entry{}, false
	}
	e := Entry{Name: name, Path: relJoin(dir, name), IsDir: info.IsDir(), Size: info.Size()}
	if !e.IsDir {
		if k := kindOf(abs, name); k != sniff.Unknown {
			e.Kind = k.String()
			e.Playable = true
		}
	}
	return e, true
}

func sortEntries(es []Entry) {
	sort.SliceStable(es, func(i, j int) bool {
		if es[i].IsDir != es[j].IsDir {
			return es[i].IsDir
		}
		a, b := strings.ToLower(es[i].Name), strings.ToLower(es[j].Name)
		if a != b {
			return a < b
		}
		return es[i].Name < es[j].Name
	})
}

// Browse lists one folder: folders first, then files, case-insensitive;
// hidden entries are skipped.
func (l *Library) Browse(rel string) ([]Entry, error) {
	abs, err := l.Resolve(rel)
	if err != nil {
		return nil, err
	}
	des, err := os.ReadDir(abs) // fails for missing paths and for files
	if err != nil {
		return nil, err
	}
	dir := CleanRel(rel)
	out := make([]Entry, 0, len(des))
	for _, de := range des {
		if e, ok := entryFor(dir, abs, de); ok {
			out = append(out, e)
		}
	}
	sortEntries(out)
	return out, nil
}

// Tracks returns every playable file under rel in playback order (the same
// order a user sees when walking the folders), so auto-advance crosses
// folder boundaries.
func (l *Library) Tracks(rel string) ([]Entry, error) {
	es, err := l.Browse(rel)
	if err != nil {
		return nil, err
	}
	var out []Entry
	l.collect(es, 0, &out)
	return out, nil
}

// collect appends the playable files of es, descending into folders first (the
// order Browse returns). Unreadable sub-folders are skipped; symlink loops end
// at maxDepth.
func (l *Library) collect(es []Entry, depth int, out *[]Entry) {
	for _, e := range es {
		if len(*out) >= maxTracks {
			return
		}
		switch {
		case e.IsDir:
			if depth < maxDepth {
				if sub, err := l.Browse(e.Path); err == nil {
					l.collect(sub, depth+1, out)
				}
			}
		case e.Playable:
			*out = append(*out, e)
		}
	}
}

// Describe returns the Entry for a single path.
func (l *Library) Describe(rel string) (Entry, error) {
	abs, err := l.Resolve(rel)
	if err != nil {
		return Entry{}, err
	}
	st, err := os.Stat(abs)
	if err != nil {
		return Entry{}, err
	}
	clean := CleanRel(rel)
	e := Entry{Name: filepath.Base(abs), Path: clean, IsDir: st.IsDir(), Size: st.Size()}
	if !e.IsDir {
		if k := sniff.Detect(readHeader(abs), e.Name); k != sniff.Unknown {
			e.Kind = k.String()
			e.Playable = true
		}
	}
	return e, nil
}

// Kind sniffs a file's real format (content first) and returns its absolute path.
func (l *Library) Kind(rel string) (sniff.Kind, string, error) {
	abs, err := l.Resolve(rel)
	if err != nil {
		return sniff.Unknown, "", err
	}
	st, err := os.Stat(abs)
	if err != nil {
		return sniff.Unknown, "", err
	}
	if !st.Mode().IsRegular() {
		return sniff.Unknown, "", ErrNotFile
	}
	k := sniff.Detect(readHeader(abs), filepath.Base(abs))
	if k == sniff.Unknown {
		return sniff.Unknown, "", ErrNotAudio
	}
	return k, abs, nil
}

var tokenRE = regexp.MustCompile(`^[0-9a-f]{16}$`)

func randToken() (string, error) {
	b := make([]byte, 8)
	if _, err := randRead(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// Delete moves a file into the trash. It is restorable with Undo until
// PurgeTrash removes it.
func (l *Library) Delete(rel string) (Trashed, error) {
	if l.readOnly {
		return Trashed{}, ErrReadOnly
	}
	abs, err := l.Resolve(rel)
	if err != nil {
		return Trashed{}, err
	}
	st, err := os.Stat(abs)
	if err != nil {
		return Trashed{}, err
	}
	if !st.Mode().IsRegular() {
		return Trashed{}, ErrNotFile
	}
	token, err := randToken()
	if err != nil {
		return Trashed{}, err
	}
	dir := filepath.Join(l.trashDir, token)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Trashed{}, err
	}
	clean := CleanRel(rel)
	if err := os.WriteFile(filepath.Join(dir, "orig"), []byte(clean), 0o644); err != nil {
		os.RemoveAll(dir)
		return Trashed{}, err
	}
	if err := moveFile(abs, filepath.Join(dir, "f")); err != nil {
		os.RemoveAll(dir)
		return Trashed{}, err
	}
	return Trashed{Token: token, Path: clean, Name: filepath.Base(abs)}, nil
}

// Undo restores a trashed file to where it was and returns its path.
func (l *Library) Undo(token string) (string, error) {
	if l.readOnly {
		return "", ErrReadOnly
	}
	if !tokenRE.MatchString(token) {
		return "", ErrBadToken
	}
	dir := filepath.Join(l.trashDir, token)
	orig, err := os.ReadFile(filepath.Join(dir, "orig"))
	if err != nil {
		return "", ErrBadToken
	}
	dest, err := l.Resolve(string(orig))
	if err != nil {
		return "", err
	}
	if _, err := os.Lstat(dest); err == nil {
		return "", ErrExists
	}
	_ = os.MkdirAll(filepath.Dir(dest), 0o755) // if this fails, the move below reports why
	if err := moveFile(filepath.Join(dir, "f"), dest); err != nil {
		return "", err
	}
	os.RemoveAll(dir)
	return CleanRel(string(orig)), nil
}

// PurgeTrash permanently removes trash entries older than maxAge.
func (l *Library) PurgeTrash(maxAge time.Duration) (int, error) {
	des, err := os.ReadDir(l.trashDir)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	cutoff := l.now().Add(-maxAge)
	n := 0
	for _, de := range des {
		info, err := de.Info()
		if err != nil || !info.IsDir() || !info.ModTime().Before(cutoff) {
			continue
		}
		if os.RemoveAll(filepath.Join(l.trashDir, de.Name())) == nil {
			n++
		}
	}
	return n, nil
}

func moveFile(src, dst string) error {
	if err := renameFile(src, dst); err == nil {
		return nil
	}
	// Cross-device or similar: copy then remove.
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(dst)
		return err
	}
	return os.Remove(src)
}

// SanitizeName reduces an uploaded file name to a safe base name.
func SanitizeName(name string) string {
	base := path.Base(strings.ReplaceAll(name, "\\", "/"))
	var b strings.Builder
	for _, r := range base {
		switch {
		case r < 0x20 || r == 0x7f:
		case strings.ContainsRune(`<>:"|?*`, r):
			b.WriteRune('_')
		default:
			b.WriteRune(r)
		}
	}
	s := strings.Trim(b.String(), " .")
	if s == "" || s == "/" {
		return ""
	}
	if rs := []rune(s); len(rs) > 200 {
		ext := path.Ext(s)
		if len([]rune(ext)) > 20 {
			ext = ""
		}
		stem := []rune(strings.TrimSuffix(s, ext))
		s = string(stem[:200-len([]rune(ext))]) + ext
	}
	return s
}

func uniqueName(dir, name string) string {
	if _, err := os.Lstat(filepath.Join(dir, name)); err != nil {
		return name
	}
	ext := path.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	for i := 1; ; i++ {
		cand := fmt.Sprintf("%s (%d)%s", stem, i, ext)
		if _, err := os.Lstat(filepath.Join(dir, cand)); err != nil {
			return cand
		}
	}
}

// SaveFile stores an uploaded file under dirRel after checking that it
// really is audio. It never overwrites; clashes get a " (1)" suffix.
func (l *Library) SaveFile(dirRel, name string, r io.Reader, maxBytes int64) (string, sniff.Kind, error) {
	if l.readOnly {
		return "", sniff.Unknown, ErrReadOnly
	}
	dirAbs, err := l.Resolve(dirRel)
	if err != nil {
		return "", sniff.Unknown, err
	}
	safe := SanitizeName(name)
	if safe == "" {
		return "", sniff.Unknown, ErrBadName
	}
	br := bufio.NewReaderSize(r, 4096)
	head, _ := br.Peek(512)
	kind := sniff.Detect(head, safe)
	if kind == sniff.Unknown {
		return "", sniff.Unknown, ErrNotAudio
	}
	_ = os.MkdirAll(dirAbs, 0o755) // if this fails, CreateTemp below reports why
	tmp, err := os.CreateTemp(dirAbs, ".upload-*")
	if err != nil {
		return "", sniff.Unknown, err
	}
	n, err := io.Copy(tmp, io.LimitReader(br, maxBytes+1))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp.Name())
		return "", sniff.Unknown, err
	}
	if n > maxBytes {
		os.Remove(tmp.Name())
		return "", sniff.Unknown, ErrTooLarge
	}
	final := uniqueName(dirAbs, safe)
	_ = os.Chmod(tmp.Name(), 0o644) // best effort: CreateTemp files are 0600
	if err := renameFile(tmp.Name(), filepath.Join(dirAbs, final)); err != nil {
		os.Remove(tmp.Name())
		return "", sniff.Unknown, err
	}
	return relJoin(CleanRel(dirRel), final), kind, nil
}
