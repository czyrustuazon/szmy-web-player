// Package unpack verifies and extracts .zip and .7z archives that came from somebody else,
// without trusting them:
//
//   - entries that climb out of the destination ("../x", "/etc/x", "C:\x") are refused (Zip Slip);
//   - links are never created (a link's "content" is only a path, possibly outside the folder);
//   - extraction stops once more than a byte budget has been written (zip and 7z bombs), even
//     when the archive's own headers lie about the sizes.
//
// Zip support is pure Go. 7z (and whatever else the 7z binary reads) goes through an Exec, so
// tests and callers can swap the binary out.
package unpack

import (
	"archive/zip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// ErrTooLarge means an archive unpacks to more than the budget it was given.
var ErrTooLarge = errors.New("archive expands past the size limit")

// Exec runs an external command (7z) and returns its combined output.
type Exec func(name string, args ...string) ([]byte, error)

// DefaultExec runs the command for real.
func DefaultExec(name string, args ...string) ([]byte, error) {
	return exec.Command(name, args...).CombinedOutput()
}

// IsArchive reports whether filename is an archive this package extracts (.zip or .7z).
func IsArchive(filename string) bool {
	switch strings.ToLower(filepath.Ext(filename)) {
	case ".zip", ".7z":
		return true
	}
	return false
}

// IsZip reports whether filename is a .zip, the one format that needs no external tool.
func IsZip(filename string) bool { return strings.EqualFold(filepath.Ext(filename), ".zip") }

// Verify checks an archive's integrity without extracting it. filename decides the format (a
// .zip is read in Go and every entry's CRC32 is checked; anything else is handed to `7z t`);
// path is where the bytes are.
func Verify(run Exec, filename, path string) error {
	if !IsZip(filename) {
		out, err := run("7z", "t", path)
		if err != nil {
			return fmt.Errorf("7z integrity test failed: %w: %s", err, strings.TrimSpace(string(out)))
		}
		return nil
	}
	r, err := openZip(path)
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

// Extract unpacks the archive at path into dest (created as needed), writing at most budget
// bytes. filename decides the format, as for Verify.
func Extract(run Exec, filename, path, dest string, budget int64) error {
	if IsZip(filename) {
		return Zip(path, dest, budget)
	}
	return SevenZip(run, path, dest, budget)
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

// Zip extracts a .zip into dest, refusing entries that escape it (Zip Slip), skipping links,
// and refusing to write more than budget bytes in total (zip bombs).
func Zip(path, dest string, budget int64) error {
	r, err := openZip(path)
	if err != nil {
		return fmt.Errorf("opening zip: %w", err)
	}
	defer r.Close()
	for _, f := range r.File {
		target := filepath.Join(dest, filepath.FromSlash(f.Name))
		if target != dest && !strings.HasPrefix(target, dest+string(filepath.Separator)) {
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
			return ErrTooLarge
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

// SevenZip extracts an archive with the 7z binary. 7z itself has no size limit and recreates
// symbolic links, so the archive is listed first: links, paths that climb out of dest and a
// total unpacked size over budget are refused before anything is written. The size is checked
// again afterwards, in case the listing did not tell the truth.
func SevenZip(run Exec, path, dest string, budget int64) error {
	out, err := run("7z", "l", "-slt", path)
	if err != nil {
		return fmt.Errorf("7z listing failed: %w: %s", err, strings.TrimSpace(string(out)))
	}
	if err := checkListing(out, budget); err != nil {
		return err
	}
	out, err = run("7z", "x", "-y", "-o"+dest, path)
	if err != nil {
		return fmt.Errorf("7z extraction failed: %w: %s", err, strings.TrimSpace(string(out)))
	}
	if DirSize(dest) > budget {
		return ErrTooLarge
	}
	return nil
}

// checkListing reads `7z l -slt` output (one "Key = value" block per entry after a line of
// dashes) and refuses links, escaping paths and archives that unpack to more than budget.
func checkListing(out []byte, budget int64) error {
	started := false
	var total int64
	for _, line := range strings.Split(strings.ReplaceAll(string(out), "\r", ""), "\n") {
		if !started {
			started = line == "----------"
			continue
		}
		k, v, ok := strings.Cut(line, " = ")
		if !ok {
			continue
		}
		switch k {
		case "Path":
			p := strings.ReplaceAll(v, `\`, "/")
			if strings.HasPrefix(p, "/") || (len(p) >= 2 && p[1] == ':') || p == ".." ||
				strings.HasPrefix(p, "../") || strings.HasSuffix(p, "/..") || strings.Contains(p, "/../") {
				return fmt.Errorf("7z entry %q escapes the destination folder", v)
			}
		case "Size":
			if n, err := strconv.ParseInt(v, 10, 64); err == nil {
				if total += n; total > budget {
					return ErrTooLarge
				}
			}
		case "Attributes":
			if isLinkAttributes(v) {
				return fmt.Errorf("7z archive contains a link; links are not allowed")
			}
		case "Symbolic Link", "Hard Link":
			if strings.TrimSpace(v) != "" {
				return fmt.Errorf("7z archive contains a link; links are not allowed")
			}
		}
	}
	if !started {
		return fmt.Errorf("7z listing is unreadable")
	}
	return nil
}

// isLinkAttributes spots a link in 7z's attribute column: Windows' reparse-point flag ("L" in
// the first field, e.g. "AL") or a Unix mode string starting with "l" ("lrwxrwxrwx").
func isLinkAttributes(v string) bool {
	fields := strings.Fields(v)
	if len(fields) > 0 && strings.Contains(fields[0], "L") {
		return true
	}
	for _, f := range fields {
		if len(f) == 10 && f[0] == 'l' && strings.Trim(f[1:], "rwxsStT-") == "" {
			return true
		}
	}
	return false
}

// DirSize sums the sizes of regular files below dir (0 if it does not exist).
func DirSize(dir string) int64 {
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

// Prune removes from dir every file that is not a regular file (links, devices, ...) and every
// regular file keep refuses (keep == nil keeps them all), then every folder left empty. It
// returns what it removed as sorted, slash-separated paths relative to dir. keep sees each
// regular file once, in lexical order.
func Prune(dir string, keep func(path string) bool) []string {
	var skipped, dirs []string
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		switch {
		case err != nil:
		case d.IsDir():
			if p != dir {
				dirs = append(dirs, p)
			}
		case d.Type().IsRegular() && (keep == nil || keep(p)):
		default:
			os.Remove(p)
			rel, _ := filepath.Rel(dir, p)
			skipped = append(skipped, filepath.ToSlash(rel))
		}
		return nil
	})
	for i := len(dirs) - 1; i >= 0; i-- {
		os.Remove(dirs[i]) // only succeeds when empty
	}
	sort.Strings(skipped)
	return skipped
}

// UnwrapLone moves a lone top-level folder's contents up into dir, for as long as dir holds
// exactly one entry and it is a folder ("Album/Disc 1/x.flac" archives are common).
//
// When existing is not empty, unwrapping stops at a folder whose name already exists in
// existing: that folder is real content to merge into, not a wrapper. Without this, an archive
// with a few new files for one album would lose its folder name.
func UnwrapLone(dir, existing string) {
	for depth := 0; depth < 8; depth++ { // bounded: never loops forever on odd layouts
		entries, err := os.ReadDir(dir)
		if err != nil || len(entries) != 1 || !entries[0].IsDir() {
			return
		}
		if existing != "" {
			if st, err := os.Lstat(filepath.Join(existing, entries[0].Name())); err == nil && st.IsDir() {
				return
			}
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
