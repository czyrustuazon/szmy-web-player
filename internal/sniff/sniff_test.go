package sniff

import "testing"

func TestDetectByMagic(t *testing.T) {
	cases := []struct {
		name   string
		header []byte
		file   string
		want   Kind
	}{
		{"id3", []byte("ID3\x03\x00"), "x.bin", MP3},
		{"mp3 sync", []byte{0xFF, 0xFB, 0x90, 0x00}, "x.bin", MP3},
		{"flac", []byte("fLaC\x00"), "x.bin", FLAC},
		{"wav", []byte("RIFF\x00\x00\x00\x00WAVEfmt "), "x.bin", WAV},
		{"ogg vorbis", []byte("OggS\x00\x02vorbis"), "x.bin", Vorbis},
		{"ogg opus", []byte("OggS\x00\x02\x00\x00\x00\x00\x00\x00\x00\x00OpusHead"), "x.bin", Opus},
		{"m4a", []byte("\x00\x00\x00\x20ftypM4A "), "x.bin", M4A},
		{"brstm", []byte("RSTM\xFE\xFF"), "x.bin", VGM},
		{"bcstm", []byte("CSTM"), "x.bin", VGM},
		{"bfstm", []byte("FSTM"), "x.bin", VGM},
		{"bwav", []byte("BWAV"), "x.bin", VGM},
		{"idsp", []byte("IDSP"), "x.bin", VGM},
		{"hca", []byte("HCA\x00"), "x.bin", VGM},
		{"hca masked", []byte{0xC8, 0xC3, 0xC1, 0x00}, "x.bin", VGM},
		{"content beats extension", []byte("fLaC"), "x.mp3", FLAC},
	}
	for _, c := range cases {
		if got := Detect(c.header, c.file); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

func TestDetectFallsBackToExtension(t *testing.T) {
	cases := map[string]Kind{
		"a.MP3": MP3, "a.flac": FLAC, "a.wav": WAV, "a.wave": WAV, "a.ogg": Vorbis,
		"a.oga": Vorbis, "a.opus": Opus, "a.m4a": M4A, "a.aac": M4A,
		"a.brstm": VGM, "a.BCSTM": VGM, "a.adx": VGM, "a.txt": Unknown, "noext": Unknown,
	}
	for name, want := range cases {
		if got := Detect([]byte("garbage"), name); got != want {
			t.Errorf("%s: got %v want %v", name, got, want)
		}
		if got := FromExt(name); got != want {
			t.Errorf("FromExt %s: got %v want %v", name, got, want)
		}
	}
}

func TestADTSIsNotMP3(t *testing.T) {
	// ADTS AAC frames start FF F1 (layer bits 00) and must not be classified as MP3.
	if got := Detect([]byte{0xFF, 0xF1, 0x50, 0x80}, "x.bin"); got != Unknown {
		t.Fatalf("got %v", got)
	}
	// Reserved MPEG version (01) is not MP3 either.
	if got := Detect([]byte{0xFF, 0xEB}, "x.bin"); got != Unknown {
		t.Fatalf("got %v", got)
	}
}

func TestShortOrEmptyHeader(t *testing.T) {
	for _, h := range [][]byte{nil, {}, {0xFF}, []byte("RIFF"), []byte("Og")} {
		if got := Detect(h, "x.bin"); got != Unknown {
			t.Errorf("header %q: got %v", h, got)
		}
	}
}

func TestKindProperties(t *testing.T) {
	if Unknown.Native() || VGM.Native() {
		t.Error("unknown and vgm must not be native")
	}
	for _, k := range []Kind{MP3, FLAC, WAV, Vorbis, Opus, M4A} {
		if !k.Native() {
			t.Errorf("%v should be native", k)
		}
		if k.MIME() == "application/octet-stream" || k.String() == "unknown" {
			t.Errorf("%v missing mime or name", k)
		}
	}
	if VGM.MIME() != "audio/wav" || Unknown.MIME() != "application/octet-stream" {
		t.Error("unexpected MIME for vgm/unknown")
	}
	if Kind(99).String() != "unknown" || Kind(-1).String() != "unknown" {
		t.Error("out of range kinds should stringify as unknown")
	}
}
