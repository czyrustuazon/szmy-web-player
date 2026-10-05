// Package library exposes the music folder: browsing, safe path handling,
// delete-with-undo and uploads. Every caller-supplied path is a slash
// separated path relative to the library root and goes through Resolve.
package library

import (
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
	"syscall"
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
	ErrBadName  = errors.New("invalid folder name")
	ErrNotDir   = errors.New("not a folder")
	ErrBadToken = errors.New("invalid undo token")
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
	IsDir bool
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

// Root is the library's real (symlink-resolved) root path.
func (l *Library) Root() string { return l.root }

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
		// Not there yet (upload target, undo destination): whatever it will be created in
		// must still be inside the library, so a symlinked folder cannot lead out of it.
		if !l.insideExisting(filepath.Dir(abs)) {
			return "", ErrOutside
		}
	default:
		return "", err
	}
	return abs, nil
}

// insideExisting reports whether the nearest existing folder at or above dir (a path inside
// the root, as Resolve builds it), with symlinks resolved, is inside the library. A link that
// leads nowhere counts as outside, and so does everything once the library itself is gone.
func (l *Library) insideExisting(dir string) bool {
	for {
		real, err := filepath.EvalSymlinks(dir)
		if err == nil {
			return real == l.root || strings.HasPrefix(real, l.root+string(filepath.Separator))
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return false
		}
		if _, err := os.Lstat(dir); err == nil {
			return false // it exists, but only as a dangling link
		}
		if dir == l.root {
			return false
		}
		dir = filepath.Dir(dir)
	}
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
	if !info.IsDir() && IsImageName(name) {
		return Entry{}, false // pictures are cover art, not something to browse
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

// Delete moves a file or folder into the trash. It is restorable with Undo
// until PurgeTrash removes it.
func (l *Library) Delete(rel string) (Trashed, error) {
	if l.readOnly {
		return Trashed{}, ErrReadOnly
	}
	abs, err := l.Resolve(rel)
	if err != nil {
		return Trashed{}, err
	}
	if CleanRel(rel) == "" {
		return Trashed{}, ErrBadName // never the library root
	}
	st, err := os.Stat(abs)
	if err != nil {
		return Trashed{}, err
	}
	if !st.Mode().IsRegular() && !st.IsDir() {
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
	if err := move(abs, filepath.Join(dir, "f"), st.IsDir()); err != nil {
		if st.IsDir() && errors.Is(err, syscall.EXDEV) {
			err = l.trashNearby(abs, clean, dir, err)
		}
		if err != nil {
			os.RemoveAll(dir)
			return Trashed{}, err
		}
	}
	return Trashed{Token: token, Path: clean, Name: filepath.Base(abs), IsDir: st.IsDir()}, nil
}

// move renames a folder (which cannot be copied across file systems here) or
// moves a file with MoveFile.
func move(src, dst string, isDir bool) error {
	if isDir {
		return renameFile(src, dst)
	}
	return MoveFile(src, dst)
}

// trashNearby trashes a folder that sits on another file system than the trash
// (a separate mount inside the library, such as an uploads folder), since folders
// are not copied. It tries "<ancestor>/.trash/<token>" from the top of the
// library down until a rename succeeds, and notes that ancestor in the entry's
// "at" file so Undo and PurgeTrash can find the folder. It returns err when no
// ancestor works.
func (l *Library) trashNearby(abs, clean, dir string, err error) error {
	parts := strings.Split(clean, "/")
	for i := 1; i < len(parts); i++ {
		at := strings.Join(parts[:i], "/")
		near := l.nearTrash(at, filepath.Base(dir))
		if werr := os.WriteFile(filepath.Join(dir, "at"), []byte(at), 0o644); werr != nil {
			return werr
		}
		if merr := os.MkdirAll(near, 0o755); merr != nil {
			return merr
		}
		if err = renameFile(abs, filepath.Join(near, "f")); err == nil {
			return nil
		}
		removeNear(near)
	}
	return err
}

// nearTrash is the trash folder for token inside the library folder at.
func (l *Library) nearTrash(at, token string) string {
	return filepath.Join(l.root, filepath.FromSlash(at), ".trash", token)
}

// removeNear deletes a nearby trash entry, and its .trash folder once empty.
func removeNear(near string) {
	os.RemoveAll(near)
	os.Remove(filepath.Dir(near))
}

// trashed is where the trash entry in dir keeps its file or folder.
func (l *Library) trashed(dir string) string {
	if at, err := os.ReadFile(filepath.Join(dir, "at")); err == nil {
		return filepath.Join(l.nearTrash(string(at), filepath.Base(dir)), "f")
	}
	return filepath.Join(dir, "f")
}

// removeTrash deletes the trash entry in dir, including a folder trashed nearby.
func (l *Library) removeTrash(dir string) error {
	if f := l.trashed(dir); filepath.Dir(f) != dir {
		removeNear(filepath.Dir(f))
	}
	return os.RemoveAll(dir)
}

// Rename gives the folder at rel a new name within the same parent and returns
// its new path.
func (l *Library) Rename(rel, newName string) (string, error) {
	if l.readOnly {
		return "", ErrReadOnly
	}
	if newName == "" || newName == "." || newName == ".." || strings.HasPrefix(newName, ".") ||
		strings.ContainsAny(newName, `/\:*?"<>|`) || strings.TrimSpace(newName) != newName {
		return "", ErrBadName
	}
	clean := CleanRel(rel)
	if clean == "" {
		return "", ErrBadName
	}
	abs, err := l.Resolve(clean)
	if err != nil {
		return "", err
	}
	st, err := os.Lstat(abs)
	if err != nil {
		return "", err
	}
	if !st.IsDir() {
		return "", ErrNotDir
	}
	dest := newName
	if dir := path.Dir(clean); dir != "." {
		dest = relJoin(dir, newName)
	}
	destAbs, err := l.Resolve(dest)
	if err != nil {
		return "", err
	}
	if _, err := os.Lstat(destAbs); err == nil {
		// A case-only rename "finds" itself on case-insensitive disks; allow that.
		if base := filepath.Base(abs); base == newName || !strings.EqualFold(base, newName) {
			return "", ErrExists
		}
	}
	if err := renameFile(abs, destAbs); err != nil {
		return "", err
	}
	return dest, nil
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
	src := l.trashed(dir)
	st, err := os.Lstat(src)
	if err != nil {
		return "", err
	}
	if err := move(src, dest, st.IsDir()); err != nil {
		return "", err
	}
	l.removeTrash(dir)
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
		if l.removeTrash(filepath.Join(l.trashDir, de.Name())) == nil {
			n++
		}
	}
	return n, nil
}

// MoveFile moves a file, falling back to copy-and-remove when a rename cannot
// cross file systems. It never overwrites an existing destination.
func MoveFile(src, dst string) error {
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
