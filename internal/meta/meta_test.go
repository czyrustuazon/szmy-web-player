package meta

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf16"

	"masterplayer/internal/sniff"
)

// ---------------------------------------------------------------- fixture builders

var jpeg = []byte{0xFF, 0xD8, 0xFF, 0xE0, 'J', 'F', 'I', 'F', 0, 1, 2, 3}

func writeFile(t *testing.T, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func ssBytes(n int) []byte {
	return []byte{byte(n>>21) & 0x7f, byte(n>>14) & 0x7f, byte(n>>7) & 0x7f, byte(n) & 0x7f}
}

func utf16le(s string) []byte {
	out := []byte{0xFF, 0xFE}
	for _, u := range utf16.Encode([]rune(s)) {
		out = append(out, byte(u), byte(u>>8))
	}
	return out
}

func utf16be(s string) []byte {
	var out []byte
	for _, u := range utf16.Encode([]rune(s)) {
		out = append(out, byte(u>>8), byte(u))
	}
	return out
}

func frame23(id string, data []byte) []byte {
	size := make([]byte, 4)
	binary.BigEndian.PutUint32(size, uint32(len(data)))
	return append(append(append([]byte(id), size...), 0, 0), data...)
}

func frame24(id string, flags byte, data []byte) []byte {
	return append(append(append([]byte(id), ssBytes(len(data))...), 0, flags), data...)
}

func frame22(id string, data []byte) []byte {
	return append(append([]byte(id), byte(len(data)>>16), byte(len(data)>>8), byte(len(data))), data...)
}

func id3Tag(ver, flags byte, body []byte) []byte {
	h := append([]byte{'I', 'D', '3', ver, 0, flags}, ssBytes(len(body))...)
	return append(h, body...)
}

func txt(enc byte, payload []byte) []byte { return append([]byte{enc}, payload...) }

func apic(ptype byte, mime string, pic []byte) []byte {
	d := []byte{0}
	d = append(d, mime...)
	d = append(d, 0, ptype, 0) // terminator, picture type, empty description
	return append(d, pic...)
}

func le32(n int) []byte {
	b := make([]byte, 4)
	binary.LittleEndian.PutUint32(b, uint32(n))
	return b
}

func be32(n int) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, uint32(n))
	return b
}

func vorbisComment(comments ...string) []byte {
	out := append(le32(len("test")), "test"...)
	out = append(out, le32(len(comments))...)
	for _, c := range comments {
		out = append(out, le32(len(c))...)
		out = append(out, c...)
	}
	return out
}

func flacBlock(typ byte, last bool, data []byte) []byte {
	h := typ
	if last {
		h |= 0x80
	}
	n := len(data)
	return append([]byte{h, byte(n >> 16), byte(n >> 8), byte(n)}, data...)
}

func streamInfo(rate int, total int64) []byte {
	d := make([]byte, 34)
	d[10] = byte(rate >> 12)
	d[11] = byte(rate >> 4)
	d[12] = byte(rate&0xf)<<4 | 0x02
	d[13] = 0xf0 | byte(total>>32)&0x0f
	d[14], d[15], d[16], d[17] = byte(total>>24), byte(total>>16), byte(total>>8), byte(total)
	return d
}

func flacPicture(ptype int, mime string, pic []byte) []byte {
	d := be32(ptype)
	d = append(d, be32(len(mime))...)
	d = append(d, mime...)
	d = append(d, be32(0)...)         // description length
	d = append(d, make([]byte, 16)...) // width, height, depth, colours
	d = append(d, be32(len(pic))...)
	return append(d, pic...)
}

func oggPages(packet []byte, seq uint32) []byte {
	var segs []byte
	n := len(packet)
	for n >= 255 {
		segs = append(segs, 255)
		n -= 255
	}
	segs = append(segs, byte(n))
	var out []byte
	off := 0
	for len(segs) > 0 {
		chunk := segs
		if len(chunk) > 255 {
			chunk = segs[:255]
		}
		segs = segs[len(chunk):]
		size := 0
		for _, s := range chunk {
			size += int(s)
		}
		h := append([]byte("OggS"), 0, 0)
		h = append(h, make([]byte, 8)...)  // granule
		h = append(h, make([]byte, 4)...)  // serial
		h = append(h, le32(int(seq))...)   // sequence
		h = append(h, make([]byte, 4)...)  // checksum (not verified)
		h = append(h, byte(len(chunk)))
		h = append(h, chunk...)
		out = append(out, h...)
		out = append(out, packet[off:off+size]...)
		off += size
		seq++
	}
	return out
}

func opusFile(comments ...string) []byte {
	head := append([]byte("OpusHead"), 1, 2, 0, 0)
	head = append(head, le32(44100)...)
	head = append(head, 0, 0, 0)
	tags := append([]byte("OpusTags"), vorbisComment(comments...)...)
	return append(oggPages(head, 0), oggPages(tags, 1)...)
}

func vorbisFile(rate int, comments ...string) []byte {
	id := append([]byte("\x01vorbis"), le32(0)...)
	id = append(id, 2)
	id = append(id, le32(rate)...)
	id = append(id, make([]byte, 10)...)
	tags := append([]byte("\x03vorbis"), vorbisComment(comments...)...)
	tags = append(tags, 1)
	return append(oggPages(id, 0), oggPages(tags, 1)...)
}

func riffChunk(id string, data []byte) []byte {
	out := append(append([]byte(id), le32(len(data))...), data...)
	if len(data)%2 == 1 {
		out = append(out, 0)
	}
	return out
}

func wavFile(chunks ...[]byte) []byte {
	body := []byte("WAVE")
	for _, c := range chunks {
		body = append(body, c...)
	}
	return append(append([]byte("RIFF"), le32(len(body))...), body...)
}

func fmtChunk(rate int) []byte {
	d := make([]byte, 16)
	binary.LittleEndian.PutUint16(d[0:], 1)
	binary.LittleEndian.PutUint16(d[2:], 2)
	binary.LittleEndian.PutUint32(d[4:], uint32(rate))
	return riffChunk("fmt ", d)
}

// ---------------------------------------------------------------- ID3

func TestID3v23AllFields(t *testing.T) {
	body := bytes.Join([][]byte{
		frame23("TIT2", txt(1, utf16le("夜に駆ける"))),
		frame23("TPE1", txt(0, []byte("Caf\xe9 Band"))), // Latin-1 é
		frame23("TALB", txt(3, []byte("Album ☃"))),
		frame23("TCON", txt(0, []byte("(17)Rock"))),
		frame23("TYER", txt(0, []byte("2004-05-06T07:08"))),
		frame23("TRCK", txt(0, []byte("3/12"))),
		frame23("APIC", apic(0, "image/jpg", []byte("other"))),
		frame23("APIC", apic(3, "image/jpg", jpeg)),
		make([]byte, 20), // padding
	}, nil)
	p := writeFile(t, "song.mp3", append(id3Tag(3, 0, body), 0xFF, 0xFB))

	tags, err := Read(p, sniff.MP3)
	if err != nil {
		t.Fatal(err)
	}
	if tags.Title != "夜に駆ける" || tags.Artist != "Café Band" || tags.Album != "Album ☃" {
		t.Fatalf("text decoding wrong: %+v", tags)
	}
	if tags.Genre != "Rock" || tags.Year != "2004" || tags.Track != "3/12" {
		t.Fatalf("cleanup wrong: genre=%q year=%q track=%q", tags.Genre, tags.Year, tags.Track)
	}
	if !bytes.Equal(tags.Art, jpeg) || tags.ArtMIME != "image/jpeg" {
		t.Fatalf("front cover should win: %v %q", tags.Art, tags.ArtMIME)
	}
}

func TestID3v24UTF16BEAndFrameFlags(t *testing.T) {
	titleUnsync := append([]byte{3}, "Tit\xff\x00le"...) // unsynchronised 0xFF 0x00
	body := bytes.Join([][]byte{
		frame24("TIT2", 0x02, titleUnsync),
		frame24("TPE1", 0, txt(2, utf16be("Artist"))),
		frame24("TALB", 0x0c, txt(0, []byte("skipped (compressed)"))),
		frame24("TDRC", 0x01, append(be32(4), txt(0, []byte("1999"))...)), // data length indicator
	}, nil)
	p := writeFile(t, "x.mp3", id3Tag(4, 0, body))
	tags, err := Read(p, sniff.MP3)
	if err != nil {
		t.Fatal(err)
	}
	if tags.Title != "Tit\xffle" && tags.Title != "Tit�le" {
		t.Fatalf("title: %q", tags.Title)
	}
	if tags.Artist != "Artist" || tags.Album != "" || tags.Year != "1999" { // compressed TALB frame is skipped
		t.Fatalf("got %+v", tags)
	}
}

func TestID3v22(t *testing.T) {
	body := bytes.Join([][]byte{
		frame22("TT2", txt(0, []byte("Old Title"))),
		frame22("TP1", txt(0, []byte("Old Artist"))),
		frame22("TAL", txt(0, []byte("Old Album"))),
		frame22("PIC", append(append([]byte{0}, "JPG"...), append([]byte{3, 0}, jpeg...)...)),
	}, nil)
	p := writeFile(t, "old.mp3", id3Tag(2, 0, body))
	tags, err := Read(p, sniff.MP3)
	if err != nil {
		t.Fatal(err)
	}
	if tags.Title != "Old Title" || tags.Artist != "Old Artist" || tags.Album != "Old Album" {
		t.Fatalf("got %+v", tags)
	}
	if !bytes.Equal(tags.Art, jpeg) || tags.ArtMIME != "image/jpg" && tags.ArtMIME != "image/jpeg" {
		t.Fatalf("art %v mime %q", tags.Art, tags.ArtMIME)
	}
}

func TestID3GlobalUnsyncAndExtendedHeader(t *testing.T) {
	frames := frame23("TIT2", txt(0, []byte("Plain")))
	ext := append(be32(6), 0, 0, 0, 0, 0, 0) // v2.3 extended header: size 6, flags, padding size
	p := writeFile(t, "e.mp3", id3Tag(3, 0x40, append(ext, frames...)))
	tags, err := Read(p, sniff.MP3)
	if err != nil || tags.Title != "Plain" {
		t.Fatalf("ext header v2.3: %+v %v", tags, err)
	}

	ext4 := append(ssBytes(6), 1, 0) // v2.4: syncsafe size including itself
	p = writeFile(t, "e4.mp3", id3Tag(4, 0x40, append(ext4, frame24("TIT2", 0, txt(0, []byte("Four")))...)))
	if tags, err = Read(p, sniff.MP3); err != nil || tags.Title != "Four" {
		t.Fatalf("ext header v2.4: %+v %v", tags, err)
	}

	p = writeFile(t, "u.mp3", id3Tag(3, 0x80, frames))
	if tags, err = Read(p, sniff.MP3); err != nil || tags.Title != "Plain" {
		t.Fatalf("global unsync: %+v %v", tags, err)
	}
}

func TestID3Errors(t *testing.T) {
	// No tag at all: not an error, title falls back to the file name.
	p := writeFile(t, "my_song_title.mp3", []byte{0xFF, 0xFB, 0x90, 0})
	tags, err := Read(p, sniff.MP3)
	if err != nil || tags.Title != "my song title" {
		t.Fatalf("got %+v %v", tags, err)
	}
	// Empty file.
	if tags, err = Read(writeFile(t, "empty.mp3", nil), sniff.MP3); err != nil || tags.Title != "empty" {
		t.Fatalf("empty: %+v %v", tags, err)
	}
	// Unsupported version.
	if _, err = Read(writeFile(t, "v5.mp3", id3Tag(5, 0, nil)), sniff.MP3); err == nil {
		t.Error("expected unsupported version error")
	}
	// Truncated tag.
	trunc := id3Tag(3, 0, bytes.Repeat([]byte{0}, 50))[:30]
	if _, err = Read(writeFile(t, "t.mp3", trunc), sniff.MP3); err == nil {
		t.Error("expected truncated error")
	}
	// Oversized declared size.
	huge := append([]byte{'I', 'D', '3', 3, 0, 0}, ssBytes(maxTag+1)...)
	if _, err = Read(writeFile(t, "h.mp3", huge), sniff.MP3); err == nil {
		t.Error("expected too large error")
	}
	// Frame claiming more data than exists is ignored without panicking.
	bad := append([]byte("TIT2"), be32(9999)...)
	bad = append(bad, 0, 0, 1)
	if tags, err = Read(writeFile(t, "bad.mp3", id3Tag(3, 0, bad)), sniff.MP3); err != nil || tags.Title != "bad" {
		t.Fatalf("bad frame: %+v %v", tags, err)
	}
	// Missing file.
	if tags, err = Read(filepath.Join(t.TempDir(), "nope.mp3"), sniff.MP3); err == nil || tags == nil {
		t.Error("expected error but usable tags for a missing file")
	}
}

func TestPictureEdgeCases(t *testing.T) {
	cases := [][]byte{
		nil,
		{0},
		{0, 'i', 'm', 'g'},                        // no mime terminator
		{0, 'i', 0},                               // no picture type
		{0, 'i', 0, 3},                            // no description terminator
		{0, 'i', 0, 3, 0},                         // empty picture data
		append([]byte{1, 'i', 0, 3}, 0, 0, 1, 2),  // utf16 description, data follows
	}
	for i, c := range cases {
		tags := &Tags{}
		applyPicture(false, c, tags)
		if i < 6 && tags.Art != nil {
			t.Errorf("case %d: unexpected art %v", i, tags.Art)
		}
		if i == 6 && !bytes.Equal(tags.Art, []byte{1, 2}) {
			t.Errorf("case %d: art %v", i, tags.Art)
		}
	}
	tags := &Tags{}
	applyPicture(true, []byte{0, 'P', 'N'}, tags) // v2.2 with short format field
	if tags.Art != nil {
		t.Error("short v2.2 picture should be ignored")
	}
}

func TestSkipExtHeaderBounds(t *testing.T) {
	if got := skipExtHeader([]byte{1, 2}, 3); len(got) != 2 {
		t.Error("short buffer should be returned unchanged")
	}
	if got := skipExtHeader(append(be32(1000), 1, 2), 3); len(got) != 6 {
		t.Error("oversized extended header should be ignored")
	}
}

func TestTextHelpers(t *testing.T) {
	if got := decodeText(9, []byte("a\xffb")); !strings.Contains(got, "a") {
		t.Errorf("unknown encoding: %q", got)
	}
	if got := decodeUTF16([]byte{0xFE, 0xFF, 0, 'A'}, true); got != "A" {
		t.Errorf("BE BOM: %q", got)
	}
	if got := decodeUTF16([]byte{'A', 0}, true); got != "A" {
		t.Errorf("no BOM defaults to LE: %q", got)
	}
	if got := decodeUTF16(nil, true); got != "" {
		t.Errorf("empty: %q", got)
	}
	if got := textFrame(nil); got != "" {
		t.Errorf("empty frame: %q", got)
	}
	if got := cleanText("abc\x00def"); got != "abc" {
		t.Errorf("NUL split: %q", got)
	}
	if i, n := indexTerm([]byte{'a', 0, 0, 0}, 1); i != 2 || n != 2 {
		t.Errorf("utf16 terminator: %d %d", i, n)
	}
	if i, _ := indexTerm([]byte{'a', 0, 'b'}, 1); i != -1 {
		t.Error("misaligned utf16 terminator must not match")
	}
	if i, n := indexTerm([]byte("ab"), 0); i != -1 || n != 0 {
		t.Error("missing terminator")
	}
}

func TestFinishCleanup(t *testing.T) {
	cases := []struct{ in, want string }{
		{"(17)Rock", "Rock"}, {"(RX)Remix", "Remix"}, {"(17)(18)Mixed", "Mixed"},
		{"(17)", ""}, {"Jazz", "Jazz"}, {"(not a code)Pop", "(not a code)Pop"},
	}
	for _, c := range cases {
		tags := &Tags{Genre: c.in}
		finish(tags, "/x/y.mp3")
		if tags.Genre != c.want {
			t.Errorf("genre %q: got %q want %q", c.in, tags.Genre, c.want)
		}
	}
	for in, want := range map[string]string{"2004-05-06": "2004", "1999": "1999", "99": "99", "": "", "abcd": "abcd", " 2001 ": "2001"} {
		if got := trimYear(in); got != want {
			t.Errorf("year %q: got %q want %q", in, got, want)
		}
	}
	if got := titleFromName("/a/____.mp3"); got != "____.mp3" {
		// all-underscore stem becomes empty after trimming spaces only, so it is kept; guard the fallback path
		if got != "" {
			t.Errorf("unexpected fallback title %q", got)
		}
	}
	if got := titleFromName("/a/.mp3"); got == "" {
		t.Error("dotfile name must still produce a title")
	}
}

// ---------------------------------------------------------------- FLAC

func TestFLACTagsPictureAndLoop(t *testing.T) {
	data := []byte("fLaC")
	data = append(data, flacBlock(0, false, streamInfo(44100, 1000000))...)
	data = append(data, flacBlock(1, false, make([]byte, 10))...) // padding block is skipped
	data = append(data, flacBlock(4, false, vorbisComment(
		"TITLE=Boss Theme", "ARTIST=Composer", "ALBUM=OST", "GENRE=Game", "DATE=2011-01-01",
		"TRACKNUMBER=7", "LOOPSTART=44100", "LOOPLENGTH=500000", "no equals sign"))...)
	data = append(data, flacBlock(6, true, flacPicture(3, "image/jpeg", jpeg))...)
	data = append(data, 0xFF, 0xF8)

	tags, err := Read(writeFile(t, "boss.flac", data), sniff.FLAC)
	if err != nil {
		t.Fatal(err)
	}
	if tags.Title != "Boss Theme" || tags.Artist != "Composer" || tags.Album != "OST" || tags.Genre != "Game" || tags.Year != "2011" || tags.Track != "7" {
		t.Fatalf("tags: %+v", tags)
	}
	if !bytes.Equal(tags.Art, jpeg) || tags.ArtMIME != "image/jpeg" {
		t.Fatalf("art: %v %q", tags.Art, tags.ArtMIME)
	}
	if tags.SampleRate != 44100 {
		t.Fatalf("rate %d", tags.SampleRate)
	}
	want := Loop{Start: 44100, End: 544100, SampleRate: 44100}
	if tags.Loop == nil || *tags.Loop != want {
		t.Fatalf("loop: %+v want %+v", tags.Loop, want)
	}
}

func TestFLACLoopEndVariants(t *testing.T) {
	build := func(c ...string) []byte {
		d := []byte("fLaC")
		d = append(d, flacBlock(0, false, streamInfo(48000, 96000))...)
		return append(d, flacBlock(4, true, vorbisComment(c...))...)
	}
	// LOOPEND wins.
	tags, _ := Read(writeFile(t, "a.flac", build("LOOPSTART=100", "LOOPEND=900", "LOOPLENGTH=5")), sniff.FLAC)
	if tags.Loop == nil || tags.Loop.End != 900 {
		t.Errorf("LOOPEND: %+v", tags.Loop)
	}
	// Only LOOPSTART: end defaults to the stream length.
	tags, _ = Read(writeFile(t, "b.flac", build("loopstart=100")), sniff.FLAC)
	if tags.Loop == nil || tags.Loop.End != 96000 {
		t.Errorf("default end: %+v", tags.Loop)
	}
	// Invalid or empty loop regions are dropped.
	for _, c := range [][]string{{"LOOPSTART=abc"}, {"LOOPSTART=500", "LOOPEND=100"}, {"LOOPSTART=-1", "LOOPEND=9"}, {"LOOPEND=9"}} {
		tags, _ = Read(writeFile(t, "c.flac", build(c...)), sniff.FLAC)
		if tags.Loop != nil {
			t.Errorf("%v should not produce a loop: %+v", c, tags.Loop)
		}
	}
}

func TestFLACErrors(t *testing.T) {
	if _, err := Read(writeFile(t, "no.flac", []byte("nope")), sniff.FLAC); err == nil {
		t.Error("bad magic")
	}
	if _, err := Read(writeFile(t, "short.flac", []byte("fLaC")), sniff.FLAC); err == nil {
		t.Error("truncated metadata header")
	}
	if _, err := Read(writeFile(t, "blk.flac", append([]byte("fLaC"), flacBlock(0, true, make([]byte, 34))[:10]...)), sniff.FLAC); err == nil {
		t.Error("truncated block")
	}
	badComment := append([]byte("fLaC"), flacBlock(4, true, []byte{1, 2})...)
	if _, err := Read(writeFile(t, "bc.flac", badComment), sniff.FLAC); err == nil {
		t.Error("bad vorbis comment block")
	}
	// A stream with an oversized skip block and then EOF reports truncation.
	skip := append([]byte("fLaC"), flacBlock(3, false, make([]byte, 4))...)
	if _, err := Read(writeFile(t, "sk.flac", skip), sniff.FLAC); err == nil {
		t.Error("EOF after skipped block")
	}
}

func TestVorbisCommentErrors(t *testing.T) {
	cases := map[string][]byte{
		"short":         {1},
		"vendor":        append(le32(50), 'x'),
		"count":         append(le32(0)),
		"entry length":  append(append(le32(0), le32(1)...), 1, 2),
		"entry payload": append(append(append(le32(0), le32(1)...), le32(20)...), 'a'),
	}
	for name, c := range cases {
		if err := parseVorbisComment(c, &Tags{}); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestParsePictureTruncations(t *testing.T) {
	full := flacPicture(3, "image/png", []byte{1, 2, 3})
	for n := 0; n < len(full); n++ {
		tags := &Tags{}
		parsePicture(full[:n], tags) // must never panic
		if tags.Art != nil {
			t.Errorf("truncated at %d produced art", n)
		}
	}
	tags := &Tags{}
	parsePicture(full, tags)
	if !bytes.Equal(tags.Art, []byte{1, 2, 3}) {
		t.Errorf("full picture: %v", tags.Art)
	}
	// A non-front cover is kept only until a front cover arrives.
	tags.setArt(0, "image/png", []byte{9})
	if !bytes.Equal(tags.Art, []byte{1, 2, 3}) {
		t.Error("later non-front picture must not replace the first")
	}
	other := &Tags{}
	other.setArt(0, "image/png", []byte{9})
	other.setArt(3, "IMAGE/JPEG", []byte{8})
	if !bytes.Equal(other.Art, []byte{8}) || other.ArtMIME != "image/jpeg" {
		t.Errorf("front cover should replace: %v %q", other.Art, other.ArtMIME)
	}
	other.setArt(3, "image/png", nil)
	if !bytes.Equal(other.Art, []byte{8}) {
		t.Error("empty picture is ignored")
	}
}

// ---------------------------------------------------------------- Ogg

func TestOpusTagsArtAndLoop(t *testing.T) {
	pic := base64.StdEncoding.EncodeToString(flacPicture(3, "image/jpeg", jpeg))
	p := writeFile(t, "a.opus", opusFile("TITLE=Opus Song", "ARTIST=Ann", "LOOPSTART=48000", "LOOPEND=480000", "METADATA_BLOCK_PICTURE="+pic))
	tags, err := Read(p, sniff.Opus)
	if err != nil {
		t.Fatal(err)
	}
	if tags.Title != "Opus Song" || tags.Artist != "Ann" || tags.SampleRate != 48000 {
		t.Fatalf("tags: %+v", tags)
	}
	if tags.Loop == nil || *tags.Loop != (Loop{Start: 48000, End: 480000, SampleRate: 48000}) {
		t.Fatalf("loop: %+v", tags.Loop)
	}
	if !bytes.Equal(tags.Art, jpeg) {
		t.Fatalf("art: %v", tags.Art)
	}
}

func TestOggCommentSpanningManyPages(t *testing.T) {
	long := "COMMENT=" + strings.Repeat("x", 80000) // forces a packet across several Ogg pages
	p := writeFile(t, "long.opus", opusFile(long, "TITLE=After The Long Comment"))
	tags, err := Read(p, sniff.Opus)
	if err != nil || tags.Title != "After The Long Comment" {
		t.Fatalf("got %+v %v", tags, err)
	}
}

func TestVorbisOgg(t *testing.T) {
	p := writeFile(t, "v.ogg", vorbisFile(32000, "TITLE=Vorbis Song", "LOOPSTART=1000", "LOOPLENGTH=2000"))
	tags, err := Read(p, sniff.Vorbis)
	if err != nil {
		t.Fatal(err)
	}
	if tags.Title != "Vorbis Song" || tags.SampleRate != 32000 {
		t.Fatalf("tags: %+v", tags)
	}
	if tags.Loop == nil || *tags.Loop != (Loop{Start: 1000, End: 3000, SampleRate: 32000}) {
		t.Fatalf("loop: %+v", tags.Loop)
	}
}

func TestOggErrors(t *testing.T) {
	if _, err := Read(writeFile(t, "e.ogg", nil), sniff.Vorbis); err == nil {
		t.Error("empty ogg")
	}
	if _, err := Read(writeFile(t, "bad.ogg", bytes.Repeat([]byte{'x'}, 100)), sniff.Vorbis); err == nil {
		t.Error("bad page magic")
	}
	good := opusFile("TITLE=x")
	if _, err := Read(writeFile(t, "t1.opus", good[:20]), sniff.Opus); err == nil {
		t.Error("truncated header")
	}
	if _, err := Read(writeFile(t, "t2.opus", good[:28]), sniff.Opus); err == nil {
		t.Error("truncated segment table")
	}
	if _, err := Read(writeFile(t, "t3.opus", good[:40]), sniff.Opus); err == nil {
		t.Error("truncated packet body")
	}
	if _, err := Read(writeFile(t, "wrong.opus", vorbisFile(44100, "TITLE=x")), sniff.Opus); err == nil {
		t.Error("vorbis stream read as opus")
	}
	if _, err := Read(writeFile(t, "wrong.ogg", opusFile("TITLE=x")), sniff.Vorbis); err == nil {
		t.Error("opus stream read as vorbis")
	}
}

// ---------------------------------------------------------------- WAV

func TestWAVInfoAndSmplLoop(t *testing.T) {
	info := []byte("INFO")
	info = append(info, riffChunk("INAM", []byte("Wave Title\x00"))...)
	info = append(info, riffChunk("IART", []byte("Wave Artist"))...)
	info = append(info, riffChunk("IPRD", []byte("Wave Album"))...)
	info = append(info, riffChunk("IGNR", []byte("(17)Rock"))...)
	info = append(info, riffChunk("ICRD", []byte("2020-02-02"))...)
	info = append(info, riffChunk("ITRK", []byte("5"))...)

	smpl := make([]byte, 36+24)
	binary.LittleEndian.PutUint32(smpl[28:], 1)
	binary.LittleEndian.PutUint32(smpl[44:], 1000)
	binary.LittleEndian.PutUint32(smpl[48:], 4999) // inclusive end

	p := writeFile(t, "w.wav", wavFile(fmtChunk(22050), riffChunk("data", make([]byte, 101)), riffChunk("LIST", info), riffChunk("smpl", smpl)))
	tags, err := Read(p, sniff.WAV)
	if err != nil {
		t.Fatal(err)
	}
	if tags.Title != "Wave Title" || tags.Artist != "Wave Artist" || tags.Album != "Wave Album" || tags.Genre != "Rock" || tags.Year != "2020" || tags.Track != "5" {
		t.Fatalf("tags: %+v", tags)
	}
	if tags.Loop == nil || *tags.Loop != (Loop{Start: 1000, End: 5000, SampleRate: 22050}) {
		t.Fatalf("loop: %+v", tags.Loop)
	}
	if tags.SampleRate != 22050 {
		t.Fatalf("rate %d", tags.SampleRate)
	}
}

func TestWAVSmplBeforeFmtGetsRate(t *testing.T) {
	smpl := make([]byte, 60)
	binary.LittleEndian.PutUint32(smpl[28:], 1)
	binary.LittleEndian.PutUint32(smpl[44:], 10)
	binary.LittleEndian.PutUint32(smpl[48:], 99)
	p := writeFile(t, "w.wav", wavFile(riffChunk("smpl", smpl), fmtChunk(8000)))
	tags, err := Read(p, sniff.WAV)
	if err != nil || tags.Loop == nil || tags.Loop.SampleRate != 8000 {
		t.Fatalf("got %+v %v", tags, err)
	}
}

func TestWAVEdgeCases(t *testing.T) {
	if _, err := Read(writeFile(t, "no.wav", []byte("RIFFxxxxWAVX")), sniff.WAV); err == nil {
		t.Error("bad form type")
	}
	if _, err := Read(writeFile(t, "tiny.wav", []byte("RIFF")), sniff.WAV); err == nil {
		t.Error("short header")
	}
	// Truncated wanted chunk.
	trunc := wavFile(fmtChunk(8000))
	if _, err := Read(writeFile(t, "tr.wav", trunc[:len(trunc)-4]), sniff.WAV); err == nil {
		t.Error("truncated fmt chunk")
	}
	// Short fmt, short smpl with no loops, non-INFO LIST and malformed INFO are ignored.
	bad := wavFile(
		riffChunk("fmt ", []byte{1, 2}),
		riffChunk("smpl", make([]byte, 60)),
		riffChunk("LIST", []byte("adtl")),
		riffChunk("LIST", append([]byte("INFO"), 'I', 'N', 'A', 'M', 200, 0, 0, 0, 'x')),
		riffChunk("junk", []byte{1, 2, 3}), // odd size exercises padding
	)
	tags, err := Read(writeFile(t, "bad.wav", bad), sniff.WAV)
	if err != nil || tags.Loop != nil || tags.Title != "bad" {
		t.Fatalf("got %+v %v", tags, err)
	}
	// An empty loop (end before start) is dropped.
	smpl := make([]byte, 60)
	binary.LittleEndian.PutUint32(smpl[28:], 1)
	binary.LittleEndian.PutUint32(smpl[44:], 500)
	binary.LittleEndian.PutUint32(smpl[48:], 10)
	tags, _ = Read(writeFile(t, "l.wav", wavFile(fmtChunk(8000), riffChunk("smpl", smpl))), sniff.WAV)
	if tags.Loop != nil {
		t.Errorf("inverted loop kept: %+v", tags.Loop)
	}
}

func TestOtherKindsOnlyGetFilenameTitle(t *testing.T) {
	for _, k := range []sniff.Kind{sniff.M4A, sniff.VGM, sniff.Unknown} {
		tags, err := Read(writeFile(t, "game_theme.brstm", []byte("RSTM")), k)
		if err != nil || tags.Title != "game theme" || tags.Loop != nil {
			t.Errorf("%v: %+v %v", k, tags, err)
		}
	}
}
