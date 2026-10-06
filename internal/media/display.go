package media

import (
	"fmt"
	"strings"

	"golang.org/x/text/language"
	"golang.org/x/text/language/display"
)

// Jellyfin's MediaStream.DisplayTitle (12.1.0), the track names clients show
// in their audio/subtitle pickers (TASKS P1.20). Reproduced from captured
// streams; display_test.go checks every stream in testdata/jellyfin.

// DisplayInfo is what DisplayTitle needs from a stream.
type DisplayInfo struct {
	Type                                                       StreamType
	Codec, Profile, Language, Title, VideoRange, ChannelLayout string
	Width, Height, Channels                                    int
	Interlaced                                                 bool
	IsDefault, IsForced, IsExternal, IsHearingImpaired         bool
	IsOriginal                                                 bool
	DoViProfile, DoViCompatID                                  int // 0 = not Dolby Vision
}

// LanguageName is the English name of an ISO 639 code ("jpn" → "Japanese"),
// or the code itself when unknown.
func LanguageName(code string) string {
	if code == "" {
		return ""
	}
	tag, err := language.Parse(code)
	if err != nil {
		return code
	}
	if name := display.English.Languages().Name(tag); name != "" {
		return name
	}
	return code
}

// ResolutionText is Jellyfin's resolution label ("4K", "1080p", "SD"…).
func ResolutionText(width, height int, interlaced bool) string {
	if width == 0 || height == 0 {
		return ""
	}
	scan := "p"
	if interlaced {
		scan = "i"
	}
	switch {
	case width >= 3800 || height >= 2000:
		return "4K"
	case width >= 2500 || height >= 1400:
		return "1440" + scan
	case width >= 1900 || height >= 1000:
		return "1080" + scan
	case width >= 1260 || height >= 700:
		return "720" + scan
	case width >= 1000 || height >= 560:
		return "576" + scan
	case width >= 700 || height >= 440:
		return "480" + scan
	}
	return "SD"
}

var audioCodecNames = map[string]string{
	"aac": "AAC", "ac3": "Dolby Digital", "eac3": "Dolby Digital+", "truehd": "Dolby TrueHD",
	"dts": "DTS", "flac": "FLAC", "mp3": "MP3", "mp2": "MP2", "opus": "Opus", "vorbis": "Vorbis",
	"alac": "ALAC", "wmav2": "WMA", "pcm_s16le": "PCM", "pcm_s24le": "PCM",
}

// AudioCodecName is the friendly name Jellyfin shows for an audio codec.
func AudioCodecName(codec string) string {
	if n, ok := audioCodecNames[strings.ToLower(codec)]; ok {
		return n
	}
	return strings.ToUpper(codec)
}

func firstUpper(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// DoViTitle names a Dolby Vision stream ("Dolby Vision Profile 8.1 (HDR10)").
func DoViTitle(profile, compatID int) string {
	if profile == 0 {
		return ""
	}
	t := fmt.Sprintf("Dolby Vision Profile %d", profile)
	if compatID > 0 {
		t += fmt.Sprintf(".%d", compatID)
	}
	switch compatID {
	case 1:
		t += " (HDR10)"
	case 2:
		t += " (SDR)"
	case 4:
		t += " (HLG)"
	}
	return t
}

// DisplayTitle is the stream's display name.
func DisplayTitle(s DisplayInfo) string {
	var attrs []string
	switch s.Type {
	case StreamVideo:
		if r := ResolutionText(s.Width, s.Height, s.Interlaced); r != "" {
			attrs = append(attrs, r)
		}
		if s.Codec != "" {
			attrs = append(attrs, strings.ToUpper(s.Codec))
		}
		if dv := DoViTitle(s.DoViProfile, s.DoViCompatID); dv != "" {
			attrs = append(attrs, dv)
		} else if s.VideoRange != "" && s.VideoRange != "Unknown" {
			attrs = append(attrs, s.VideoRange)
		}
		if s.Title == "" {
			return strings.Join(attrs, " ")
		}
	case StreamAudio:
		if s.Language != "" {
			attrs = append(attrs, firstUpper(LanguageName(s.Language)))
		}
		if s.Profile != "" && !strings.EqualFold(s.Profile, "lc") {
			attrs = append(attrs, s.Profile)
		} else if s.Codec != "" {
			attrs = append(attrs, AudioCodecName(s.Codec))
		}
		if s.ChannelLayout != "" {
			attrs = append(attrs, firstUpper(s.ChannelLayout))
		} else if s.Channels > 0 {
			attrs = append(attrs, fmt.Sprintf("%d ch", s.Channels))
		}
		if s.IsDefault {
			attrs = append(attrs, "Default")
		}
		if s.IsExternal {
			attrs = append(attrs, "External")
		}
		if s.IsOriginal {
			attrs = append(attrs, "Original")
		}
	case StreamSubtitle:
		if s.Language != "" {
			attrs = append(attrs, firstUpper(LanguageName(s.Language)))
		} else {
			attrs = append(attrs, "Undefined")
		}
		if s.IsHearingImpaired {
			attrs = append(attrs, "Hearing Impaired")
		}
		if s.IsDefault {
			attrs = append(attrs, "Default")
		}
		if s.IsForced {
			attrs = append(attrs, "Forced")
		}
		if s.Codec != "" {
			attrs = append(attrs, strings.ToUpper(s.Codec))
		}
		if s.IsExternal {
			attrs = append(attrs, "External")
		}
	default:
		return s.Title
	}
	if s.Title == "" {
		return strings.Join(attrs, " - ")
	}
	// A title is kept as is; attributes it doesn't already mention are appended.
	out := s.Title
	for _, a := range attrs {
		if !strings.Contains(strings.ToLower(s.Title), strings.ToLower(a)) {
			out += " - " + a
		}
	}
	return out
}
