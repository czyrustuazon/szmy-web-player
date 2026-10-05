// Package sniff identifies audio formats from content first and file
// extension second, like szmy's file_magic.
package sniff

import (
	"bytes"
	"path/filepath"
	"strings"
)

// Kind is a detected audio format.
type Kind int

const (
	Unknown Kind = iota
	MP3
	FLAC
	WAV
	Vorbis
	Opus
	M4A
	VGM    // anything that needs vgmstream (BRSTM, BCSTM, BFSTM, ADPCM, ...)
	FFmpeg // formats browsers cannot play that ffmpeg can: WMA, APE, WavPack, TTA, ASF/WMV/FLV audio, ...
)

var names = [...]string{"unknown", "mp3", "flac", "wav", "ogg", "opus", "m4a", "vgm", "ffmpeg"}

func (k Kind) String() string {
	if k < 0 || int(k) >= len(names) {
		return names[0]
	}
	return names[k]
}

// Native reports whether browsers can usually play the format directly.
func (k Kind) Native() bool { return k != Unknown && k != VGM && k != FFmpeg }

// MIME is the Content-Type used when serving the original file.
func (k Kind) MIME() string {
	switch k {
	case MP3:
		return "audio/mpeg"
	case FLAC, FFmpeg:
		return "audio/flac" // FFmpeg files are only ever served after conversion to FLAC
	case WAV, VGM:
		return "audio/wav" // VGM is only ever served after rendering to WAV
	case Vorbis:
		return "audio/ogg"
	case Opus:
		return "audio/ogg; codecs=opus"
	case M4A:
		return "audio/mp4"
	}
	return "application/octet-stream"
}

var vgmExts = map[string]bool{
	"brstm": true, "bcstm": true, "bfstm": true, "brwav": true, "bcwav": true,
	"bfwav": true, "bwav": true, "adx": true, "hca": true, "dsp": true,
	"idsp": true, "ast": true, "aax": true, "awb": true, "acb": true,
	"wem": true, "fsb": true, "at3": true, "at9": true, "vag": true,
	"xa": true, "ads": true, "ss2": true, "genh": true, "mus": true,
	"strm": true, "lwav": true, "nus3audio": true, "logg": true, "lopus": true,
	"adp": true, "agsc": true, "gcm": true, "mib": true, "mihb": true,
	"xwma": true, "xvag": true, "rwsd": true, "rsd": true,
}

// Extensions ffmpeg converts for us. Video containers are included for their audio track
// (a "music video", a drama CD ripped to WMV). MIDI is deliberately absent: it needs a soundfont.
var ffmpegExts = map[string]bool{
	"wma": true, "wmv": true, "asf": true, "ape": true, "wv": true, "tta": true, "mka": true,
	"mpc": true, "dsf": true, "dff": true, "flv": true, "avi": true, "mkv": true, "mov": true,
	"webm": true, "3gp": true, "amr": true, "ac3": true, "dts": true, "aif": true, "aiff": true,
	"aifc": true, "mp2": true, "caf": true,
}

var asfGUID = []byte{0x30, 0x26, 0xB2, 0x75, 0x8E, 0x66, 0xCF, 0x11, 0xA6, 0xD9, 0x00, 0xAA, 0x00, 0x62, 0xCE, 0x6C}

var vgmMagics = [][]byte{
	[]byte("RSTM"), []byte("CSTM"), []byte("FSTM"),
	[]byte("RWAV"), []byte("CWAV"), []byte("FWAV"),
	[]byte("BWAV"), []byte("IDSP"),
}

// Detect inspects the first bytes of a file (512 are plenty) and falls back
// to the file name's extension.
func Detect(header []byte, name string) Kind {
	if k := fromMagic(header); k != Unknown {
		return k
	}
	return FromExt(name)
}

func fromMagic(h []byte) Kind {
	switch {
	case bytes.HasPrefix(h, []byte("ID3")):
		return MP3
	case bytes.HasPrefix(h, []byte("fLaC")):
		return FLAC
	case len(h) >= 12 && bytes.HasPrefix(h, []byte("RIFF")) && string(h[8:12]) == "WAVE":
		return WAV
	case isFFmpegMagic(h):
		return FFmpeg
	case bytes.HasPrefix(h, []byte("OggS")):
		if bytes.Contains(h, []byte("OpusHead")) {
			return Opus
		}
		return Vorbis
	case len(h) >= 8 && string(h[4:8]) == "ftyp":
		return M4A
	case isVGMMagic(h):
		return VGM
	case isMP3Sync(h):
		return MP3
	}
	return Unknown
}

func isFFmpegMagic(h []byte) bool {
	switch {
	case bytes.HasPrefix(h, asfGUID): // WMA, WMV, ASF
		return true
	case bytes.HasPrefix(h, []byte("MAC ")), // Monkey's Audio
		bytes.HasPrefix(h, []byte("wvpk")), // WavPack
		bytes.HasPrefix(h, []byte("TTA1")), // True Audio
		bytes.HasPrefix(h, []byte("MPCK")), bytes.HasPrefix(h, []byte("MP+")), // Musepack
		bytes.HasPrefix(h, []byte("DSD ")), bytes.HasPrefix(h, []byte("FRM8")), // DSF, DFF
		bytes.HasPrefix(h, []byte("FLV")),
		bytes.HasPrefix(h, []byte("#!AMR")),
		bytes.HasPrefix(h, []byte{0x1A, 0x45, 0xDF, 0xA3}): // Matroska, WebM
		return true
	case len(h) >= 12 && bytes.HasPrefix(h, []byte("FORM")) && (string(h[8:12]) == "AIFF" || string(h[8:12]) == "AIFC"):
		return true
	case len(h) >= 12 && bytes.HasPrefix(h, []byte("RIFF")) && string(h[8:12]) == "AVI ":
		return true
	}
	return false
}

func isVGMMagic(h []byte) bool {
	for _, m := range vgmMagics {
		if bytes.HasPrefix(h, m) {
			return true
		}
	}
	// HCA headers may be masked with 0x80 per byte.
	return len(h) >= 4 && h[0]&0x7f == 'H' && h[1]&0x7f == 'C' && h[2]&0x7f == 'A' && h[3]&0x7f == 0
}

// isMP3Sync matches an MPEG audio frame header (not ADTS AAC, whose layer bits are 00).
func isMP3Sync(h []byte) bool {
	return len(h) >= 2 && h[0] == 0xFF && h[1]&0xE0 == 0xE0 &&
		(h[1]>>1)&3 != 0 && (h[1]>>3)&3 != 1
}

// FromExt classifies by extension only.
func FromExt(name string) Kind {
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(name), "."))
	switch ext {
	case "mp3":
		return MP3
	case "flac":
		return FLAC
	case "wav", "wave":
		return WAV
	case "ogg", "oga":
		return Vorbis
	case "opus":
		return Opus
	case "m4a", "aac", "mp4":
		return M4A
	}
	if vgmExts[ext] {
		return VGM
	}
	if ffmpegExts[ext] {
		return FFmpeg
	}
	return Unknown
}
