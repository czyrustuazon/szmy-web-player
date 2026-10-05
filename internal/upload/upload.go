// Package upload is the music library's side of chunked, resumable uploads. The protocol itself
// (sessions, CRC32-checked chunks, resume, corrupt-chunk recovery, safe zip/7z extraction,
// merging without overwriting) lives in media-kit's resumable and unpack packages; this package
// decides what a music library accepts:
//
//   - a loose file must be audio;
//   - an archive is reduced to its audio files plus up to maxImagesPerFolder real pictures per
//     folder (the best cover candidates), and must contain at least one audio file;
//   - what was added is counted as tracks and images.
//
// Uploads may only land inside the configured upload folder of the library.
package upload

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"

	"github.com/czyrustuazon/lib-szmy-media-kit/resumable"
	"github.com/czyrustuazon/lib-szmy-media-kit/unpack"

	"masterplayer/internal/library"
	"masterplayer/internal/sniff"
)

// MaxChunkBytes bounds a single chunk regardless of what the client sends.
var MaxChunkBytes = resumable.MaxChunkBytes

// The protocol's errors and states, re-exported for the HTTP layer.
var (
	ErrBadPath     = resumable.ErrBadPath
	ErrNoDest      = resumable.ErrNoDest
	ErrBadName     = resumable.ErrBadName
	ErrBadSize     = resumable.ErrBadSize
	ErrTooLarge    = resumable.ErrTooLarge
	ErrNoSpace     = resumable.ErrNoSpace
	ErrNoSession   = resumable.ErrNoSession
	ErrOverrun     = resumable.ErrOverrun
	ErrIncomplete  = resumable.ErrIncomplete
	ErrUnsupported = resumable.ErrUnsupported
	ErrNoReport    = resumable.ErrNoReport
)

type (
	OffsetMismatchError   = resumable.OffsetMismatchError
	ChecksumMismatchError = resumable.ChecksumMismatchError
	State                 = resumable.State
)

const (
	Running   = resumable.Running
	Done      = resumable.Done
	Failed    = resumable.Failed
	Corrupted = resumable.Corrupted
)

// Status is resumable.Status as the player's browser code reads it: what was added is split
// into tracks and images.
type Status struct {
	State        State  `json:"state"`
	BytesWritten int64  `json:"bytesWritten"`
	TotalBytes   int64  `json:"totalBytes"`
	Error        string `json:"error,omitempty"`
	ResumeOffset int64  `json:"resumeOffset,omitempty"`
	Tracks       int    `json:"tracks,omitempty"`     // audio files added
	Images       int    `json:"images,omitempty"`     // cover images kept (they become cover art)
	Duplicates   int    `json:"duplicates,omitempty"` // files that were already in the folder, identical, and so not added again
	Skipped      int    `json:"skipped,omitempty"`    // other files dropped from an archive
	Path         string `json:"path,omitempty"`       // library path of the file or folder that was added

	// What was dropped, by file type ("jpg": 927, "txt": 66, ...), and whether the full list
	// of dropped files can be fetched with Report.
	SkippedTypes map[string]int `json:"skippedTypes,omitempty"`
	HasReport    bool           `json:"hasReport,omitempty"`
}

const (
	kindAudio = "audio"
	kindImage = "image"
)

func fromResumable(s resumable.Status) Status {
	return Status{
		State: s.State, BytesWritten: s.BytesWritten, TotalBytes: s.TotalBytes, Error: s.Error, ResumeOffset: s.ResumeOffset,
		Tracks: s.Kinds[kindAudio], Images: s.Kinds[kindImage], Duplicates: s.Duplicates, Skipped: s.Skipped, Path: s.Path,
		SkippedTypes: s.SkippedTypes, HasReport: s.HasReport,
	}
}

// Manager owns the upload sessions of one library.
type Manager struct{ *resumable.Manager }

// New creates a Manager. root must be the library's real root path; uploadRel
// is the upload folder inside it (for example "uploads"); maxBytes caps a
// file's declared size (and an archive's extracted size); minFree is the free
// space that must remain after an upload (0 disables the check).
func New(root, uploadRel string, maxBytes, minFree int64) *Manager {
	return &Manager{resumable.New(options(root, uploadRel, maxBytes, minFree))}
}

func options(root, uploadRel string, maxBytes, minFree int64) resumable.Options {
	return resumable.Options{
		Root: root, Dir: uploadRel, MaxBytes: maxBytes, MinFree: minFree,
		Accept:       acceptAudio,
		Prune:        prune,
		Classify:     classify,
		FallbackName: "track",
		ReportNote:   fmt.Sprintf("Only audio files and up to %d cover pictures per folder are kept.", maxImagesPerFolder),
	}
}

// Complete finalises one file's upload; see resumable.Manager.Complete.
func (m *Manager) Complete(relPath, filename string, size int64) (Status, error) {
	st, err := m.Manager.Complete(relPath, filename, size)
	return fromResumable(st), err
}

// StatusOf reports a tracked completion; see resumable.Manager.StatusOf.
func (m *Manager) StatusOf(relPath, filename string) (Status, bool) {
	st, found := m.Manager.StatusOf(relPath, filename)
	return fromResumable(st), found
}

// isAudio judges a file like the rest of the app: by its first bytes and its name.
func isAudio(path, name string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	buf := make([]byte, 512)
	n, _ := io.ReadFull(f, buf)
	return sniff.Detect(buf[:n], name) != sniff.Unknown
}

var errNotAudio = errors.New("not an audio file")

// acceptAudio lets a loose file into the library only if it is audio. The staged bytes have no
// extension of their own, so the name the file was sent as is what is judged.
func acceptAudio(path, filename string) error {
	if !isAudio(path, filename) {
		return errNotAudio
	}
	return nil
}

func classify(path string) string {
	if isAudio(path, filepath.Base(path)) {
		return kindAudio
	}
	return kindImage // prune leaves nothing but audio and pictures
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

var errNoAudio = errors.New("contains no audio files")

// prune reduces an unpacked archive to what a music library wants: regular audio files, plus up
// to maxImagesPerFolder real pictures per folder (the best cover candidates, see
// library.RankImages) to serve as cover art. Links, scripts, text files, thumbnails and
// everything else are removed, then empty folders. An archive without audio is refused.
func prune(dir string) ([]string, error) {
	tracks := 0
	pics := map[string][]string{} // folder -> names of the pictures in it
	skipped := unpack.Prune(dir, func(p string) bool {
		switch {
		case isAudio(p, filepath.Base(p)):
			tracks++
		case isImage(p):
			pics[filepath.Dir(p)] = append(pics[filepath.Dir(p)], filepath.Base(p))
		default:
			return false
		}
		return true
	})
	for folder, names := range pics {
		for i, name := range library.RankImages(names) {
			if i < maxImagesPerFolder {
				continue
			}
			os.Remove(filepath.Join(folder, name))
			rel, _ := filepath.Rel(dir, filepath.Join(folder, name))
			skipped = append(skipped, filepath.ToSlash(rel))
		}
	}
	if tracks == 0 {
		return nil, errNoAudio
	}
	sort.Strings(skipped)
	return skipped, nil
}
