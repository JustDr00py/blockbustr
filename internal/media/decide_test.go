package media

import (
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// browser is jollyrogarr's fixed rule set as a DeviceProfile: MP4 with
// H.264 and AAC/MP3 plays directly, anything else goes to H.264/AAC HLS.
var browser = DeviceProfile{
	DirectPlayProfiles:  []DirectPlayProfile{{Type: "Video", Container: "mp4", VideoCodec: "h264", AudioCodec: "aac,mp3"}},
	TranscodingProfiles: []TranscodingProfile{{Type: "Video", Container: "ts", VideoCodec: "h264", AudioCodec: "aac", Protocol: "hls"}},
}

// The cases ported from jollyrogarr's decide_test.go.
func TestDecideBrowserProfile(t *testing.T) {
	src := func(container, video, audio string) Source {
		s := Source{Container: container, Streams: []Stream{{Index: 0, Type: StreamVideo, Codec: video}}}
		if audio != "" {
			s.Streams = append(s.Streams, Stream{Index: 1, Type: StreamAudio, Codec: audio})
		}
		return s
	}
	for _, c := range []struct {
		name    string
		src     Source
		want    Mode
		reasons string
	}{
		{"mp4 h264 aac direct", src("mp4", "h264", "aac"), ModeDirect, ""},
		{"mp4 h264 no audio direct", src("mp4", "h264", ""), ModeDirect, ""},
		{"mkv h264 aac remux", src("mkv", "h264", "aac"), ModeRemux, "ContainerNotSupported"},
		{"mp4 hevc transcode", src("mp4", "hevc", "aac"), ModeTranscode, "VideoCodecNotSupported"},
		{"mkv av1 transcode", src("mkv", "av1", "aac"), ModeTranscode, "ContainerNotSupported,VideoCodecNotSupported"},
		{"mp4 h264 dts transcode", src("mp4", "h264", "dts"), ModeTranscode, "AudioCodecNotSupported"},
	} {
		got := Decide(browser, c.src, PlayOptions{})
		if got.Mode != c.want || strings.Join(got.Reasons, ",") != c.reasons {
			t.Errorf("%s: %s %v, want %s %s", c.name, got.Mode, got.Reasons, c.want, c.reasons)
		}
	}
}

func TestConditions(t *testing.T) {
	for _, c := range []struct {
		cond   ProfileCondition
		actual string
		want   bool
	}{
		{ProfileCondition{Condition: "LessThanEqual", Value: "153"}, "150", true},
		{ProfileCondition{Condition: "LessThanEqual", Value: "153"}, "156", false},
		{ProfileCondition{Condition: "NotEquals", Value: "DOVI", IsRequired: true}, "DOVIWithHDR10", true},
		{ProfileCondition{Condition: "NotEquals", Value: "DOVI", IsRequired: true}, "dovi", false},
		{ProfileCondition{Condition: "EqualsAny", Value: "high|main|baseline"}, "Main", true},
		{ProfileCondition{Condition: "EqualsAny", Value: "high|main"}, "Main 10", false},
		{ProfileCondition{Condition: "GreaterThanEqual", Value: "2"}, "6", true},
		{ProfileCondition{Condition: "Equals", Value: "false"}, "false", true},
		{ProfileCondition{Condition: "LessThanEqual", Value: "8"}, "", true},                    // unknown, not required
		{ProfileCondition{Condition: "LessThanEqual", Value: "8", IsRequired: true}, "", false}, // unknown, required
	} {
		if got := conditionHolds(c.cond, c.actual); got != c.want {
			t.Errorf("%+v on %q = %v", c.cond, c.actual, got)
		}
	}
}

// A codec profile condition blocks direct play with its reason, and the
// copy into the transcoding container is refused for the same stream.
func TestDecideCodecConditions(t *testing.T) {
	p := browser
	p.DirectPlayProfiles = []DirectPlayProfile{{Type: "Video", Container: "mp4,mkv", VideoCodec: "h264,hevc", AudioCodec: "aac"}}
	p.TranscodingProfiles = []TranscodingProfile{{Type: "Video", Container: "ts", VideoCodec: "h264,hevc", AudioCodec: "aac", Protocol: "hls"}}
	p.CodecProfiles = []CodecProfile{{Type: "Video", Codec: "hevc", Conditions: []ProfileCondition{
		{Condition: "NotEquals", Property: "VideoRangeType", Value: "DOVI", IsRequired: true},
		{Condition: "LessThanEqual", Property: "VideoBitDepth", Value: "8"},
	}}}
	src := Source{Container: "mkv", Streams: []Stream{
		{Index: 0, Type: StreamVideo, Codec: "hevc", VideoRangeType: "DOVI", BitDepth: 10},
		{Index: 1, Type: StreamAudio, Codec: "aac", Channels: 2},
	}}
	d := Decide(p, src, PlayOptions{})
	if d.Mode != ModeTranscode || strings.Join(d.Reasons, ",") != "VideoBitDepthNotSupported,VideoRangeTypeNotSupported" || d.Transcode.CopyVideo {
		t.Errorf("DOVI 10-bit HEVC: %s %v %+v", d.Mode, d.Reasons, d.Transcode)
	}
	src.Streams[0].VideoRangeType, src.Streams[0].BitDepth = "HDR10", 8
	if d := Decide(p, src, PlayOptions{}); d.Mode != ModeDirect || d.Container != "mkv" {
		t.Errorf("8-bit HDR10 HEVC: %s %v", d.Mode, d.Reasons)
	}
}

func TestDecideOptions(t *testing.T) {
	src := Source{Container: "mp4", Bitrate: 5_000_000, Streams: []Stream{
		{Index: 0, Type: StreamVideo, Codec: "h264", Bitrate: 4_800_000, RealFrameRate: 23.976},
		{Index: 1, Type: StreamAudio, Codec: "aac", Channels: 2, Bitrate: 128_000, IsDefault: true},
		{Index: 2, Type: StreamAudio, Codec: "truehd", Channels: 8},
		{Index: 3, Type: StreamSubtitle, Codec: "hdmv_pgs_subtitle"},
		{Index: 4, Type: StreamSubtitle, Codec: "subrip"},
	}}
	p := browser
	p.SubtitleProfiles = []SubtitleProfile{{Format: "srt", Method: "External"}, {Format: "vtt", Method: "Hls"}}
	idx := func(n int) *int { return &n }

	d := Decide(p, src, PlayOptions{})
	if d.Mode != ModeDirect || d.Subtitles[3] != "Encode" || d.Subtitles[4] != "External" {
		t.Errorf("default tracks: %+v", d)
	}
	// Choosing the TrueHD track: audio isn't playable, and isn't encodable
	// as is, so it's transcoded to 8-channel AAC.
	if d := Decide(p, src, PlayOptions{AudioStreamIndex: idx(2)}); d.Mode != ModeTranscode ||
		strings.Join(d.Reasons, ",") != "AudioCodecNotSupported" || d.Transcode.AudioBitrate != 384_000 {
		t.Errorf("truehd: %s %v %+v", d.Mode, d.Reasons, d.Transcode)
	}
	// An image subtitle the client can't take is burned in.
	if d := Decide(p, src, PlayOptions{SubtitleStreamIndex: idx(3)}); d.Mode != ModeTranscode || !strings.Contains(strings.Join(d.Reasons, ","), "SubtitleCodecNotSupported") {
		t.Errorf("pgs: %s %v", d.Mode, d.Reasons)
	}
	if d := Decide(p, src, PlayOptions{SubtitleStreamIndex: idx(-1)}); d.Mode != ModeDirect {
		t.Errorf("no subtitles: %s", d.Mode)
	}
	// A sidecar file can't be embedded, and Hls isn't served: burn it in
	// unless the client takes it as a file.
	ext := src
	ext.Streams = append(append([]Stream(nil), src.Streams...), Stream{Index: 5, Type: StreamSubtitle, Codec: "ass", IsExternal: true})
	pe := p
	pe.SubtitleProfiles = []SubtitleProfile{{Format: "ass", Method: "Embed"}, {Format: "vtt", Method: "Hls"}}
	if d := Decide(pe, ext, PlayOptions{SubtitleStreamIndex: idx(5)}); d.Subtitles[5] != "Encode" || d.Subtitles[4] != "Encode" || d.Mode != ModeTranscode {
		t.Errorf("external ASS with Embed only: %s %v", d.Mode, d.Subtitles)
	}
	pe.SubtitleProfiles = append(pe.SubtitleProfiles, SubtitleProfile{Format: "ass", Method: "External"})
	if d := Decide(pe, ext, PlayOptions{SubtitleStreamIndex: idx(5)}); d.Subtitles[5] != "External" || d.Mode != ModeDirect {
		t.Errorf("external ASS as a file: %s %v", d.Mode, d.Subtitles)
	}
	// Over the limit: transcode at limit minus the copied audio.
	d = Decide(p, src, PlayOptions{MaxBitrate: 3_000_000})
	if d.Mode != ModeTranscode || strings.Join(d.Reasons, ",") != "ContainerBitrateExceedsLimit" ||
		d.Transcode.VideoBitrate != 2_872_000 || d.Transcode.AudioBitrate != 128_000 || d.Transcode.MaxFramerate != 23.976 {
		t.Errorf("over limit: %s %v %+v", d.Mode, d.Reasons, d.Transcode)
	}
	if d := Decide(p, src, PlayOptions{DisableDirectPlay: true}); d.Mode != ModeTranscode || d.SupportsDirectPlay {
		t.Errorf("direct play disabled: %s", d.Mode)
	}
	if d := Decide(DeviceProfile{}, src, PlayOptions{}); d.Mode != ModeNone || d.SupportsDirectPlay || d.SupportsDirectStream || d.SupportsTranscoding {
		t.Errorf("empty profile: %+v", d)
	}
}

// capturedPlaybackInfo is one recorded PlaybackInfo exchange.
type capturedPlaybackInfo struct {
	Request struct {
		Query map[string][]string `json:"query"`
		Body  map[string]json.RawMessage
	} `json:"request"`
	Response struct {
		Body struct {
			MediaSources []struct {
				Container                                                     string
				Bitrate                                                       int64
				IsRemote                                                      bool
				SupportsDirectPlay, SupportsDirectStream, SupportsTranscoding bool
				TranscodingUrl                                                string
				MediaStreams                                                  []struct {
					Index                                int
					Type, Codec, Profile, VideoRangeType string
					DeliveryMethod                       string
					Level, RealFrameRate                 float64
					BitDepth, Width, Height, Channels    int
					SampleRate                           int
					BitRate                              int64
					IsInterlaced, IsDefault              bool
				}
			}
		} `json:"body"`
	} `json:"response"`
}

// field finds a body key case-insensitively (Streamyfin sends camelCase).
func (c capturedPlaybackInfo) field(name string, v any) bool {
	for k, raw := range c.Request.Body {
		if strings.EqualFold(k, name) {
			return json.Unmarshal(raw, v) == nil
		}
	}
	return false
}

// Every captured PlaybackInfo replays to Jellyfin 12.1.0's decision: flags,
// container name, transcode reasons and target, subtitle delivery.
func TestDecideMatchesJellyfin(t *testing.T) {
	files, _ := filepath.Glob("../../testdata/jellyfin/*/*/[0-9]*-POST-Items-id-PlaybackInfo.json")
	if len(files) == 0 {
		t.Fatal("no PlaybackInfo captures")
	}
	var sources, transcodes, subtitles, remote int
	defer func() {
		t.Logf("compared %d sources (%d transcodes, %d subtitle streams); %d remote over-limit divergences", sources, transcodes, subtitles, remote)
		if sources < 25 || transcodes < 2 || subtitles < 50 {
			t.Errorf("too few comparisons")
		}
	}()
	for _, f := range files {
		name := strings.TrimPrefix(f, "../../testdata/jellyfin/")
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var c capturedPlaybackInfo
		if err := json.Unmarshal(raw, &c); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var p DeviceProfile
		if !c.field("DeviceProfile", &p) {
			t.Fatalf("%s: no DeviceProfile", name)
		}
		var o PlayOptions
		c.field("MaxStreamingBitrate", &o.MaxBitrate)
		var a, s int
		if c.field("AudioStreamIndex", &a) {
			o.AudioStreamIndex = &a
		}
		if c.field("SubtitleStreamIndex", &s) {
			o.SubtitleStreamIndex = &s
		}
		for _, ms := range c.Response.Body.MediaSources {
			src := Source{Container: ms.Container, Bitrate: ms.Bitrate}
			if src.Container == "mov" {
				src.Container = "mp4" // stored name; "mov" is what Streamyfin's profile calls it
			}
			for _, st := range ms.MediaStreams {
				src.Streams = append(src.Streams, Stream{Index: st.Index, Type: StreamType(st.Type), Codec: st.Codec,
					Profile: st.Profile, Level: int(st.Level), VideoRangeType: st.VideoRangeType, RealFrameRate: st.RealFrameRate,
					BitDepth: st.BitDepth, Width: st.Width, Height: st.Height, Channels: st.Channels, SampleRate: st.SampleRate,
					Bitrate: st.BitRate, IsInterlaced: st.IsInterlaced, IsDefault: st.IsDefault})
			}
			d := Decide(p, src, o)
			sources++

			// Intentional divergence (DESIGN §8.3a, TASKS P2.4): Jellyfin
			// ignores the bitrate limit for remote (.strm) sources.
			if ms.IsRemote && o.MaxBitrate > 0 && ms.Bitrate > o.MaxBitrate {
				if d.Mode != ModeTranscode || !strings.Contains(strings.Join(d.Reasons, ","), "ContainerBitrateExceedsLimit") {
					t.Errorf("%s: remote source over the limit must transcode: %s %v", name, d.Mode, d.Reasons)
				}
				remote++
				continue
			}
			if d.SupportsDirectPlay != ms.SupportsDirectPlay || d.SupportsDirectStream != ms.SupportsDirectStream || d.SupportsTranscoding != ms.SupportsTranscoding {
				t.Errorf("%s: DP/DS/TC %v/%v/%v, Jellyfin %v/%v/%v (%s %v)", name, d.SupportsDirectPlay, d.SupportsDirectStream,
					d.SupportsTranscoding, ms.SupportsDirectPlay, ms.SupportsDirectStream, ms.SupportsTranscoding, d.Mode, d.Reasons)
			}
			if d.Container != ms.Container {
				t.Errorf("%s: container %q, Jellyfin %q", name, d.Container, ms.Container)
			}
			for _, st := range ms.MediaStreams {
				if st.Type == "Subtitle" {
					subtitles++
				}
				if st.Type == "Subtitle" && d.Subtitles[st.Index] != st.DeliveryMethod {
					t.Errorf("%s: subtitle %d (%s) %q, Jellyfin %q", name, st.Index, st.Codec, d.Subtitles[st.Index], st.DeliveryMethod)
				}
			}
			if ms.TranscodingUrl == "" {
				continue
			}
			u, err := url.Parse(ms.TranscodingUrl)
			if err != nil || d.Transcode == nil {
				t.Errorf("%s: transcode %+v, Jellyfin %s", name, d.Transcode, ms.TranscodingUrl)
				continue
			}
			q := u.Query()
			tc := d.Transcode
			transcodes++
			got := map[string]string{
				"TranscodeReasons": strings.Join(d.Reasons, ","), "VideoCodec": strings.Join(tc.VideoCodecs, ","),
				"AudioCodec": strings.Join(tc.AudioCodecs, ","), "SegmentContainer": tc.Container,
				"VideoBitrate": strconv.FormatInt(tc.VideoBitrate, 10), "AudioBitrate": strconv.FormatInt(tc.AudioBitrate, 10),
				"MaxFramerate": strconv.FormatFloat(tc.MaxFramerate, 'f', -1, 64),
			}
			if tc.MaxAudioChannels > 0 {
				got["TranscodingMaxAudioChannels"] = strconv.Itoa(tc.MaxAudioChannels)
			}
			for k, v := range got {
				if q.Get(k) != v {
					t.Errorf("%s: %s=%q, Jellyfin %q", name, k, v, q.Get(k))
				}
			}
			if q.Has("TranscodingMaxAudioChannels") && got["TranscodingMaxAudioChannels"] == "" {
				t.Errorf("%s: Jellyfin limits audio channels to %s", name, q.Get("TranscodingMaxAudioChannels"))
			}
		}
	}
}

// TASKS P2.4: the limit applies to remote sources too, and an unknown
// remote bitrate counts as UnknownRemoteBitrate.
func TestDecideRemoteBitrate(t *testing.T) {
	src := Source{Container: "mp4", Remote: true, Streams: []Stream{
		{Index: 0, Type: StreamVideo, Codec: "h264"}, {Index: 1, Type: StreamAudio, Codec: "aac"},
	}}
	for _, c := range []struct {
		bitrate, limit int64
		want           Mode
	}{
		{0, 10_000_000, ModeTranscode}, // unknown: assume it doesn't fit a phone's limit
		{0, 120_000_000, ModeDirect},   // "maximum" settings still play directly
		{0, 0, ModeDirect},             // no limit at all
		{7_900_000, 8_000_000, ModeDirect},
		{7_900_000, 4_000_000, ModeTranscode}, // Jellyfin would direct play this (§8.3a)
	} {
		src.Bitrate = c.bitrate
		if d := Decide(browser, src, PlayOptions{MaxBitrate: c.limit}); d.Mode != c.want {
			t.Errorf("remote %d at limit %d: %s %v, want %s", c.bitrate, c.limit, d.Mode, d.Reasons, c.want)
		}
	}
	src.Remote, src.Bitrate = false, 0
	if d := Decide(browser, src, PlayOptions{MaxBitrate: 10_000_000}); d.Mode != ModeDirect {
		t.Errorf("a local file of unknown bitrate isn't assumed over the limit: %s", d.Mode)
	}
}

// An embedded text track of a remote file is offered as a file only once
// it is selected: extracting it reads the whole file over the network,
// and some apps fetch every External track at start (Streamyfin did,
// 2026-10-07, with no subtitle selected).
func TestDecideRemoteEmbeddedSubtitles(t *testing.T) {
	src := Source{Container: "mkv", Remote: true, Bitrate: 5_000_000, Streams: []Stream{
		{Index: 0, Type: StreamVideo, Codec: "h264", Bitrate: 4_800_000},
		{Index: 1, Type: StreamAudio, Codec: "aac", Channels: 2, IsDefault: true},
		{Index: 2, Type: StreamSubtitle, Codec: "subrip"},
		{Index: 3, Type: StreamSubtitle, Codec: "subrip"},
		{Index: 100, Type: StreamSubtitle, Codec: "subrip", IsExternal: true}, // an addon's file
	}}
	p := browser
	p.SubtitleProfiles = []SubtitleProfile{{Format: "srt", Method: "External"}}
	idx := func(n int) *int { return &n }

	d := Decide(p, src, PlayOptions{})
	if d.Subtitles[2] != "Encode" || d.Subtitles[3] != "Encode" || d.Subtitles[100] != "External" {
		t.Errorf("nothing selected: %v", d.Subtitles)
	}
	d = Decide(p, src, PlayOptions{SubtitleStreamIndex: idx(3)})
	if d.Subtitles[3] != "External" || d.Subtitles[2] != "Encode" {
		t.Errorf("track 3 selected: %s %v", d.Mode, d.Subtitles)
	}
	local := src
	local.Remote = false
	if d := Decide(p, local, PlayOptions{}); d.Subtitles[2] != "External" || d.Subtitles[3] != "External" {
		t.Errorf("a local file's tracks stay files: %v", d.Subtitles)
	}
}

// With the video encoder ruled out (a user's policy, or no encoder the
// server may use), a source still plays when its video can be copied;
// one that needs its video re-encoded doesn't.
func TestDecideNoVideoEncode(t *testing.T) {
	idx := func(n int) *int { return &n }
	src := Source{Container: "mp4", Bitrate: 5_000_000, Streams: []Stream{
		{Index: 0, Type: StreamVideo, Codec: "h264", Bitrate: 4_800_000, RealFrameRate: 23.976},
		{Index: 1, Type: StreamAudio, Codec: "aac", Channels: 2, Bitrate: 128_000, IsDefault: true},
		{Index: 2, Type: StreamAudio, Codec: "truehd", Channels: 8},
		{Index: 3, Type: StreamSubtitle, Codec: "hdmv_pgs_subtitle"},
	}}
	no := PlayOptions{NoVideoEncode: true}
	if d := Decide(browser, src, no); d.Mode != ModeDirect {
		t.Errorf("playable as is: %s", d.Mode)
	}
	// TrueHD: the video is copied, only the audio converted.
	no.AudioStreamIndex = idx(2)
	if d := Decide(browser, src, no); d.Mode != ModeRemux || !d.SupportsTranscoding || d.SupportsDirectStream ||
		d.Transcode == nil || !d.Transcode.CopyVideo || strings.Join(d.Reasons, ",") != "AudioCodecNotSupported" {
		t.Errorf("truehd without the video encoder: %s %+v %v", d.Mode, d.Transcode, d.Reasons)
	}
	// Without the restriction it's a full transcode, as Jellyfin does.
	if d := Decide(browser, src, PlayOptions{AudioStreamIndex: idx(2)}); d.Mode != ModeTranscode {
		t.Errorf("truehd: %s", d.Mode)
	}
	// The client turning transcoding off rules out converting audio too.
	if d := Decide(browser, src, PlayOptions{AudioStreamIndex: idx(2), DisableTranscoding: true, NoVideoEncode: true}); d.Mode != ModeNone {
		t.Errorf("truehd, client disabled transcoding: %s", d.Mode)
	}
	// Burning in a subtitle needs the video re-encoded.
	if d := Decide(browser, src, PlayOptions{SubtitleStreamIndex: idx(3), NoVideoEncode: true}); d.Mode != ModeNone || d.SupportsTranscoding {
		t.Errorf("pgs burn-in without the video encoder: %s", d.Mode)
	}
	// A codec the client can't take, too.
	hevc := src
	hevc.Streams = append([]Stream{{Index: 0, Type: StreamVideo, Codec: "hevc", Bitrate: 4_800_000}}, src.Streams[1:]...)
	if d := Decide(browser, hevc, PlayOptions{NoVideoEncode: true}); d.Mode != ModeNone {
		t.Errorf("hevc to a browser without the video encoder: %s", d.Mode)
	}

	// No CPU filters: a transcode is fine, unless it tonemaps or burns in.
	cpu := PlayOptions{NoCPUFilters: true}
	if d := Decide(browser, hevc, cpu); d.Mode != ModeTranscode {
		t.Errorf("SDR hevc, no CPU filters: %s", d.Mode)
	}
	hdr := hevc
	hdr.Streams = append([]Stream{{Index: 0, Type: StreamVideo, Codec: "hevc", Bitrate: 4_800_000, VideoRangeType: "HDR10"}}, src.Streams[1:]...)
	if d := Decide(browser, hdr, cpu); d.Mode != ModeNone {
		t.Errorf("HDR hevc, no CPU filters: %s", d.Mode)
	}
	cpu.SubtitleStreamIndex = idx(3)
	if d := Decide(browser, src, cpu); d.Mode != ModeNone {
		t.Errorf("pgs burn-in, no CPU filters: %s", d.Mode)
	}
}
