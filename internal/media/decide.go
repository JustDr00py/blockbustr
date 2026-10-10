package media

import (
	"slices"
	"strconv"
	"strings"
)

// Playback decision (TASKS P2.1, DESIGN §8.1), ported from jollyrogarr's
// decide.go and driven by the client's Jellyfin DeviceProfile instead of a
// fixed browser codec list. It answers what PlaybackInfo reports for a
// source: whether the client can direct play it, direct stream it (remux:
// video copied into the transcoding container), or needs a transcode — with
// Jellyfin's TranscodeReasons, the transcode target, and how each subtitle
// stream is delivered. Rules not visible in the OpenAPI spec are matched to
// Jellyfin 12.1.0's answers in the captures (decide_test.go).
//
// The profile types mirror Jellyfin's DeviceProfile JSON, so a request body
// decodes straight into them; media must not depend on jfapi (AGENTS.md).

// Mode is the playback strategy chosen for a source.
type Mode string

const (
	ModeDirect    Mode = "direct"    // the client plays the file as it is
	ModeRemux     Mode = "remux"     // video copied, container (and maybe audio) changed
	ModeTranscode Mode = "transcode" // video re-encoded
	ModeNone      Mode = "none"      // nothing the client accepts
)

// DeviceProfile is the part of Jellyfin's DeviceProfile the decision reads.
type DeviceProfile struct {
	Name                string
	MaxStreamingBitrate int64
	MaxStaticBitrate    int64
	DirectPlayProfiles  []DirectPlayProfile
	TranscodingProfiles []TranscodingProfile
	ContainerProfiles   []ContainerProfile
	CodecProfiles       []CodecProfile
	SubtitleProfiles    []SubtitleProfile
}

// DirectPlayProfile lists containers and codecs (comma-separated; empty =
// any) the client plays natively.
type DirectPlayProfile struct {
	Type, Container, VideoCodec, AudioCodec string
}

// TranscodingProfile is a format the client accepts a transcode in.
type TranscodingProfile struct {
	Type, Container, VideoCodec, AudioCodec, Protocol, Context string
	MaxAudioChannels                                           string
	Conditions                                                 []ProfileCondition
}

// ContainerProfile adds conditions for a container.
type ContainerProfile struct {
	Type, Container string
	Conditions      []ProfileCondition
}

// CodecProfile adds conditions for codecs (Type Video, VideoAudio or Audio);
// it applies when its ApplyConditions hold.
type CodecProfile struct {
	Type, Codec, Container      string
	Conditions, ApplyConditions []ProfileCondition
}

// ProfileCondition compares a stream property with a value. A condition on
// an unknown property fails only when IsRequired.
type ProfileCondition struct {
	Condition, Property, Value string
	IsRequired                 bool
}

// SubtitleProfile says how the client takes a subtitle format: Embed (in the
// stream), External (separate file), Hls (in the HLS playlist), Encode.
type SubtitleProfile struct {
	Format, Method, Container string
}

// Source is a media source to decide on.
type Source struct {
	Container string // short name as stored: "mkv", "mp4", "ts", …
	Bitrate   int64  // container bits/s; 0 = unknown
	Remote    bool   // fetched over the network (.strm, debrid, addon)
	Streams   []Stream
}

// UnknownRemoteBitrate is what a remote source of unknown bitrate is
// assumed to need (TASKS P2.4): about a 4K remux, so a capped client gets a
// transcode instead of a stall, while "maximum" settings (≥ 120 Mbps)
// still play it directly. Jellyfin ignores the cap for remote sources.
const UnknownRemoteBitrate = 80_000_000

// PlayOptions are the request's playback options.
type PlayOptions struct {
	MaxBitrate          int64 // 0 = the profile's MaxStreamingBitrate
	AudioStreamIndex    *int  // nil = default audio stream
	SubtitleStreamIndex *int  // nil or -1 = none
	DisableDirectPlay   bool
	DisableDirectStream bool
	DisableTranscoding  bool
	// NoVideoEncode: the video may not be re-encoded (the user's policy, or
	// no encoder this server may use). Copying it while the container or
	// audio changes is still offered, as a remux.
	NoVideoEncode bool
	// NoCPUFilters rules out re-encoding that needs video work on the CPU
	// before the encoder: tonemapping an HDR source, burning in the chosen
	// subtitle (transcode.software off).
	NoCPUFilters bool
}

// Transcode is the target of a remux or transcode (what the HLS URL asks for).
type Transcode struct {
	Container        string   // segment container: "ts", "mp4"
	Protocol         string   // "hls" or "http"
	VideoCodecs      []string // acceptable output codecs, preferred first
	AudioCodecs      []string
	MaxAudioChannels int // 0 = no limit
	VideoBitrate     int64
	AudioBitrate     int64
	MaxFramerate     float64
	CopyVideo        bool
}

// Decision is what to do with a source for one client.
type Decision struct {
	Mode                 Mode
	SupportsDirectPlay   bool
	SupportsDirectStream bool
	SupportsTranscoding  bool
	// Reasons are Jellyfin TranscodeReason names, in its enum order; empty
	// for direct play.
	Reasons []string
	// Container is the source container as the client names it ("mov" for
	// an MP4 to a client that lists mov before mp4); the stored name when no
	// profile matched.
	Container string
	Transcode *Transcode
	// Subtitles maps every subtitle stream index to its delivery method.
	Subtitles map[int]string
}

// reasonOrder is Jellyfin's TranscodeReason enum order (12.1.0 OpenAPI);
// reasons are reported in it.
var reasonOrder = []string{
	"ContainerNotSupported", "VideoCodecNotSupported", "AudioCodecNotSupported", "SubtitleCodecNotSupported",
	"AudioIsExternal", "SecondaryAudioNotSupported", "VideoProfileNotSupported", "VideoLevelNotSupported",
	"VideoResolutionNotSupported", "VideoBitDepthNotSupported", "VideoFramerateNotSupported", "RefFramesNotSupported",
	"AnamorphicVideoNotSupported", "InterlacedVideoNotSupported", "AudioChannelsNotSupported", "AudioProfileNotSupported",
	"AudioSampleRateNotSupported", "AudioBitDepthNotSupported", "ContainerBitrateExceedsLimit", "VideoBitrateNotSupported",
	"AudioBitrateNotSupported", "UnknownVideoStreamInfo", "UnknownAudioStreamInfo", "DirectPlayError",
	"VideoRangeTypeNotSupported", "VideoCodecTagNotSupported", "StreamCountExceedsLimit", "VideoRotationNotSupported",
}

// reasonFor is the TranscodeReason a failed condition on a property gives.
var reasonFor = map[string]string{
	"videolevel": "VideoLevelNotSupported", "videoprofile": "VideoProfileNotSupported",
	"videorangetype": "VideoRangeTypeNotSupported", "videobitdepth": "VideoBitDepthNotSupported",
	"width": "VideoResolutionNotSupported", "height": "VideoResolutionNotSupported",
	"videoframerate": "VideoFramerateNotSupported", "refframes": "RefFramesNotSupported",
	"isanamorphic": "AnamorphicVideoNotSupported", "isinterlaced": "InterlacedVideoNotSupported",
	"videobitrate": "VideoBitrateNotSupported", "isavc": "VideoCodecNotSupported",
	"videocodectag": "VideoCodecTagNotSupported", "videorotation": "VideoRotationNotSupported",
	"audiochannels": "AudioChannelsNotSupported", "audioprofile": "AudioProfileNotSupported",
	"audiosamplerate": "AudioSampleRateNotSupported", "audiobitdepth": "AudioBitDepthNotSupported",
	"audiobitrate": "AudioBitrateNotSupported", "issecondaryaudio": "SecondaryAudioNotSupported",
	"numaudiostreams": "StreamCountExceedsLimit", "numvideostreams": "StreamCountExceedsLimit",
	"numstreams": "StreamCountExceedsLimit",
}

// containerAliases are the names a stored container answers to, in
// ffprobe's order (Jellyfin reports the first one the client lists).
var containerAliases = map[string][]string{
	"mkv": {"mkv", "matroska"},
	"mp4": {"mov", "mp4", "m4a", "3gp", "3g2", "mj2"},
	"ts":  {"ts", "mpegts", "m2ts"},
}

// codecAliases groups names clients and ffprobe use for the same codec.
var codecAliases = [][]string{
	{"hevc", "h265"}, {"h264", "avc"}, {"mpeg2video", "mpeg2"}, {"dts", "dca"},
	{"subrip", "srt"}, {"pgssub", "pgs", "hdmv_pgs_subtitle"}, {"webvtt", "vtt"},
	{"dvdsub", "dvd_subtitle", "vobsub"}, {"dvbsub", "dvb_subtitle"},
}

// Encoders this server can transcode to; a transcoding profile's codecs are
// filtered to these (observed: Jellyfin drops dts, mp1/mp2 and truehd).
var (
	videoEncoders = []string{"h264", "hevc"}
	audioEncoders = []string{"aac", "mp3", "ac3", "eac3", "opus", "flac", "vorbis"}
)

func canonicalCodec(c string) string {
	c = strings.ToLower(strings.TrimSpace(c))
	for _, group := range codecAliases {
		if slices.Contains(group, c) {
			return group[0]
		}
	}
	return c
}

func splitList(s string) []string {
	var out []string
	for _, v := range strings.Split(s, ",") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// CodecIn reports whether a comma-separated codec list (as in a
// DeviceProfile or a transcoding URL) names codec, alias-aware; an empty
// list accepts anything.
func CodecIn(list, codec string) bool { return listHasCodec(list, codec) }

// listHasCodec: an empty list accepts any codec; "pcm" covers pcm_*.
func listHasCodec(list, codec string) bool {
	items := splitList(list)
	if len(items) == 0 {
		return true
	}
	c := canonicalCodec(codec)
	for _, it := range items {
		it = canonicalCodec(it)
		if it == c || (it == "pcm" && strings.HasPrefix(c, "pcm_")) {
			return true
		}
	}
	return false
}

// matchContainer returns the alias of container that list names (empty
// list = any, answered with the first alias).
func matchContainer(list, container string) (string, bool) {
	aliases := containerAliases[strings.ToLower(container)]
	if aliases == nil {
		aliases = []string{strings.ToLower(container)}
	}
	items := splitList(strings.ToLower(list))
	if len(items) == 0 {
		return aliases[0], true
	}
	for _, a := range aliases {
		if slices.Contains(items, a) {
			return a, true
		}
	}
	return "", false
}

func streamOf(src Source, typ StreamType, index *int) *Stream {
	var first, def *Stream
	for i := range src.Streams {
		s := &src.Streams[i]
		if s.Type != typ {
			continue
		}
		if index != nil && s.Index == *index {
			return s
		}
		if first == nil {
			first = s
		}
		if def == nil && s.IsDefault {
			def = s
		}
	}
	if index != nil && typ == StreamSubtitle {
		return nil // an explicit subtitle index that doesn't exist: none
	}
	if def != nil {
		return def
	}
	return first
}

// property is the value of a condition property for the chosen streams, or
// "" when unknown.
func property(name string, src Source, video, audio *Stream) string {
	itoa := func(n int) string {
		if n == 0 {
			return ""
		}
		return strconv.Itoa(n)
	}
	count := func(t StreamType) string {
		n := 0
		for _, s := range src.Streams {
			if t == "" || s.Type == t {
				n++
			}
		}
		return strconv.Itoa(n)
	}
	switch strings.ToLower(name) {
	case "numaudiostreams":
		return count(StreamAudio)
	case "numvideostreams":
		return count(StreamVideo)
	case "numstreams":
		return count("")
	}
	if v := video; v != nil {
		switch strings.ToLower(name) {
		case "videolevel":
			return itoa(v.Level)
		case "videoprofile":
			return v.Profile
		case "videorangetype":
			return v.VideoRangeType
		case "videobitdepth":
			return itoa(v.BitDepth)
		case "width":
			return itoa(v.Width)
		case "height":
			return itoa(v.Height)
		case "videoframerate":
			if v.RealFrameRate > 0 {
				return strconv.FormatFloat(v.RealFrameRate, 'f', -1, 64)
			}
			return ""
		case "videobitrate":
			if v.Bitrate > 0 {
				return strconv.FormatInt(v.Bitrate, 10)
			}
			return ""
		case "isinterlaced":
			return strconv.FormatBool(v.IsInterlaced)
		case "isavc":
			return strconv.FormatBool(canonicalCodec(v.Codec) == "h264")
		}
	}
	if a := audio; a != nil {
		switch strings.ToLower(name) {
		case "audiochannels":
			return itoa(a.Channels)
		case "audioprofile":
			return a.Profile
		case "audiosamplerate":
			return itoa(a.SampleRate)
		case "audiobitrate":
			if a.Bitrate > 0 {
				return strconv.FormatInt(a.Bitrate, 10)
			}
			return ""
		case "issecondaryaudio":
			return "false" // the chosen track is always the one played
		}
	}
	return ""
}

// conditionHolds evaluates one condition. Numbers compare numerically,
// everything else case-insensitively; EqualsAny takes "|"-separated values.
func conditionHolds(c ProfileCondition, actual string) bool {
	if actual == "" {
		return !c.IsRequired
	}
	a, aErr := strconv.ParseFloat(actual, 64)
	cmp := func(want string) int {
		if w, err := strconv.ParseFloat(want, 64); err == nil && aErr == nil {
			switch {
			case a < w:
				return -1
			case a > w:
				return 1
			}
			return 0
		}
		return strings.Compare(strings.ToLower(actual), strings.ToLower(want))
	}
	switch strings.ToLower(c.Condition) {
	case "equals":
		return cmp(c.Value) == 0
	case "notequals":
		return cmp(c.Value) != 0
	case "lessthanequal":
		return cmp(c.Value) <= 0
	case "greaterthanequal":
		return cmp(c.Value) >= 0
	case "equalsany":
		for _, v := range strings.Split(c.Value, "|") {
			if cmp(strings.TrimSpace(v)) == 0 {
				return true
			}
		}
		return false
	}
	return true // unknown condition kinds don't block playback
}

// failedConditions returns the reasons of conditions that don't hold.
func failedConditions(conds []ProfileCondition, src Source, video, audio *Stream) []string {
	var out []string
	for _, c := range conds {
		if !conditionHolds(c, property(c.Property, src, video, audio)) {
			if r := reasonFor[strings.ToLower(c.Property)]; r != "" {
				out = append(out, r)
			} else {
				out = append(out, "DirectPlayError")
			}
		}
	}
	return out
}

func allHold(conds []ProfileCondition, src Source, video, audio *Stream) bool {
	return len(failedConditions(conds, src, video, audio)) == 0
}

// codecConditions checks the CodecProfiles that apply to the video and
// audio stream in container.
func codecConditions(p DeviceProfile, src Source, container string, video, audio *Stream) []string {
	var out []string
	for _, cp := range p.CodecProfiles {
		var s *Stream
		switch strings.ToLower(cp.Type) {
		case "video":
			s = video
		case "videoaudio", "audio":
			s = audio
		}
		if s == nil || !listHasCodec(cp.Codec, s.Codec) {
			continue
		}
		if cp.Container != "" {
			if _, ok := matchContainer(cp.Container, container); !ok {
				continue
			}
		}
		if !allHold(cp.ApplyConditions, src, video, audio) {
			continue
		}
		out = append(out, failedConditions(cp.Conditions, src, video, audio)...)
	}
	return out
}

// directPlayReasons is why src can't be direct played (none = it can), and
// the container alias the matching profile used.
func directPlayReasons(p DeviceProfile, src Source, video, audio *Stream) ([]string, string) {
	var reasons []string
	containerOK, videoOK, audioOK := false, false, false
	alias := ""
	for _, dp := range p.DirectPlayProfiles {
		if !strings.EqualFold(dp.Type, "Video") {
			continue
		}
		a, ok := matchContainer(dp.Container, src.Container)
		if !ok {
			continue
		}
		containerOK = true
		v := video == nil || listHasCodec(dp.VideoCodec, video.Codec)
		au := audio == nil || listHasCodec(dp.AudioCodec, audio.Codec)
		videoOK, audioOK = videoOK || v, audioOK || au
		if v && au && alias == "" {
			alias = a
		}
	}
	switch {
	case !containerOK:
		reasons = append(reasons, "ContainerNotSupported")
	case alias == "":
		if !videoOK {
			reasons = append(reasons, "VideoCodecNotSupported")
		}
		if !audioOK {
			reasons = append(reasons, "AudioCodecNotSupported")
		}
	}
	for _, cp := range p.ContainerProfiles {
		if !strings.EqualFold(cp.Type, "Video") {
			continue
		}
		if _, ok := matchContainer(cp.Container, src.Container); ok {
			reasons = append(reasons, failedConditions(cp.Conditions, src, video, audio)...)
		}
	}
	reasons = append(reasons, codecConditions(p, src, src.Container, video, audio)...)
	return reasons, alias
}

// subtitleMethod is how one subtitle stream reaches the client: embedded
// when the file is played or remuxed (and the stream is in the file), else
// as a separate file (text only), else burned in. Jellyfin also offers Hls
// (subtitle renditions in the HLS playlist) where a profile asks for it;
// blockbustr doesn't serve those yet, so such streams are burned in, and no
// captured client asks for Hls.
func subtitleMethod(p DeviceProfile, s Stream, direct bool) string {
	has := func(method string) bool {
		for _, sp := range p.SubtitleProfiles {
			if strings.EqualFold(sp.Method, method) && canonicalCodec(sp.Format) == canonicalCodec(s.Codec) {
				return true
			}
		}
		return false
	}
	text := s.IsTextSubtitle() || canonicalCodec(s.Codec) == "subrip" || canonicalCodec(s.Codec) == "webvtt"
	switch {
	case direct && !s.IsExternal && has("Embed"):
		return "Embed"
	case text && has("External"):
		return "External"
	}
	return "Encode"
}

func sortReasons(rs []string) []string {
	var out []string
	for _, r := range reasonOrder {
		if slices.Contains(rs, r) {
			out = append(out, r)
		}
	}
	return out
}

// Decide chooses how src plays on the client described by p.
func Decide(p DeviceProfile, src Source, o PlayOptions) Decision {
	video := streamOf(src, StreamVideo, nil)
	audio := streamOf(src, StreamAudio, o.AudioStreamIndex)
	var sub *Stream
	if o.SubtitleStreamIndex != nil && *o.SubtitleStreamIndex >= 0 {
		sub = streamOf(src, StreamSubtitle, o.SubtitleStreamIndex)
	}
	limit := o.MaxBitrate
	if limit <= 0 {
		limit = p.MaxStreamingBitrate
	}
	bitrate := src.Bitrate
	if bitrate <= 0 && src.Remote {
		bitrate = UnknownRemoteBitrate
	}
	overLimit := limit > 0 && bitrate > limit

	d := Decision{Container: src.Container, Subtitles: map[int]string{}}

	// Direct play: a profile takes container and codecs, conditions hold,
	// the bitrate fits and the chosen subtitle needn't be burned in.
	dpReasons, alias := directPlayReasons(p, src, video, audio)
	if alias != "" {
		d.Container = alias
	}
	if overLimit {
		dpReasons = append(dpReasons, "ContainerBitrateExceedsLimit")
	}
	if sub != nil && subtitleMethod(p, *sub, true) == "Encode" {
		dpReasons = append(dpReasons, "SubtitleCodecNotSupported")
	}
	d.SupportsDirectPlay = len(dpReasons) == 0 && !o.DisableDirectPlay

	// The first video transcoding profile is the transcode/remux target.
	var tp *TranscodingProfile
	for i := range p.TranscodingProfiles {
		if strings.EqualFold(p.TranscodingProfiles[i].Type, "Video") {
			tp = &p.TranscodingProfiles[i]
			break
		}
	}

	// Direct stream: playable as is, or the video can be copied into the
	// transcoding container (only the container is wrong).
	copyVideo := tp != nil && video != nil && listHasCodec(tp.VideoCodec, video.Codec) && !overLimit &&
		len(codecConditions(p, src, tp.Container, video, nil)) == 0
	remuxOnly := len(dpReasons) == 1 && dpReasons[0] == "ContainerNotSupported" && copyVideo
	d.SupportsDirectStream = (d.SupportsDirectPlay || remuxOnly) && !o.DisableDirectStream
	cpuWork := video != nil && ((video.VideoRangeType != "" && !strings.EqualFold(video.VideoRangeType, "SDR")) ||
		(sub != nil && subtitleMethod(p, *sub, false) == "Encode"))
	encodeVideo := !o.NoVideoEncode && (!o.NoCPUFilters || !cpuWork)
	d.SupportsTranscoding = tp != nil && !o.DisableTranscoding && encodeVideo
	// With the video encoder ruled out, a source whose video the client
	// takes still plays with only its container or audio converted.
	audioOnly := tp != nil && !o.DisableTranscoding && !encodeVideo && copyVideo &&
		!d.SupportsDirectPlay && !d.SupportsDirectStream && onlyAudioOrContainer(dpReasons)
	if audioOnly {
		d.SupportsTranscoding = true
	}

	switch {
	case d.SupportsDirectPlay:
		d.Mode = ModeDirect
	case d.SupportsDirectStream, audioOnly:
		d.Mode = ModeRemux
	case d.SupportsTranscoding:
		d.Mode = ModeTranscode
	default:
		d.Mode = ModeNone
	}

	if d.Mode == ModeRemux || d.Mode == ModeTranscode {
		reasons := dpReasons
		if video != nil && !listHasCodec(tp.VideoCodec, video.Codec) {
			reasons = append(reasons, "VideoCodecNotSupported")
		}
		if audio != nil && !listHasCodec(tp.AudioCodec, audio.Codec) {
			reasons = append(reasons, "AudioCodecNotSupported")
		}
		d.Reasons = sortReasons(reasons)
		d.Transcode = transcodeTarget(*tp, video, audio, limit, d.Mode == ModeRemux)
	}

	direct := d.Mode == ModeDirect || d.Mode == ModeRemux
	for _, s := range src.Streams {
		if s.Type == StreamSubtitle {
			d.Subtitles[s.Index] = subtitleMethod(p, s, direct)
			// An embedded track of a remote file is extracted (a read of the
			// whole file over the network) only for the track picked: apps
			// such as Streamyfin download every External track at start.
			// The rest are offered for burn-in; picking one asks again with
			// it selected, and then it is a file.
			if src.Remote && !s.IsExternal && d.Subtitles[s.Index] == "External" && (sub == nil || sub.Index != s.Index) {
				d.Subtitles[s.Index] = "Encode"
			}
		}
	}
	return d
}

// onlyAudioOrContainer: none of the reasons concern the video or the
// subtitles, so copying the video into the transcoding container, with
// the audio converted, plays.
func onlyAudioOrContainer(reasons []string) bool {
	for _, r := range reasons {
		if r != "ContainerNotSupported" && r != "SecondaryAudioNotSupported" && !strings.HasPrefix(r, "Audio") {
			return false
		}
	}
	return true
}

// transcodeTarget fills in what the transcode produces. Audio is copied
// (keeping its bitrate) when the profile takes its codec; the video gets
// the bitrate limit minus the audio (observed: 6,000,000 − 160,461 =
// 5,839,539), never more than the source's.
func transcodeTarget(tp TranscodingProfile, video, audio *Stream, limit int64, copyVideo bool) *Transcode {
	t := &Transcode{Container: strings.ToLower(tp.Container), Protocol: strings.ToLower(tp.Protocol), CopyVideo: copyVideo}
	if t.Protocol == "" {
		t.Protocol = "http"
	}
	for _, c := range splitList(tp.VideoCodec) {
		if slices.Contains(videoEncoders, canonicalCodec(c)) {
			t.VideoCodecs = append(t.VideoCodecs, strings.ToLower(c))
		}
	}
	for _, c := range splitList(tp.AudioCodec) {
		if slices.Contains(audioEncoders, canonicalCodec(c)) {
			t.AudioCodecs = append(t.AudioCodecs, strings.ToLower(c))
		}
	}
	t.MaxAudioChannels, _ = strconv.Atoi(strings.TrimSpace(tp.MaxAudioChannels))
	if audio != nil {
		channels := audio.Channels
		if t.MaxAudioChannels > 0 && channels > t.MaxAudioChannels {
			channels = t.MaxAudioChannels
		}
		switch {
		case listHasCodec(tp.AudioCodec, audio.Codec) && audio.Bitrate > 0 && channels == audio.Channels:
			t.AudioBitrate = audio.Bitrate // copied
		case channels > 2:
			t.AudioBitrate = 384_000
		default:
			t.AudioBitrate = 192_000
		}
	}
	if video != nil {
		t.MaxFramerate = video.RealFrameRate
		if limit > 0 {
			t.VideoBitrate = limit - t.AudioBitrate
		}
		if video.Bitrate > 0 && (t.VideoBitrate <= 0 || t.VideoBitrate > video.Bitrate) {
			t.VideoBitrate = video.Bitrate
		}
	}
	return t
}
