package transcode

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const wmaProbe = `{
  "streams": [
    {"codec_type": "video", "width": 320},
    {"codec_type": "audio", "sample_rate": "44100", "channels": 2, "tags": {"TITLE": "stream title", "language": "eng"}}
  ],
  "format": {"tags": {"title": "Sonic Next Gen Mix", "ARTIST": "Sonic Team", "album": "Sonic 2006",
                      "genre": "Game", "date": "2006-11-14", "track": "7"}}
}`

func TestParseProbeReadsTagsFromTheFormatAndTheAudioStream(t *testing.T) {
	in, err := ParseProbe([]byte(wmaProbe))
	if err != nil {
		t.Fatal(err)
	}
	want := Info{SampleRate: 44100, Channels: 2, Title: "Sonic Next Gen Mix", Artist: "Sonic Team", Album: "Sonic 2006", Genre: "Game", Year: "2006", Track: "7"}
	if in != want {
		t.Fatalf("got %+v\nwant %+v", in, want)
	}
}

func TestParseProbeFallbacksAndOddValues(t *testing.T) {
	// Tags only on the stream, alternative key names, an unusable sample rate and a short year.
	in, err := ParseProbe([]byte(`{"streams":[{"codec_type":"audio","sample_rate":"n/a","channels":1,
	  "tags":{"Author":"  Someone ","tracknumber":"3/12","year":"99"}}],"format":{}}`))
	if err != nil {
		t.Fatal(err)
	}
	if in.Artist != "Someone" || in.Track != "3/12" || in.Year != "99" || in.SampleRate != 0 || in.Channels != 1 || in.Title != "" {
		t.Errorf("got %+v", in)
	}
	// album_artist is used when there is no artist; a non-numeric start of the date is kept as is.
	in, _ = ParseProbe([]byte(`{"streams":[{"codec_type":"audio"}],"format":{"tags":{"album_artist":"Band","date":"spring"}}}`))
	if in.Artist != "Band" || in.Year != "spring" {
		t.Errorf("got %+v", in)
	}
}

func TestParseProbeErrors(t *testing.T) {
	if _, err := ParseProbe([]byte(`{"streams":[{"codec_type":"video"}],"format":{}}`)); err == nil || !strings.Contains(err.Error(), "no audio stream") {
		t.Errorf("video only: %v", err)
	}
	if _, err := ParseProbe([]byte(`{}`)); err == nil {
		t.Error("empty probe")
	}
	if _, err := ParseProbe([]byte(`not json`)); err == nil || !strings.Contains(err.Error(), "unreadable") {
		t.Errorf("garbage: %v", err)
	}
}

func TestFFmpegCommands(t *testing.T) {
	var gotName string
	var gotArgs []string
	f := FFmpeg{Bin: "/usr/bin/ffmpeg", Probe: "/usr/bin/ffprobe", Exec: func(ctx context.Context, name string, args ...string) ([]byte, error) {
		gotName, gotArgs = name, args
		return []byte(wmaProbe), nil
	}}
	in, err := f.Metadata(context.Background(), "/m/a.wma")
	if err != nil || in.Title != "Sonic Next Gen Mix" {
		t.Fatalf("metadata: %+v %v", in, err)
	}
	if gotName != "/usr/bin/ffprobe" || gotArgs[len(gotArgs)-1] != "/m/a.wma" || !strings.Contains(strings.Join(gotArgs, " "), "-protocol_whitelist file -format_whitelist "+formats) {
		t.Errorf("probe command: %s %v", gotName, gotArgs)
	}

	if err := f.Decode(context.Background(), "/m/a.wma", "/c/out.flac.part"); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(gotArgs, " ")
	for _, want := range []string{"-nostdin", "-protocol_whitelist file", "-i /m/a.wma", "-vn", "-map 0:a:0", "-c:a flac", "-f flac /c/out.flac.part"} {
		if !strings.Contains(joined, want) {
			t.Errorf("decode command lacks %q: %s", want, joined)
		}
	}
	if gotName != "/usr/bin/ffmpeg" || strings.Index(joined, "-protocol_whitelist") > strings.Index(joined, "-i ") ||
		strings.Index(joined, "-format_whitelist") > strings.Index(joined, "-i ") {
		t.Errorf("the whitelists are input options and must come before -i: %s", joined)
	}
	for _, banned := range []string{"hls", "concat", "image2", "tee"} {
		for _, f := range strings.Split(formats, ",") {
			if f == banned {
				t.Errorf("%s must not be an allowed input format", banned)
			}
		}
	}
}

func TestFFmpegErrorsCarryTheToolOutput(t *testing.T) {
	f := FFmpeg{Bin: "ffmpeg", Probe: "ffprobe", Exec: func(context.Context, string, ...string) ([]byte, error) {
		return []byte("Invalid data found when processing input"), errors.New("exit status 1")
	}}
	if _, err := f.Metadata(context.Background(), "x"); err == nil || !strings.Contains(err.Error(), "Invalid data") {
		t.Errorf("probe: %v", err)
	}
	if err := f.Decode(context.Background(), "x", "y"); err == nil || !strings.Contains(err.Error(), "Invalid data") {
		t.Errorf("decode: %v", err)
	}
	bad := FFmpeg{Probe: "ffprobe", Exec: func(context.Context, string, ...string) ([]byte, error) { return []byte("garbage"), nil }}
	if _, err := bad.Metadata(context.Background(), "x"); err == nil {
		t.Error("unparseable probe output")
	}
	// With no Exec it runs a real process; a missing binary is an error, not a panic.
	missing := FFmpeg{Bin: "definitely-not-a-real-binary-xyz", Probe: "definitely-not-a-real-binary-xyz"}
	if _, err := missing.Metadata(context.Background(), "x"); err == nil {
		t.Error("missing ffprobe")
	}
	if err := missing.Decode(context.Background(), "x", "y"); err == nil {
		t.Error("missing ffmpeg")
	}
}

func TestServiceRendersFLACWithItsOwnExtensionMimeAndCache(t *testing.T) {
	r := &fakeRunner{}
	dir := t.TempDir()
	s, err := New(r, filepath.Join(dir, "cache"), 1, 30, WithExt(".flac"), WithTimeout(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if s.MIME() != "audio/flac" || s.timeout != time.Minute {
		t.Errorf("options: %s %v", s.MIME(), s.timeout)
	}
	src := filepath.Join(dir, "song.wma")
	os.WriteFile(src, []byte("asf"), 0o644)
	out, err := s.Render(context.Background(), src)
	if err != nil || filepath.Ext(out) != ".flac" {
		t.Fatalf("render: %q %v", out, err)
	}

	// Cache trimming only looks at this service's own extension.
	now := time.Now()
	for i, name := range []string{"a.flac", "b.flac", "c.wav"} {
		p := filepath.Join(s.cacheDir, name)
		os.WriteFile(p, []byte("RIFFfakewav"), 0o644)
		mt := now.Add(time.Duration(i-5) * time.Minute)
		os.Chtimes(p, mt, mt)
	}
	s.trim()
	if _, err := os.Stat(filepath.Join(s.cacheDir, "c.wav")); err != nil {
		t.Error("a file with another extension is not this cache's business")
	}
	if _, err := os.Stat(filepath.Join(s.cacheDir, "a.flac")); err == nil {
		t.Error("the oldest .flac render should have been evicted")
	}

	wav, _ := New(r, filepath.Join(dir, "wavcache"), 1, 0)
	if wav.MIME() != "audio/wav" || wav.ext != ".wav" {
		t.Errorf("defaults stay as they were: %s %s", wav.MIME(), wav.ext)
	}
}
