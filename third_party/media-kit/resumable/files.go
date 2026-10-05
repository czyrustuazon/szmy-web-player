package resumable

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// SanitizeFolderName turns a title into a safe single path segment ("upload" if nothing
// usable is left).
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
// does not exist yet.
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

// cleanRel normalises a relative path: slash-separated, no leading slash, no "..".
func cleanRel(rel string) string {
	return strings.TrimPrefix(path.Clean("/"+filepath.ToSlash(rel)), "/")
}

// joinRel joins a relative folder (possibly "") and a name.
func joinRel(dir, name string) string {
	if dir == "" {
		return name
	}
	return dir + "/" + name
}

// SameContent is true when both are regular files with identical bytes.
func SameContent(a, b string) bool {
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

var rename = os.Rename // a seam, so tests can force the copy fallback

// MoveFile renames src to dst, or copies and then removes it when a rename is impossible
// (another file system). dst must not exist.
func MoveFile(src, dst string) error {
	if err := rename(src, dst); err == nil {
		return nil
	}
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
