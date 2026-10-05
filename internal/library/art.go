package library

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// maxArtBytes is the largest folder image that is used as cover art.
const maxArtBytes = 16 << 20

var imageExts = map[string]bool{".jpg": true, ".jpeg": true, ".png": true, ".webp": true, ".gif": true}

// IsImageName reports whether name looks like a picture the browser can show.
func IsImageName(name string) bool { return imageExts[strings.ToLower(filepath.Ext(name))] }

// Names that mean "this is the cover", best first.
var coverNames = []string{"cover", "folder", "front", "album", "albumart", "art", "artwork", "thumb"}

// Sub-folders that usually hold an album's artwork or scans.
var artFolders = map[string]bool{"scans": true, "scan": true, "artwork": true, "covers": true, "cover": true, "art": true, "images": true, "booklet": true}

func imageRank(name string) int {
	base := strings.ToLower(strings.TrimSuffix(name, filepath.Ext(name)))
	for i, c := range coverNames {
		if base == c {
			return i
		}
	}
	if strings.Contains(base, "cover") || strings.Contains(base, "front") {
		return len(coverNames)
	}
	return len(coverNames) + 1
}

// RankImages returns the picture file names among names, best cover candidate first: files
// called cover/folder/front/album and so on, then anything mentioning cover or front, then the
// rest in name order.
func RankImages(names []string) []string {
	var out []string
	for _, n := range names {
		if IsImageName(n) {
			out = append(out, n)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		ri, rj := imageRank(out[i]), imageRank(out[j])
		if ri != rj {
			return ri < rj
		}
		return strings.ToLower(out[i]) < strings.ToLower(out[j])
	})
	return out
}

// imageIn returns the best cover candidate that is a regular file (not a link) of reasonable
// size directly inside dir.
func imageIn(dir string) (string, bool) {
	des, _ := os.ReadDir(dir) // an unreadable folder simply has no art
	byName := map[string]os.DirEntry{}
	var names []string
	for _, de := range des {
		if strings.HasPrefix(de.Name(), ".") || !de.Type().IsRegular() {
			continue
		}
		byName[de.Name()] = de
		names = append(names, de.Name())
	}
	for _, n := range RankImages(names) {
		if info, err := byName[n].Info(); err == nil && info.Size() <= maxArtBytes {
			return filepath.Join(dir, n), true
		}
	}
	return "", false
}

// imageInArtFolders looks inside dir's Scans, Artwork, Covers... sub-folders.
func imageInArtFolders(dir string) (string, bool) {
	des, _ := os.ReadDir(dir)
	for _, de := range des {
		if de.IsDir() && artFolders[strings.ToLower(de.Name())] {
			if p, ok := imageIn(filepath.Join(dir, de.Name())); ok {
				return p, true
			}
		}
	}
	return "", false
}

// FolderArt finds cover art for a track that has none embedded: a cover/folder/front image in
// its folder, then in the folder's Scans/Artwork sub-folders, then (for multi-disc albums such
// as Album/CD1/track.mp3) the same in the folder above. It returns the image's absolute path.
func (l *Library) FolderArt(rel string) (string, bool) {
	abs, err := l.Resolve(rel)
	if err != nil {
		return "", false
	}
	dir := filepath.Dir(abs)
	if p, ok := imageIn(dir); ok {
		return p, true
	}
	if p, ok := imageInArtFolders(dir); ok {
		return p, true
	}
	if parent := filepath.Dir(dir); dir != l.root && parent != l.root {
		if p, ok := imageIn(parent); ok {
			return p, true
		}
		return imageInArtFolders(parent)
	}
	return "", false
}
