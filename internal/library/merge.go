package library

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// ErrNested is returned when asked to merge a folder with one inside it or around it.
var ErrNested = errors.New("a folder cannot be merged with a folder inside it or around it")

// MergeResult says what Merge did. Moves maps every file that left the source folder to where its
// content now is (the moved file, or the identical file that was already there), so that
// favorites and play counts can follow.
type MergeResult struct {
	Path       string // the folder everything was merged into
	Moved      int    // files moved into it
	Duplicates int    // files dropped because an identical file was already there
	Skipped    int    // hidden entries and links, left where they were
	Moves      map[string]string
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

// uniqueName returns name, or "name (2)", "name (3)", ... (before the extension for files),
// whichever does not exist in dir yet.
func uniqueName(dir, name string, file bool) string {
	ext := ""
	if file {
		ext = filepath.Ext(name)
	}
	stem := strings.TrimSuffix(name, ext)
	candidate := name
	for i := 2; ; i++ {
		if _, err := os.Lstat(filepath.Join(dir, candidate)); err != nil {
			return candidate
		}
		candidate = fmt.Sprintf("%s (%d)%s", stem, i, ext)
	}
}

// Merge moves everything in folder src into folder dst and removes src. Nothing is overwritten: a
// sub-folder that exists in both is merged too; a file with identical bytes is dropped (dst
// already has it); a file with the same name and other bytes is kept beside it as "name (2).ext".
// Files move one by one, so it also works when the two folders are on different disks.
func (l *Library) Merge(src, dst string) (MergeResult, error) {
	if l.readOnly {
		return MergeResult{}, ErrReadOnly
	}
	src, dst = CleanRel(src), CleanRel(dst)
	if src == "" || dst == "" {
		return MergeResult{}, ErrBadName // never the library root
	}
	if strings.HasPrefix(src+"/", dst+"/") || strings.HasPrefix(dst+"/", src+"/") {
		return MergeResult{}, ErrNested // includes merging a folder with itself
	}
	var abs [2]string
	for i, rel := range []string{src, dst} {
		p, err := l.Resolve(rel)
		if err != nil {
			return MergeResult{}, err
		}
		st, err := os.Lstat(p)
		if err != nil {
			return MergeResult{}, err
		}
		if !st.IsDir() {
			return MergeResult{}, ErrNotDir
		}
		abs[i] = p
	}
	res := MergeResult{Path: dst, Moves: map[string]string{}}
	if err := mergeDir(abs[0], abs[1], src, dst, &res); err != nil {
		return res, err
	}
	os.Remove(abs[0]) // only succeeds when nothing was left behind
	return res, nil
}

func mergeDir(srcAbs, dstAbs, srcRel, dstRel string, res *MergeResult) error {
	entries, err := os.ReadDir(srcAbs)
	if err != nil {
		return err
	}
	for _, e := range entries {
		name := e.Name()
		from, to := filepath.Join(srcAbs, name), filepath.Join(dstAbs, name)
		fromRel := relJoin(srcRel, name)
		existing, statErr := os.Lstat(to)
		switch {
		case strings.HasPrefix(name, "."):
			res.Skipped++
		case e.IsDir():
			target, targetRel := to, relJoin(dstRel, name)
			if statErr == nil && !existing.IsDir() { // a file is in the way: keep both
				name = uniqueName(dstAbs, name, false)
				target, targetRel = filepath.Join(dstAbs, name), relJoin(dstRel, name)
			}
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			if err := mergeDir(from, target, fromRel, targetRel, res); err != nil {
				return err
			}
			os.Remove(from) // empty now, unless something was skipped
		case !e.Type().IsRegular():
			res.Skipped++
		case statErr == nil && SameContent(from, to):
			os.Remove(from)
			res.Duplicates++
			res.Moves[fromRel] = relJoin(dstRel, name)
		default:
			if statErr == nil {
				name = uniqueName(dstAbs, name, true)
			}
			if err := MoveFile(from, filepath.Join(dstAbs, name)); err != nil {
				return err
			}
			res.Moved++
			res.Moves[fromRel] = relJoin(dstRel, name)
		}
	}
	return nil
}
