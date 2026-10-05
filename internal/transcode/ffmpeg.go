package transcode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// formats are the demuxers the player needs: what sniff routes to ffmpeg, plus the formats
// browsers play natively (transcode=1 converts those too).
const formats = "asf,ape,wv,tta,mpc,mpc8,dsf,iff,flv,amr,matroska,webm,avi,mov,mp4,aiff,ac3,eac3,dts,caf," +
	"mp3,flac,wav,w64,ogg,aac"

// FFmpeg drives ffmpeg and ffprobe: it converts formats browsers cannot play (WMA, APE,
// WavPack, TTA, the audio of WMV/FLV/MKV/AVI files, ...) to FLAC, which every browser plays,
// losslessly, and reads their tags.
//
// -protocol_whitelist file keeps a crafted playlist inside an uploaded file from making ffmpeg
// open network addresses or other protocols, and -format_whitelist keeps ffmpeg to audio and
// video containers, so such a file cannot be read as a playlist (HLS, concat, ...) that
// stitches other files on the disk into the audio.
type FFmpeg struct {
	Bin   string   // ffmpeg
	Probe string   // ffprobe
	Exec  ExecFunc // nil means a real subprocess
}

// Metadata reads tags and stream info with ffprobe.
func (f FFmpeg) Metadata(ctx context.Context, src string) (Info, error) {
	out, err := runCommand(ctx, f.Exec, f.Probe, "-v", "error", "-protocol_whitelist", "file", "-format_whitelist", formats,
		"-print_format", "json", "-show_format", "-show_streams", src)
	if err != nil {
		return Info{}, err
	}
	return ParseProbe(out)
}

// Decode converts the first audio stream to FLAC.
func (f FFmpeg) Decode(ctx context.Context, src, dst string) error {
	_, err := runCommand(ctx, f.Exec, f.Bin, "-nostdin", "-y", "-v", "error", "-protocol_whitelist", "file",
		"-format_whitelist", formats, "-i", src, "-vn", "-map", "0:a:0", "-c:a", "flac", "-f", "flac", dst)
	return err
}

// ParseProbe reads the JSON printed by `ffprobe -print_format json -show_format -show_streams`.
// It fails if the file has no audio stream.
func ParseProbe(data []byte) (Info, error) {
	var p struct {
		Streams []struct {
			CodecType  string            `json:"codec_type"`
			SampleRate string            `json:"sample_rate"`
			Channels   int               `json:"channels"`
			Tags       map[string]string `json:"tags"`
		} `json:"streams"`
		Format struct {
			Tags map[string]string `json:"tags"`
		} `json:"format"`
	}
	if err := json.Unmarshal(data, &p); err != nil {
		return Info{}, fmt.Errorf("unreadable ffprobe output: %w", err)
	}
	for _, st := range p.Streams {
		if st.CodecType != "audio" {
			continue
		}
		tags := map[string]string{}
		for _, src := range []map[string]string{st.Tags, p.Format.Tags} { // format tags win
			for k, v := range src {
				tags[strings.ToLower(k)] = strings.TrimSpace(v)
			}
		}
		first := func(keys ...string) string {
			for _, k := range keys {
				if v := tags[k]; v != "" {
					return v
				}
			}
			return ""
		}
		rate, _ := strconv.Atoi(st.SampleRate)
		year := first("date", "year")
		if len(year) >= 4 && strings.Trim(year[:4], "0123456789") == "" {
			year = year[:4]
		}
		return Info{
			SampleRate: rate, Channels: st.Channels,
			Title: first("title"), Artist: first("artist", "album_artist", "author"),
			Album: first("album"), Genre: first("genre"), Year: year, Track: first("track", "tracknumber"),
		}, nil
	}
	return Info{}, errors.New("no audio stream")
}
