package handlers

import (
	"path/filepath"
	"strings"

	"github.com/google/uuid"

	"github.com/sysadmin/blockbustr/internal/jfapi/dto"
	"github.com/sysadmin/blockbustr/internal/media"
	"github.com/sysadmin/blockbustr/internal/store/pg/db"
)

// MediaSourceInfo / MediaStream as Jellyfin 12.1.0 reports them in item
// details (TASKS P1.20). PlaybackInfo (P2.1) builds on the same functions and
// adds the per-device play decision; here every Supports* flag is true,
// as in Jellyfin's item details.

// mediaSourceID is the id clients send back as mediaSourceId: the item id for
// an item's first source (as in Jellyfin), the source's own id otherwise.
func mediaSourceID(it db.Item, i int, src db.MediaSource) string {
	if i == 0 {
		return dto.IDFromUUID(it.ID).String()
	}
	return dto.IDFromUUID(src.ID).String()
}

// mediaSourceDtos builds an item's sources. A playable item always gets at
// least one: an unprobed .strm still shows a Play button (DESIGN §7.4).
// base is the server URL remote sources' Path points at.
func mediaSourceDtos(it db.Item, sources []db.MediaSource, streams map[uuid.UUID][]db.MediaStream, base string) []dto.MediaSourceInfo {
	if len(sources) == 0 {
		src := db.MediaSource{ID: it.ID, ItemID: it.ID, Protocol: "File", Etag: it.Etag, RuntimeTicks: it.RuntimeTicks}
		switch {
		case it.StrmUrl != nil:
			src.Protocol, src.IsRemote, src.PathOrUrl = "Http", true, *it.StrmUrl
		case it.Path != nil:
			src.PathOrUrl = *it.Path
		}
		if it.Path != nil {
			src.Name = strings.TrimSuffix(filepath.Base(*it.Path), filepath.Ext(*it.Path))
		}
		sources = []db.MediaSource{src}
	}
	out := make([]dto.MediaSourceInfo, len(sources))
	for i, src := range sources {
		out[i] = mediaSourceDto(it, mediaSourceID(it, i, src), src, streams[src.ID])
		if src.IsRemote {
			// Never the raw target (it may carry signatures or debrid
			// tokens): clients open Path for Http sources, so it is this
			// server's stream URL, which redirects or proxies (DESIGN §6).
			out[i].Path = ptr(base + "/Videos/" + dto.IDFromUUID(it.ID).String() + "/stream?static=true&MediaSourceId=" + *out[i].Id)
		}
	}
	return out
}

func mediaSourceDto(it db.Item, id string, src db.MediaSource, streams []db.MediaStream) dto.MediaSourceInfo {
	container := src.Container
	if container == nil && it.Path != nil && strings.EqualFold(filepath.Ext(*it.Path), ".strm") {
		container = ptr("strm") // what Jellyfin reports for an unprobed .strm
	}
	etag := src.Etag
	if etag == nil {
		etag = ptr(itemEtag(it.ID, it.DateModified, it.Etag))
	}
	ms := mediaStreamDtos(streams)
	d := dto.MediaSourceInfo{
		Protocol: ptr(dto.MediaProtocol(src.Protocol)), Id: &id, Path: ptr(src.PathOrUrl), Type: ptr(dto.MediaSourceTypeDefault),
		Container: container, Size: src.Size, Name: ptr(src.Name), IsRemote: ptr(src.IsRemote), ETag: etag,
		RunTimeTicks: src.RuntimeTicks, Bitrate: src.Bitrate,
		ReadAtNativeFramerate: ptr(false), IgnoreDts: ptr(false), IgnoreIndex: ptr(false), GenPtsInput: ptr(false),
		SupportsTranscoding: ptr(true), SupportsDirectStream: ptr(true), SupportsDirectPlay: ptr(true),
		IsInfiniteStream: ptr(false), UseMostCompatibleTranscodingProfile: ptr(false),
		RequiresOpening: ptr(false), RequiresClosing: ptr(false), RequiresLooping: ptr(false), SupportsProbing: ptr(true),
		VideoType: ptr(dto.VideoTypeVideoFile), Formats: &[]string{}, RequiredHttpHeaders: &map[string]*string{},
		TranscodingSubProtocol: ptr(dto.MediaStreamProtocol("http")), HasSegments: ptr(false),
		MediaStreams: &ms, MediaAttachments: &[]dto.MediaAttachment{},
	}
	firstAudio := int32(-1)
	for _, s := range streams {
		switch {
		case s.Type == "Audio" && s.IsDefault && d.DefaultAudioStreamIndex == nil:
			d.DefaultAudioStreamIndex = ptr(s.Idx)
		case s.Type == "Audio" && firstAudio < 0:
			firstAudio = s.Idx
		case s.Type == "Subtitle" && s.IsDefault && d.DefaultSubtitleStreamIndex == nil:
			d.DefaultSubtitleStreamIndex = ptr(s.Idx)
		}
	}
	if d.DefaultAudioStreamIndex == nil && firstAudio >= 0 {
		d.DefaultAudioStreamIndex = &firstAudio
	}
	if d.DefaultSubtitleStreamIndex == nil && len(streams) > 0 {
		d.DefaultSubtitleStreamIndex = ptr(int32(-1)) // none
	}
	return d
}

var textSubtitleCodecs = map[string]bool{"subrip": true, "srt": true, "ass": true, "ssa": true, "mov_text": true, "webvtt": true, "text": true}

// mediaStreamDtos converts stored streams.
func mediaStreamDtos(streams []db.MediaStream) []dto.MediaStream {
	out := make([]dto.MediaStream, 0, len(streams))
	for _, s := range streams {
		codec := deref(s.Codec)
		title := media.DisplayTitle(media.DisplayInfo{
			Type: media.StreamType(s.Type), Codec: codec, Profile: deref(s.Profile), Language: deref(s.Language),
			Title: deref(s.Title), VideoRange: deref(s.VideoRange), ChannelLayout: deref(s.ChannelLayout),
			Width: int(deref(s.Width)), Height: int(deref(s.Height)), Channels: int(deref(s.Channels)), Interlaced: s.IsInterlaced,
			IsDefault: s.IsDefault, IsForced: s.IsForced, IsExternal: s.IsExternal, IsHearingImpaired: s.IsHearingImpaired,
			IsOriginal: s.IsOriginal, DoViProfile: int(deref(s.DvProfile)), DoViCompatID: int(deref(s.DvBlCompatID)),
		})
		text := s.Type == "Subtitle" && textSubtitleCodecs[codec]
		d := dto.MediaStream{
			Codec: s.Codec, Language: s.Language, TimeBase: s.TimeBase, Title: s.Title, DisplayTitle: &title,
			VideoRange: ptr(dto.VideoRange("Unknown")), VideoRangeType: ptr(dto.VideoRangeType("Unknown")),
			AudioSpatialFormat: ptr(dto.AudioSpatialFormatNone), IsInterlaced: ptr(s.IsInterlaced),
			IsDefault: ptr(s.IsDefault), IsForced: ptr(s.IsForced), IsHearingImpaired: ptr(s.IsHearingImpaired),
			IsOriginal: ptr(s.IsOriginal), Type: ptr(dto.MediaStreamType(s.Type)), Index: ptr(s.Idx),
			IsExternal: ptr(s.IsExternal), IsTextSubtitleStream: ptr(text), SupportsExternalStream: ptr(text),
			BitRate: s.Bitrate, DeliveryUrl: s.DeliveryUrl,
		}
		lang := ""
		if s.Language != nil {
			lang = media.LanguageName(*s.Language)
		}
		switch s.Type {
		case "Video":
			d.VideoRange, d.VideoRangeType = ptr(dto.VideoRange(orDefault(s.VideoRange, "SDR"))), ptr(dto.VideoRangeType(orDefault(s.VideoRangeType, "SDR")))
			d.Width, d.Height, d.BitDepth, d.PixelFormat, d.AspectRatio = s.Width, s.Height, s.BitDepth, s.PixelFormat, s.AspectRatio
			d.AverageFrameRate, d.RealFrameRate, d.ReferenceFrameRate = s.AverageFrameRate, s.RealFrameRate, s.AverageFrameRate
			d.Profile, d.IsAnamorphic = s.Profile, ptr(false)
			if codec != "hevc" { // Jellyfin leaves RefFrames out for HEVC
				d.RefFrames = ptr(int32(1))
			}
			if s.Level != nil {
				d.Level = ptr(float64(*s.Level))
			}
			if codec == "h264" {
				d.NalLengthSize, d.IsAVC = ptr("4"), ptr(true)
			}
			d.ColorTransfer, d.ColorPrimaries, d.ColorSpace, d.ColorRange = s.ColorTransfer, s.ColorPrimaries, s.ColorSpace, s.ColorRange
			if s.DvProfile != nil {
				d.DvProfile, d.DvLevel, d.DvBlSignalCompatibilityId = s.DvProfile, s.DvLevel, s.DvBlCompatID
				d.DvVersionMajor, d.DvVersionMinor = s.DvVersionMajor, s.DvVersionMinor
				flag := func(b *bool) *int32 {
					if b == nil {
						return nil
					}
					if *b {
						return ptr(int32(1))
					}
					return ptr(int32(0))
				}
				d.RpuPresentFlag, d.ElPresentFlag, d.BlPresentFlag = flag(s.DvRpuPresent), flag(s.DvElPresent), flag(s.DvBlPresent)
				d.VideoDoViTitle = ptr(media.DoViTitle(int(*s.DvProfile), int(deref(s.DvBlCompatID))))
			}
		case "Audio":
			d.ChannelLayout, d.Channels, d.SampleRate, d.BitDepth = s.ChannelLayout, s.Channels, s.SampleRate, s.BitDepth
			d.Profile = s.Profile
			d.AudioSpatialFormat = ptr(spatialFormat(codec, deref(s.Profile)))
			d.LocalizedDefault, d.LocalizedExternal, d.LocalizedOriginal = ptr("Default"), ptr("External"), ptr("Original")
			if lang != "" {
				d.LocalizedLanguage = &lang
			}
		case "Subtitle":
			d.LocalizedUndefined, d.LocalizedDefault, d.LocalizedForced = ptr("Undefined"), ptr("Default"), ptr("Forced")
			d.LocalizedExternal, d.LocalizedHearingImpaired = ptr("External"), ptr("Hearing Impaired")
			if lang != "" {
				d.LocalizedLanguage = &lang
			}
		}
		out = append(out, d)
	}
	return out
}

func orDefault(p *string, def string) string {
	if p == nil || *p == "" {
		return def
	}
	return *p
}

// spatialFormat is Jellyfin's AudioSpatialFormat, read from the ffprobe profile.
func spatialFormat(codec, profile string) dto.AudioSpatialFormat {
	p := strings.ToLower(profile)
	switch {
	case strings.Contains(p, "atmos"):
		return dto.AudioSpatialFormat("DolbyAtmos")
	case codec == "dts" && strings.Contains(p, "dts:x"):
		return dto.AudioSpatialFormat("DTSX")
	}
	return dto.AudioSpatialFormatNone
}
