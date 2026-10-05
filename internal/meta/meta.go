// Package meta reads tags, cover art and loop points from audio files.
//
// Supported: ID3v2.2/2.3/2.4 (MP3), FLAC, Ogg Opus/Vorbis comments and WAV
// (LIST/INFO plus smpl loop chunk). Loop points come from LOOPSTART/LOOPEND/
// LOOPLENGTH Vorbis comments or a WAV smpl chunk.
package meta

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf16"

	"masterplayer/internal/sniff"
)

const maxTag = 16 << 20 // upper bound on anything read into memory

// Loop is a loop region in samples at SampleRate.
type Loop struct {
	Start      int64 `json:"start"`
	End        int64 `json:"end"`
	SampleRate int   `json:"sampleRate"`
}

// Tags is everything we know about a file's metadata.
type Tags struct {
	Title, Artist, Album, Genre, Year, Track string
	Art                                      []byte
	ArtMIME                                  string
	Loop                                     *Loop
	SampleRate                               int

	artType int
	rate    int
	total   int64
}

// Read parses metadata for the given kind. It always returns usable Tags
// (with a filename-derived title); a non-nil error means the tags were
// unreadable or partial and is meant for the error log.
func Read(path string, kind sniff.Kind) (*Tags, error) {
	f, err := os.Open(path)
	if err != nil {
		t := &Tags{}
		finish(t, path)
		return t, err
	}
	defer f.Close()

	var t *Tags
	switch kind {
	case sniff.MP3:
		t, err = readID3(f)
	case sniff.FLAC:
		t, err = readFLAC(f)
	case sniff.Opus:
		t, err = readOgg(f, true)
	case sniff.Vorbis:
		t, err = readOgg(f, false)
	case sniff.WAV:
		t, err = readWAV(f)
	default:
		t = &Tags{}
	}
	if t == nil {
		t = &Tags{}
	}
	finish(t, path)
	return t, err
}

var genrePrefix = regexp.MustCompile(`^\(\s*(?:\d+|RX|CR)\s*\)\s*`)

func finish(t *Tags, path string) {
	if t.Title == "" {
		t.Title = titleFromName(path)
	}
	for genrePrefix.MatchString(t.Genre) {
		t.Genre = genrePrefix.ReplaceAllString(t.Genre, "")
	}
	t.Genre = strings.TrimSpace(t.Genre)
	t.Year = trimYear(t.Year)
}

func titleFromName(path string) string {
	base := filepath.Base(path)
	name := strings.TrimSuffix(base, filepath.Ext(base))
	name = strings.TrimSpace(strings.ReplaceAll(name, "_", " "))
	if name == "" {
		return base
	}
	return name
}

// trimYear crops full timestamps (2004-05-06T...) to the 4-digit year.
func trimYear(y string) string {
	y = strings.TrimSpace(y)
	if len(y) >= 4 && strings.Trim(y[:4], "0123456789") == "" {
		return y[:4]
	}
	return y
}

func (t *Tags) setArt(ptype int, mime string, data []byte) {
	if len(data) == 0 {
		return
	}
	if t.Art == nil || (ptype == 3 && t.artType != 3) {
		t.Art = append([]byte(nil), data...)
		t.ArtMIME = strings.ToLower(mime)
		t.artType = ptype
	}
}

func set(dst *string, v string) {
	if *dst == "" {
		*dst = v
	}
}

// ---------------------------------------------------------------- ID3v2

func syncsafe(b []byte) uint32 {
	return uint32(b[0]&0x7f)<<21 | uint32(b[1]&0x7f)<<14 | uint32(b[2]&0x7f)<<7 | uint32(b[3]&0x7f)
}

func unsync(b []byte) []byte {
	out := make([]byte, 0, len(b))
	for i := 0; i < len(b); i++ {
		out = append(out, b[i])
		if b[i] == 0xFF && i+1 < len(b) && b[i+1] == 0 {
			i++
		}
	}
	return out
}

func readID3(f io.Reader) (*Tags, error) {
	t := &Tags{}
	hdr := make([]byte, 10)
	if _, err := io.ReadFull(f, hdr); err != nil || string(hdr[:3]) != "ID3" {
		return t, nil // no tag is not an error
	}
	ver, flags := hdr[3], hdr[5]
	if ver < 2 || ver > 4 {
		return t, fmt.Errorf("unsupported ID3v2.%d", ver)
	}
	size := int(syncsafe(hdr[6:10]))
	if size > maxTag {
		return t, errors.New("ID3 tag too large")
	}
	body := make([]byte, size)
	if _, err := io.ReadFull(f, body); err != nil {
		return t, fmt.Errorf("truncated ID3 tag: %w", err)
	}
	if flags&0x80 != 0 {
		body = unsync(body)
	}
	if flags&0x40 != 0 && ver >= 3 {
		body = skipExtHeader(body, ver)
	}
	parseFrames(body, ver, t)
	return t, nil
}

func skipExtHeader(b []byte, ver byte) []byte {
	if len(b) < 4 {
		return b
	}
	var n int
	if ver == 4 {
		n = int(syncsafe(b[:4]))
	} else {
		n = int(binary.BigEndian.Uint32(b[:4])) + 4
	}
	if n <= 0 || n > len(b) {
		return b
	}
	return b[n:]
}

func parseFrames(b []byte, ver byte, t *Tags) {
	idLen, hdrLen := 4, 10
	if ver == 2 {
		idLen, hdrLen = 3, 6
	}
	for len(b) >= hdrLen && b[0] != 0 {
		id := string(b[:idLen])
		var size int
		var fmtFlags byte
		switch ver {
		case 2:
			size = int(b[3])<<16 | int(b[4])<<8 | int(b[5])
		case 3:
			size = int(binary.BigEndian.Uint32(b[4:8]))
			fmtFlags = b[9]
		default:
			size = int(syncsafe(b[4:8]))
			fmtFlags = b[9]
		}
		b = b[hdrLen:]
		if size < 0 || size > len(b) {
			return
		}
		data := b[:size]
		b = b[size:]

		switch ver {
		case 3:
			if fmtFlags&0xc0 != 0 { // compressed or encrypted
				continue
			}
		case 4:
			if fmtFlags&0x0c != 0 { // compressed or encrypted
				continue
			}
			if fmtFlags&0x01 != 0 { // data length indicator
				if len(data) < 4 {
					continue
				}
				data = data[4:]
			}
			if fmtFlags&0x02 != 0 {
				data = unsync(data)
			}
		}
		applyFrame(id, data, t)
	}
}

func applyFrame(id string, data []byte, t *Tags) {
	switch id {
	case "TIT2", "TT2":
		set(&t.Title, textFrame(data))
	case "TPE1", "TP1":
		set(&t.Artist, textFrame(data))
	case "TALB", "TAL":
		set(&t.Album, textFrame(data))
	case "TCON", "TCO":
		set(&t.Genre, textFrame(data))
	case "TYER", "TYE", "TDRC":
		set(&t.Year, textFrame(data))
	case "TRCK", "TRK":
		set(&t.Track, textFrame(data))
	case "APIC", "PIC":
		applyPicture(id == "PIC", data, t)
	}
}

func textFrame(data []byte) string {
	if len(data) < 1 {
		return ""
	}
	return cleanText(decodeText(data[0], data[1:]))
}

func cleanText(s string) string {
	if i := strings.IndexByte(s, 0); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

// decodeText handles ID3 encodings: 0 Latin-1, 1 UTF-16 (BOM), 2 UTF-16BE, 3 UTF-8.
func decodeText(enc byte, b []byte) string {
	switch enc {
	case 0:
		rs := make([]rune, len(b))
		for i, c := range b {
			rs[i] = rune(c)
		}
		return string(rs)
	case 1:
		return decodeUTF16(b, true)
	case 2:
		return decodeUTF16(b, false)
	}
	return strings.ToValidUTF8(string(b), "�")
}

func decodeUTF16(b []byte, detectBOM bool) string {
	little := false
	if detectBOM {
		little = true
		if len(b) >= 2 {
			switch {
			case b[0] == 0xFF && b[1] == 0xFE:
				b = b[2:]
			case b[0] == 0xFE && b[1] == 0xFF:
				little = false
				b = b[2:]
			}
		}
	}
	u := make([]uint16, len(b)/2)
	for i := range u {
		if little {
			u[i] = uint16(b[2*i]) | uint16(b[2*i+1])<<8
		} else {
			u[i] = uint16(b[2*i])<<8 | uint16(b[2*i+1])
		}
	}
	return string(utf16.Decode(u))
}

func indexTerm(b []byte, enc byte) (idx, n int) {
	if enc == 1 || enc == 2 {
		for i := 0; i+1 < len(b); i += 2 {
			if b[i] == 0 && b[i+1] == 0 {
				return i, 2
			}
		}
		return -1, 0
	}
	i := bytes.IndexByte(b, 0)
	if i < 0 {
		return -1, 0
	}
	return i, 1
}

func applyPicture(v22 bool, data []byte, t *Tags) {
	if len(data) < 2 {
		return
	}
	enc, rest := data[0], data[1:]
	var mime string
	if v22 {
		if len(rest) < 3 {
			return
		}
		mime = "image/" + strings.ToLower(string(rest[:3]))
		rest = rest[3:]
	} else {
		i := bytes.IndexByte(rest, 0)
		if i < 0 {
			return
		}
		mime = string(rest[:i])
		rest = rest[i+1:]
	}
	if mime == "image/jpg" {
		mime = "image/jpeg"
	}
	if len(rest) < 1 {
		return
	}
	ptype := int(rest[0])
	rest = rest[1:]
	i, n := indexTerm(rest, enc)
	if i < 0 {
		return
	}
	t.setArt(ptype, mime, rest[i+n:])
}

// ---------------------------------------------------------------- Vorbis comments / FLAC / Ogg

type reader struct {
	b   []byte
	off int
	le  bool
}

func (r *reader) take(n int) ([]byte, bool) {
	if n < 0 || r.off+n > len(r.b) {
		return nil, false
	}
	s := r.b[r.off : r.off+n]
	r.off += n
	return s, true
}

func (r *reader) u32() (uint32, bool) {
	s, ok := r.take(4)
	if !ok {
		return 0, false
	}
	if r.le {
		return binary.LittleEndian.Uint32(s), true
	}
	return binary.BigEndian.Uint32(s), true
}

func parseSample(s string) (int64, bool) {
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	return n, err == nil && n >= 0
}

func parseVorbisComment(b []byte, t *Tags) error {
	r := &reader{b: b, le: true}
	vlen, ok := r.u32()
	if !ok {
		return errors.New("truncated Vorbis comment header")
	}
	if _, ok := r.take(int(vlen)); !ok {
		return errors.New("truncated Vorbis vendor string")
	}
	count, ok := r.u32()
	if !ok {
		return errors.New("truncated Vorbis comment count")
	}
	var loopStart, loopEnd, loopLen string
	for i := uint32(0); i < count; i++ {
		n, ok := r.u32()
		if !ok {
			return errors.New("truncated Vorbis comment")
		}
		raw, ok := r.take(int(n))
		if !ok {
			return errors.New("truncated Vorbis comment")
		}
		k, v, found := strings.Cut(string(raw), "=")
		if !found {
			continue
		}
		v = strings.ToValidUTF8(v, "�")
		switch strings.ToUpper(k) {
		case "TITLE":
			set(&t.Title, strings.TrimSpace(v))
		case "ARTIST":
			set(&t.Artist, strings.TrimSpace(v))
		case "ALBUM":
			set(&t.Album, strings.TrimSpace(v))
		case "GENRE":
			set(&t.Genre, strings.TrimSpace(v))
		case "DATE", "YEAR":
			set(&t.Year, strings.TrimSpace(v))
		case "TRACKNUMBER":
			set(&t.Track, strings.TrimSpace(v))
		case "LOOPSTART":
			loopStart = v
		case "LOOPEND":
			loopEnd = v
		case "LOOPLENGTH":
			loopLen = v
		case "METADATA_BLOCK_PICTURE":
			if pic, err := base64.StdEncoding.DecodeString(strings.TrimSpace(v)); err == nil {
				parsePicture(pic, t)
			}
		}
	}
	t.applyLoop(loopStart, loopEnd, loopLen)
	return nil
}

// applyLoop derives a loop region from LOOPSTART plus LOOPEND, LOOPLENGTH or
// (for FLAC) the stream's total sample count.
func (t *Tags) applyLoop(startS, endS, lenS string) {
	start, ok := parseSample(startS)
	if !ok {
		return
	}
	var end int64
	if v, ok := parseSample(endS); ok {
		end = v
	} else if v, ok := parseSample(lenS); ok {
		end = start + v
	} else {
		end = t.total
	}
	if end <= start {
		return
	}
	t.Loop = &Loop{Start: start, End: end, SampleRate: t.rate}
}

// parsePicture decodes a FLAC METADATA_BLOCK_PICTURE structure.
func parsePicture(b []byte, t *Tags) {
	r := &reader{b: b}
	ptype, ok := r.u32()
	if !ok {
		return
	}
	mlen, ok := r.u32()
	if !ok {
		return
	}
	mime, ok := r.take(int(mlen))
	if !ok {
		return
	}
	dlen, ok := r.u32()
	if !ok {
		return
	}
	if _, ok := r.take(int(dlen)); !ok {
		return
	}
	if _, ok := r.take(16); !ok { // width, height, depth, colours
		return
	}
	n, ok := r.u32()
	if !ok {
		return
	}
	data, ok := r.take(int(n))
	if !ok {
		return
	}
	t.setArt(int(ptype), string(mime), data)
}

func readFLAC(f io.ReadSeeker) (*Tags, error) {
	t := &Tags{}
	magic := make([]byte, 4)
	if _, err := io.ReadFull(f, magic); err != nil || string(magic) != "fLaC" {
		return t, errors.New("not a FLAC file")
	}
	for {
		h := make([]byte, 4)
		if _, err := io.ReadFull(f, h); err != nil {
			return t, fmt.Errorf("truncated FLAC metadata: %w", err)
		}
		last := h[0]&0x80 != 0
		typ := h[0] & 0x7f
		n := int(h[1])<<16 | int(h[2])<<8 | int(h[3])
		switch typ {
		case 0, 4, 6:
			data := make([]byte, n)
			if _, err := io.ReadFull(f, data); err != nil {
				return t, fmt.Errorf("truncated FLAC block: %w", err)
			}
			switch typ {
			case 0:
				parseStreamInfo(data, t)
			case 4:
				if err := parseVorbisComment(data, t); err != nil {
					return t, err
				}
			case 6:
				parsePicture(data, t)
			}
		default:
			if _, err := f.Seek(int64(n), io.SeekCurrent); err != nil {
				return t, err
			}
		}
		if last {
			return t, nil
		}
	}
}

func parseStreamInfo(d []byte, t *Tags) {
	if len(d) < 18 {
		return
	}
	t.rate = int(d[10])<<12 | int(d[11])<<4 | int(d[12])>>4
	t.total = int64(d[13]&0x0f)<<32 | int64(d[14])<<24 | int64(d[15])<<16 | int64(d[16])<<8 | int64(d[17])
	t.SampleRate = t.rate
}

func readOgg(f io.Reader, opus bool) (*Tags, error) {
	t := &Tags{}
	r := bufio.NewReader(io.LimitReader(f, maxTag))
	var packets [][]byte
	var cur []byte
	for {
		hdr := make([]byte, 27)
		if _, err := io.ReadFull(r, hdr); err != nil {
			return t, errors.New("truncated Ogg stream")
		}
		if string(hdr[:4]) != "OggS" {
			return t, errors.New("bad Ogg page")
		}
		segs := make([]byte, int(hdr[26]))
		if _, err := io.ReadFull(r, segs); err != nil {
			return t, errors.New("truncated Ogg page")
		}
		for _, sl := range segs {
			chunk := make([]byte, int(sl))
			if _, err := io.ReadFull(r, chunk); err != nil {
				return t, errors.New("truncated Ogg page")
			}
			cur = append(cur, chunk...)
			if sl < 255 { // packet boundary
				packets = append(packets, cur)
				cur = nil
				if len(packets) == 2 {
					return parseOggHeaders(packets[0], packets[1], opus, t)
				}
			}
		}
	}
}

func parseOggHeaders(id, comment []byte, opus bool, t *Tags) (*Tags, error) {
	if opus {
		if !bytes.HasPrefix(id, []byte("OpusHead")) || !bytes.HasPrefix(comment, []byte("OpusTags")) {
			return t, errors.New("not an Opus stream")
		}
		t.rate, t.SampleRate = 48000, 48000 // Opus loop points are in 48 kHz samples
		return t, parseVorbisComment(comment[8:], t)
	}
	if len(id) < 16 || !bytes.HasPrefix(id, []byte("\x01vorbis")) || !bytes.HasPrefix(comment, []byte("\x03vorbis")) {
		return t, errors.New("not a Vorbis stream")
	}
	t.rate = int(binary.LittleEndian.Uint32(id[12:16]))
	t.SampleRate = t.rate
	return t, parseVorbisComment(comment[7:], t)
}

// ---------------------------------------------------------------- WAV

func readWAV(f io.ReadSeeker) (*Tags, error) {
	t := &Tags{}
	hdr := make([]byte, 12)
	if _, err := io.ReadFull(f, hdr); err != nil || string(hdr[:4]) != "RIFF" || string(hdr[8:12]) != "WAVE" {
		return t, errors.New("not a WAV file")
	}
	for i := 0; i < 256; i++ {
		ch := make([]byte, 8)
		if _, err := io.ReadFull(f, ch); err != nil {
			break // end of file
		}
		id := string(ch[:4])
		size := int64(binary.LittleEndian.Uint32(ch[4:8]))
		padded := size + size&1
		wanted := id == "fmt " || id == "smpl" || id == "LIST"
		if !wanted || size > maxTag {
			if _, err := f.Seek(padded, io.SeekCurrent); err != nil {
				break
			}
			continue
		}
		data := make([]byte, size)
		if _, err := io.ReadFull(f, data); err != nil {
			return t, fmt.Errorf("truncated WAV chunk: %w", err)
		}
		if size&1 == 1 {
			if _, err := f.Seek(1, io.SeekCurrent); err != nil {
				break
			}
		}
		switch id {
		case "fmt ":
			if len(data) >= 8 {
				t.rate = int(binary.LittleEndian.Uint32(data[4:8]))
				t.SampleRate = t.rate
			}
		case "smpl":
			parseSmpl(data, t)
		case "LIST":
			parseInfoList(data, t)
		}
	}
	if t.Loop != nil && t.Loop.SampleRate == 0 {
		t.Loop.SampleRate = t.rate
	}
	return t, nil
}

func parseSmpl(d []byte, t *Tags) {
	if len(d) < 60 || binary.LittleEndian.Uint32(d[28:32]) < 1 {
		return
	}
	start := int64(binary.LittleEndian.Uint32(d[44:48]))
	end := int64(binary.LittleEndian.Uint32(d[48:52])) + 1 // smpl end is inclusive
	if end > start {
		t.Loop = &Loop{Start: start, End: end, SampleRate: t.rate}
	}
}

func parseInfoList(d []byte, t *Tags) {
	if len(d) < 4 || string(d[:4]) != "INFO" {
		return
	}
	off := 4
	for off+8 <= len(d) {
		id := string(d[off : off+4])
		sz := int(binary.LittleEndian.Uint32(d[off+4 : off+8]))
		off += 8
		if sz < 0 || off+sz > len(d) {
			return
		}
		text := cleanText(strings.ToValidUTF8(string(d[off:off+sz]), "�"))
		off += sz + sz&1
		switch id {
		case "INAM":
			set(&t.Title, text)
		case "IART":
			set(&t.Artist, text)
		case "IPRD":
			set(&t.Album, text)
		case "IGNR":
			set(&t.Genre, text)
		case "ICRD":
			set(&t.Year, text)
		case "ITRK":
			set(&t.Track, text)
		}
	}
}
