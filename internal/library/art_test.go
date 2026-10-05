package library

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIsImageName(t *testing.T) {
	for name, want := range map[string]bool{
		"a.jpg": true, "A.JPEG": true, "b.png": true, "c.webp": true, "d.gif": true,
		"e.txt": false, "f.mp3": false, "jpg": false, "g.svg": false, "h.bmp": false,
	} {
		if IsImageName(name) != want {
			t.Errorf("%s: want %v", name, want)
		}
	}
}

func TestRankImagesPutsCoversFirst(t *testing.T) {
	got := RankImages([]string{"notes.txt", "03.jpg", "front-scan.png", "Cover.JPG", "a.gif", "folder.png", "B.webp", "song.mp3", "Album.jpg"})
	want := "Cover.JPG folder.png Album.jpg front-scan.png 03.jpg a.gif B.webp"
	if strings.Join(got, " ") != want {
		t.Errorf("got  %v\nwant %s", got, want)
	}
	if len(RankImages([]string{"a.txt"})) != 0 || len(RankImages(nil)) != 0 {
		t.Error("no pictures, no result")
	}
}

func TestFolderArtFindsTheBestImageNearTheTrack(t *testing.T) {
	l, root := newLib(t)
	w := func(rel string) { mustWrite(t, filepath.Join(root, filepath.FromSlash(rel)), []byte("img")) }
	abs := func(rel string) string { return filepath.Join(l.root, filepath.FromSlash(rel)) }

	// A cover-named picture beats other pictures.
	w("A/t.mp3")
	w("A/zz.jpg")
	w("A/Folder.PNG")
	if p, ok := l.FolderArt("A/t.mp3"); !ok || p != abs("A/Folder.PNG") {
		t.Errorf("cover name: %q %v", p, ok)
	}
	// No special name: the first picture by name.
	w("B/t.mp3")
	w("B/02.jpg")
	w("B/01.jpg")
	if p, ok := l.FolderArt("B/t.mp3"); !ok || p != abs("B/01.jpg") {
		t.Errorf("first picture: %q %v", p, ok)
	}
	// An album's Scans folder.
	w("C/t.mp3")
	w("C/Scans/02.jpg")
	w("C/Scans/01.jpg")
	w("C/Booklet/ignored.jpg") // an art folder that comes later alphabetically... Booklet sorts first, so it wins
	if p, ok := l.FolderArt("C/t.mp3"); !ok || p != abs("C/Booklet/ignored.jpg") {
		t.Errorf("art folders are searched in name order: %q %v", p, ok)
	}
	os.RemoveAll(abs("C/Booklet"))
	if p, ok := l.FolderArt("C/t.mp3"); !ok || p != abs("C/Scans/01.jpg") {
		t.Errorf("scans folder: %q %v", p, ok)
	}
	// The picture next to the track wins over one in a Scans folder.
	w("C/front.jpg")
	if p, _ := l.FolderArt("C/t.mp3"); p != abs("C/front.jpg") {
		t.Errorf("nearest first: %q", p)
	}
	// A multi-disc album: the cover sits above the disc folders (and so may its Scans folder).
	w("D/CD1/t.mp3")
	w("D/cover.png")
	if p, ok := l.FolderArt("D/CD1/t.mp3"); !ok || p != abs("D/cover.png") {
		t.Errorf("album folder above the disc: %q %v", p, ok)
	}
	w("E/CD2/t.mp3")
	w("E/Artwork/front.jpg")
	if p, ok := l.FolderArt("E/CD2/t.mp3"); !ok || p != abs("E/Artwork/front.jpg") {
		t.Errorf("artwork folder above the disc: %q %v", p, ok)
	}
	// A track in the library root uses pictures in the root; a top-level folder does not inherit them.
	w("root-track.mp3")
	w("cover.jpg")
	if p, ok := l.FolderArt("root-track.mp3"); !ok || p != abs("cover.jpg") {
		t.Errorf("track in the root: %q %v", p, ok)
	}
	w("solo/t.mp3")
	if _, ok := l.FolderArt("solo/t.mp3"); ok {
		t.Error("a root-level picture must not become the cover of every top-level folder")
	}
}

func TestFolderArtIgnoresWhatItShould(t *testing.T) {
	l, root := newLib(t)
	w := func(rel string, data []byte) { mustWrite(t, filepath.Join(root, filepath.FromSlash(rel)), data) }

	w("none/t.mp3", []byte("x"))
	w("none/readme.txt", []byte("x"))
	w("none/.cover.jpg", []byte("hidden"))
	w("none/sub/deep/cover.jpg", []byte("two levels down is too far"))
	if _, ok := l.FolderArt("none/t.mp3"); ok {
		t.Error("no usable picture here")
	}

	// A huge picture is skipped in favour of the next one.
	w("big/t.mp3", []byte("x"))
	w("big/cover.jpg", []byte("small"))
	if err := os.Truncate(filepath.Join(root, "big", "cover.jpg"), maxArtBytes+1); err != nil {
		t.Skip("cannot make a large sparse file here:", err)
	}
	w("big/other.jpg", []byte("fine"))
	if p, ok := l.FolderArt("big/t.mp3"); !ok || filepath.Base(p) != "other.jpg" {
		t.Errorf("oversized picture: %q %v", p, ok)
	}

	// Links are never followed.
	w("link/t.mp3", []byte("x"))
	if err := os.Symlink("/etc/passwd", filepath.Join(root, "link", "cover.jpg")); err == nil {
		if _, ok := l.FolderArt("link/t.mp3"); ok {
			t.Error("a symlink must not be used as cover art")
		}
	}
}

func TestFolderArtRefusesHiddenPathsAndMissingFolders(t *testing.T) {
	l, _ := newLib(t)
	if _, ok := l.FolderArt(".trash/x.mp3"); ok {
		t.Error("hidden path")
	}
	if _, ok := l.FolderArt("no/such/folder/t.mp3"); ok {
		t.Error("missing folder")
	}
}

func TestBrowseHidesPicturesButKeepsEverythingElse(t *testing.T) {
	l, root := newLib(t)
	mustWrite(t, filepath.Join(root, "Album B", "cover.jpg"), []byte("img"))
	mustWrite(t, filepath.Join(root, "Album B", "notes.txt"), []byte("hi"))
	es, err := l.Browse("Album B")
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(names(es), ",")
	if strings.Contains(got, "cover.jpg") || !strings.Contains(got, "notes.txt") || !strings.Contains(got, "02 two.mp3") {
		t.Errorf("listing: %s", got)
	}
	// A folder that merely looks like a picture name is still a folder.
	mustWrite(t, filepath.Join(root, "pics.png", "x.mp3"), mp3)
	es, _ = l.Browse("")
	if !contains(names(es), "pics.png") {
		t.Error("folders are never hidden")
	}
}
