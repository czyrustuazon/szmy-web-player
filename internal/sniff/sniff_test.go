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

func TestFFmpegFormatsByMagicAndExtension(t *testing.T) {
	asf := []byte{0x30, 0x26, 0xB2, 0x75, 0x8E, 0x66, 0xCF, 0x11, 0xA6, 0xD9, 0x00, 0xAA, 0x00, 0x62, 0xCE, 0x6C, 1, 2}
	magics := map[string][]byte{
		"asf/wma/wmv": asf,
		"ape":         []byte("MAC \x96\x0f"),
		"wavpack":     []byte("wvpk\x00"),
		"tta":         []byte("TTA1"),
		"musepack":    []byte("MPCK"),
		"musepack sv7": []byte("MP+\x07"),
		"dsf":         []byte("DSD \x1c"),
		"dff":         []byte("FRM8"),
		"flv":         []byte("FLV\x01\x05"),
		"amr":         []byte("#!AMR\n"),
		"matroska":    {0x1A, 0x45, 0xDF, 0xA3, 0x01},
		"aiff":        []byte("FORM\x00\x00\x00\x00AIFFCOMM"),
		"aifc":        []byte("FORM\x00\x00\x00\x00AIFCFVER"),
		"avi":         []byte("RIFF\x00\x00\x00\x00AVI LIST"),
	}
	for name, header := range magics {
		if got := Detect(header, "x.bin"); got != FFmpeg {
			t.Errorf("%s: got %v", name, got)
		}
	}
	// A WAV is still a WAV, a lone FORM chunk that is neither AIFF nor AIFC is not claimed.
	if Detect([]byte("RIFF\x00\x00\x00\x00WAVEfmt "), "x.bin") != WAV {
		t.Error("wav")
	}
	if got := Detect([]byte("FORM\x00\x00\x00\x00ILBM"), "x.bin"); got != Unknown {
		t.Errorf("an IFF picture is not audio: %v", got)
	}
	if got := Detect([]byte("RIFF\x00\x00\x00\x00ACON"), "x.bin"); got != Unknown {
		t.Errorf("an animated cursor is not audio: %v", got)
	}
	for _, name := range []string{"a.wma", "A.WMV", "x.asf", "x.ape", "x.wv", "x.tta", "x.mka", "x.mpc", "x.dsf", "x.dff",
		"x.flv", "x.avi", "x.mkv", "x.mov", "x.webm", "x.3gp", "x.amr", "x.ac3", "x.dts", "x.aif", "x.aiff", "x.aifc", "x.mp2", "x.caf"} {
		if FromExt(name) != FFmpeg {
			t.Errorf("%s should need ffmpeg", name)
		}
	}
	for _, name := range []string{"x.mid", "x.midi", "x.cue", "x.jpg"} {
		if FromExt(name) != Unknown {
			t.Errorf("%s is not audio", name)
		}
	}
	if FFmpeg.Native() || FFmpeg.String() != "ffmpeg" || FFmpeg.MIME() != "audio/flac" {
		t.Errorf("properties: %v %q %q", FFmpeg.Native(), FFmpeg.String(), FFmpeg.MIME())
	}
}
