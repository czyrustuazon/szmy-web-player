package meta

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"

	"masterplayer/internal/sniff"
)

// noSeek reads normally but every Seek fails, to exercise the error paths a real file never hits.
type noSeek struct{ r *bytes.Reader }

func (n noSeek) Read(p []byte) (int, error) { return n.r.Read(p) }
func (noSeek) Seek(int64, int) (int64, error) { return 0, errors.New("seek failed") }

func TestID3SkipsFlaggedFramesAndKeepsParsing(t *testing.T) {
	// v2.3: a frame marked compressed (0x80) must be skipped, the next one still read.
	size := make([]byte, 4)
	binary.BigEndian.PutUint32(size, 3)
	compressed := append(append(append([]byte("TIT2"), size...), 0x00, 0x80), 0, 'x', 'y')
	body := append(compressed, frame23("TPE1", txt(0, []byte("After")))...)
	tags, err := Read(writeFile(t, "c.mp3", id3Tag(3, 0, body)), sniff.MP3)
	if err != nil || tags.Title != "c" || tags.Artist != "After" {
		t.Fatalf("v2.3 compressed frame: %+v %v", tags, err)
	}

	// v2.4: a data-length-indicator frame too short to hold the indicator is skipped.
	body = append(frame24("TIT2", 0x01, []byte{1, 2}), frame24("TPE1", 0, txt(0, []byte("Next")))...)
	tags, err = Read(writeFile(t, "d.mp3", id3Tag(4, 0, body)), sniff.MP3)
	if err != nil || tags.Title != "d" || tags.Artist != "Next" {
		t.Fatalf("v2.4 short indicator: %+v %v", tags, err)
	}
}

func TestParsePictureDescriptionLongerThanBlock(t *testing.T) {
	b := append(be32(3), be32(0)...) // picture type, empty MIME
	b = append(b, be32(100)...)      // description claims 100 bytes that are not there
	tags := &Tags{}
	parsePicture(b, tags)
	if tags.Art != nil {
		t.Errorf("truncated picture produced art: %v", tags.Art)
	}
}

func TestFLACSeekFailureWhileSkippingABlock(t *testing.T) {
	data := append([]byte("fLaC"), flacBlock(1, false, make([]byte, 8))...) // padding block is skipped with Seek
	if _, err := readFLAC(noSeek{bytes.NewReader(data)}); err == nil {
		t.Fatal("expected the seek error")
	}
}

func TestFLACShortStreamInfoIsIgnored(t *testing.T) {
	data := append([]byte("fLaC"), flacBlock(0, true, make([]byte, 10))...)
	tags, err := Read(writeFile(t, "short.flac", data), sniff.FLAC)
	if err != nil || tags.SampleRate != 0 {
		t.Fatalf("got %+v %v", tags, err)
	}
}

func TestOggTruncatedSegmentTable(t *testing.T) {
	good := opusFile("TITLE=x")
	// Exactly the 27-byte page header: the segment table the header promises is missing.
	if _, err := Read(writeFile(t, "seg.opus", good[:27]), sniff.Opus); err == nil {
		t.Fatal("expected a truncation error")
	}
}

func TestWAVSeekFailuresStopTheScan(t *testing.T) {
	// A chunk that is not wanted is skipped with Seek; when that fails the scan just ends.
	wav := wavFile(fmtChunk(8000), riffChunk("data", make([]byte, 4)))
	tags, err := readWAV(noSeek{bytes.NewReader(wav)})
	if err != nil || tags.SampleRate != 8000 {
		t.Fatalf("skip chunk: %+v %v", tags, err)
	}

	// An odd-sized wanted chunk is followed by one padding byte, also skipped with Seek.
	odd := wavFile(riffChunk("LIST", []byte("INFOx")))
	if _, err := readWAV(noSeek{bytes.NewReader(odd)}); err != nil {
		t.Fatalf("pad byte: %v", err)
	}
}
