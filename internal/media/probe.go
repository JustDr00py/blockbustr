// Package media probes media files with ffprobe (DESIGN §6) and, later,
// makes the direct-play / remux / transcode decision (§8.1).
//
// Ported from jollyrogarr's internal/media (context-bound ffprobe exec with
// stderr in errors, swappable runner for tests, real-ffmpeg fixture tests)
// and extended: blockbustr keeps every stream, including image subtitles,
// because Jellyfin's MediaStreams list them all.
package media

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// StreamType mirrors Jellyfin's MediaStreamType (and media_streams.type).
type StreamType string

const (
	StreamVideo         StreamType = "Video"
	StreamAudio         StreamType = "Audio"
	StreamSubtitle      StreamType = "Subtitle"
	StreamEmbeddedImage StreamType = "EmbeddedImage" // cover art stored as a video stream
	StreamData          StreamType = "Data"
)

// Info is what ffprobe reports about one media file.
type Info struct {
	Container   string        // ffprobe format_name, e.g. "matroska,webm", "mov,mp4,m4a,3gp,3g2,mj2"
	Duration    time.Duration // 0 if unknown
	Size        int64         // bytes; 0 if unknown
	Bitrate     int64         // container bits/s; 0 if unknown
	Streams     []Stream
	Chapters    []Chapter
	Attachments int // MKV attachments (fonts for ASS subtitles); not streams in Jellyfin's sense
}

// Stream is one video, audio, subtitle or data stream.
type Stream struct {
	Index    int
	TimeBase string // e.g. "1/1000"
	Type     StreamType
	Codec    string // ffprobe codec_name: "h264", "hevc", "av1", "aac", "eac3", "subrip", "hdmv_pgs_subtitle", …
	Profile  string // "High", "Main 10", "LC", …
	Level    int    // ffprobe level (h264 41 = 4.1, hevc 150 = 5.0)
	Language string // ISO 639-2 as tagged ("jpn", "eng"); "" if untagged
	Title    string

	IsDefault, IsForced, IsHearingImpaired, IsOriginal bool

	// IsExternal marks a sidecar subtitle file (Path) rather than a stream
	// inside the media file.
	IsExternal bool
	Path       string

	Bitrate int64 // bits/s: stream bit_rate, else the MKV "BPS" tag

	// Video
	Width, Height    int
	BitDepth         int
	PixelFormat      string
	AspectRatio      string  // display aspect ratio, e.g. "16:9"
	AverageFrameRate float64 // avg_frame_rate
	RealFrameRate    float64 // r_frame_rate
	IsInterlaced     bool
	ColorTransfer    string // "smpte2084", "arib-std-b67", "bt709", ""
	ColorPrimaries   string
	ColorSpace       string
	ColorRange       string
	VideoRange       string // Jellyfin VideoRange: "SDR", "HDR", "" for non-video
	VideoRangeType   string // Jellyfin VideoRangeType: "SDR", "HDR10", "HLG", "DOVI", "DOVIWithHDR10", …
	DoVi             *DolbyVision

	// Audio
	Channels      int
	ChannelLayout string
	SampleRate    int
}

// DolbyVision is a stream's DOVI configuration record.
type DolbyVision struct {
	Profile, Level, BLSignalCompatibilityID int
	VersionMajor, VersionMinor              int
	RPU, EL, BL                             bool
}

// Chapter is one chapter marker.
type Chapter struct {
	Start time.Duration
	Title string
}

// IsTextSubtitle reports whether a subtitle stream is text-based (can be
// delivered as SRT/VTT); image ones (PGS, VobSub, DVB) must be burned in.
func (s Stream) IsTextSubtitle() bool { return s.Type == StreamSubtitle && textSubtitleCodecs[s.Codec] }

// textSubtitleCodecs are subtitle codecs ffmpeg can convert to WebVTT/SRT.
var textSubtitleCodecs = map[string]bool{
	"subrip": true, "srt": true, "ass": true, "ssa": true,
	"mov_text": true, "webvtt": true, "text": true,
}

// Options tune a probe.
type Options struct {
	// Remote marks network targets (.strm URLs): probe less data, as remote
	// reads are slow (DESIGN §6). Bound the time with ctx.
	Remote bool
	// SkipChapters leaves chapters out (one read less on a slow remote).
	SkipChapters bool
}

// Prober runs ffprobe.
type Prober struct {
	// Binary is the ffprobe executable; "" means "ffprobe" on PATH.
	Binary string
	// run replaces the exec in tests.
	run func(ctx context.Context, bin string, args []string) ([]byte, error)
}

// Probe runs ffprobe on target (a local path or URL).
func (p Prober) Probe(ctx context.Context, target string, opts Options) (*Info, error) {
	args := []string{"-v", "error", "-print_format", "json", "-show_format", "-show_streams"}
	if !opts.SkipChapters {
		args = append(args, "-show_chapters")
	}
	if opts.Remote {
		args = append(args, "-probesize", "10000000", "-analyzeduration", "5000000")
	}
	args = append(args, target)
	bin := p.Binary
	if bin == "" {
		bin = "ffprobe"
	}
	run := p.run
	if run == nil {
		run = execFFProbe
	}
	data, err := run(ctx, bin, args)
	if err != nil {
		return nil, err
	}
	return Parse(data)
}

func execFFProbe(ctx context.Context, bin string, args []string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("ffprobe: %w", ctx.Err())
		}
		return nil, fmt.Errorf("ffprobe: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return out.Bytes(), nil
}

type ffprobeOutput struct {
	Format struct {
		FormatName string `json:"format_name"`
		Duration   string `json:"duration"`
		Size       string `json:"size"`
		BitRate    string `json:"bit_rate"`
	} `json:"format"`
	Streams []struct {
		Index              int               `json:"index"`
		CodecType          string            `json:"codec_type"`
		CodecName          string            `json:"codec_name"`
		Profile            string            `json:"profile"`
		Level              int               `json:"level"`
		Width              int               `json:"width"`
		Height             int               `json:"height"`
		PixFmt             string            `json:"pix_fmt"`
		BitsPerRawSample   string            `json:"bits_per_raw_sample"`
		DisplayAspectRatio string            `json:"display_aspect_ratio"`
		FieldOrder         string            `json:"field_order"`
		ColorTransfer      string            `json:"color_transfer"`
		ColorPrimaries     string            `json:"color_primaries"`
		ColorSpace         string            `json:"color_space"`
		ColorRange         string            `json:"color_range"`
		RFrameRate         string            `json:"r_frame_rate"`
		AvgFrameRate       string            `json:"avg_frame_rate"`
		BitRate            string            `json:"bit_rate"`
		Channels           int               `json:"channels"`
		ChannelLayout      string            `json:"channel_layout"`
		SampleRate         string            `json:"sample_rate"`
		TimeBase           string            `json:"time_base"`
		Disposition        map[string]int    `json:"disposition"`
		Tags               map[string]string `json:"tags"`
		SideDataList       []map[string]any  `json:"side_data_list"`
	} `json:"streams"`
	Chapters []struct {
		StartTime string            `json:"start_time"`
		Tags      map[string]string `json:"tags"`
	} `json:"chapters"`
}

// Parse decodes ffprobe's JSON (-show_format -show_streams -show_chapters).
func Parse(data []byte) (*Info, error) {
	var raw ffprobeOutput
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("media: parse ffprobe output: %w", err)
	}
	info := &Info{
		Container: raw.Format.FormatName,
		Duration:  seconds(raw.Format.Duration),
		Size:      atoi64(raw.Format.Size),
		Bitrate:   atoi64(raw.Format.BitRate),
	}
	for _, s := range raw.Streams {
		st := Stream{
			Index:             s.Index,
			TimeBase:          s.TimeBase,
			Codec:             s.CodecName,
			Profile:           s.Profile,
			Level:             s.Level,
			Language:          tag(s.Tags, "language"),
			Title:             tag(s.Tags, "title"),
			IsDefault:         s.Disposition["default"] == 1,
			IsForced:          s.Disposition["forced"] == 1,
			IsHearingImpaired: s.Disposition["hearing_impaired"] == 1,
			IsOriginal:        s.Disposition["original"] == 1,
			Bitrate:           atoi64(s.BitRate),
		}
		if st.Bitrate == 0 { // Matroska stores per-stream bitrate as a statistics tag
			st.Bitrate = atoi64(tag(s.Tags, "BPS"))
		}
		switch s.CodecType {
		case "video":
			st.Type = StreamVideo
			if s.Disposition["attached_pic"] == 1 {
				st.Type = StreamEmbeddedImage
			}
			st.Width, st.Height = s.Width, s.Height
			st.PixelFormat = s.PixFmt
			st.BitDepth = bitDepth(s.BitsPerRawSample, s.PixFmt)
			st.AspectRatio = s.DisplayAspectRatio
			st.AverageFrameRate, st.RealFrameRate = rate(s.AvgFrameRate), rate(s.RFrameRate)
			st.IsInterlaced = s.FieldOrder != "" && s.FieldOrder != "progressive" && s.FieldOrder != "unknown"
			st.ColorTransfer, st.ColorPrimaries, st.ColorSpace, st.ColorRange = s.ColorTransfer, s.ColorPrimaries, s.ColorSpace, s.ColorRange
			st.DoVi = doviRecord(s.SideDataList)
			if st.Type == StreamVideo {
				st.VideoRange, st.VideoRangeType = videoRange(st.ColorTransfer, st.DoVi)
			}
		case "audio":
			st.Type = StreamAudio
			st.Channels, st.ChannelLayout = s.Channels, s.ChannelLayout
			st.SampleRate = int(atoi64(s.SampleRate))
		case "subtitle":
			st.Type = StreamSubtitle
		case "attachment":
			info.Attachments++
			continue
		default:
			st.Type = StreamData
		}
		info.Streams = append(info.Streams, st)
	}
	for _, c := range raw.Chapters {
		info.Chapters = append(info.Chapters, Chapter{Start: seconds(c.StartTime), Title: tag(c.Tags, "title")})
	}
	return info, nil
}

// videoRange maps colour transfer and Dolby Vision signalling to Jellyfin's
// VideoRange/VideoRangeType. HDR10+ needs frame-level probing and is not
// detected (a DV + HDR10+ file reports DOVIWithHDR10).
func videoRange(transfer string, dv *DolbyVision) (string, string) {
	if dv != nil {
		switch {
		case dv.EL:
			return "HDR", "DOVIWithEL"
		case dv.BLSignalCompatibilityID == 1 || dv.BLSignalCompatibilityID == 6:
			return "HDR", "DOVIWithHDR10"
		case dv.BLSignalCompatibilityID == 4:
			return "HDR", "DOVIWithHLG"
		case dv.BLSignalCompatibilityID == 2:
			return "SDR", "DOVIWithSDR"
		default:
			return "HDR", "DOVI"
		}
	}
	switch transfer {
	case "smpte2084":
		return "HDR", "HDR10"
	case "arib-std-b67":
		return "HDR", "HLG"
	}
	return "SDR", "SDR"
}

func doviRecord(side []map[string]any) *DolbyVision {
	for _, sd := range side {
		if sd["side_data_type"] != "DOVI configuration record" {
			continue
		}
		n := func(k string) int { f, _ := sd[k].(float64); return int(f) }
		return &DolbyVision{
			Profile: n("dv_profile"), Level: n("dv_level"), BLSignalCompatibilityID: n("dv_bl_signal_compatibility_id"),
			VersionMajor: n("dv_version_major"), VersionMinor: n("dv_version_minor"),
			RPU: n("rpu_present_flag") == 1, EL: n("el_present_flag") == 1, BL: n("bl_present_flag") == 1,
		}
	}
	return nil
}

// bitDepth prefers bits_per_raw_sample and falls back to the pixel format
// ("yuv420p10le" → 10, "yuv420p" → 8).
func bitDepth(raw, pixFmt string) int {
	if n := int(atoi64(raw)); n > 0 {
		return n
	}
	for _, d := range []string{"16", "12", "10"} {
		if strings.Contains(pixFmt, "p"+d) {
			n, _ := strconv.Atoi(d)
			return n
		}
	}
	if pixFmt != "" {
		return 8
	}
	return 0
}

// tag reads a tag case-insensitively (MKV writes "BPS", MP4 "language", …).
func tag(tags map[string]string, key string) string {
	if v, ok := tags[key]; ok {
		return v
	}
	for k, v := range tags {
		if strings.EqualFold(k, key) {
			return v
		}
	}
	return ""
}

func seconds(s string) time.Duration {
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f < 0 {
		return 0
	}
	return time.Duration(f * float64(time.Second))
}

func atoi64(s string) int64 {
	n, _ := strconv.ParseInt(s, 10, 64)
	return n
}

// rate parses an ffprobe rational like "24000/1001"; "0/0" is 0.
func rate(r string) float64 {
	num, den, ok := strings.Cut(r, "/")
	if !ok {
		f, _ := strconv.ParseFloat(r, 64)
		return f
	}
	n, err1 := strconv.ParseFloat(num, 64)
	d, err2 := strconv.ParseFloat(den, 64)
	if err1 != nil || err2 != nil || d == 0 {
		return 0
	}
	return n / d
}
