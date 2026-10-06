package media

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every MediaStream Jellyfin sent in any capture must get the same DisplayTitle.
func TestDisplayTitleMatchesJellyfin(t *testing.T) {
	files, _ := filepath.Glob("../../testdata/jellyfin/*/*/[0-9]*.json")
	type stream struct {
		Type, Codec, Profile, Language, Title, VideoRange, ChannelLayout, DisplayTitle string
		Width, Height, Channels, DvProfile, DvBlSignalCompatibilityId                  int
		IsInterlaced, IsDefault, IsForced, IsExternal, IsHearingImpaired, IsOriginal   bool
	}
	seen := map[string]bool{}
	var walk func(v any)
	checked := 0
	walk = func(v any) {
		switch n := v.(type) {
		case map[string]any:
			if _, ok := n["DisplayTitle"]; ok && n["Type"] != nil && n["Index"] != nil {
				raw, _ := json.Marshal(n)
				var s stream
				_ = json.Unmarshal(raw, &s)
				if !seen[string(raw)] {
					seen[string(raw)] = true
					checked++
					got := DisplayTitle(DisplayInfo{Type: StreamType(s.Type), Codec: s.Codec, Profile: s.Profile, Language: s.Language,
						Title: s.Title, VideoRange: s.VideoRange, ChannelLayout: s.ChannelLayout, Width: s.Width, Height: s.Height,
						Channels: s.Channels, Interlaced: s.IsInterlaced, IsDefault: s.IsDefault, IsForced: s.IsForced,
						IsExternal: s.IsExternal, IsHearingImpaired: s.IsHearingImpaired, IsOriginal: s.IsOriginal,
						DoViProfile: s.DvProfile, DoViCompatID: s.DvBlSignalCompatibilityId})
					if got != s.DisplayTitle {
						t.Errorf("%s %s %q: got %q, want %q", s.Type, s.Codec, s.Title, got, s.DisplayTitle)
					}
				}
			}
			for _, c := range n {
				walk(c)
			}
		case []any:
			for _, c := range n {
				walk(c)
			}
		}
	}
	for _, p := range files {
		if strings.Contains(p, "/_raw/") {
			continue
		}
		raw, _ := os.ReadFile(p)
		var doc any
		_ = json.Unmarshal(raw, &doc)
		walk(doc)
	}
	if checked < 20 {
		t.Fatalf("only %d captured streams found", checked)
	}
	t.Logf("%d distinct captured streams", checked)
}

func TestDisplayTitleWithoutTitle(t *testing.T) {
	for _, c := range []struct {
		in   DisplayInfo
		want string
	}{
		{DisplayInfo{Type: StreamVideo, Codec: "hevc", Width: 3840, Height: 2160, VideoRange: "HDR"}, "4K HEVC HDR"},
		{DisplayInfo{Type: StreamVideo, Codec: "hevc", Width: 3840, Height: 1600, DoViProfile: 8, DoViCompatID: 1}, "4K HEVC Dolby Vision Profile 8.1 (HDR10)"},
		{DisplayInfo{Type: StreamAudio, Codec: "truehd", Language: "eng", ChannelLayout: "7.1", IsDefault: true}, "English - Dolby TrueHD - 7.1 - Default"},
		{DisplayInfo{Type: StreamAudio, Codec: "aac", Profile: "LC", Channels: 2}, "AAC - 2 ch"},
		{DisplayInfo{Type: StreamSubtitle, Codec: "subrip"}, "Undefined - SUBRIP"},
		{DisplayInfo{Type: StreamSubtitle, Codec: "pgssub", Language: "fre", IsForced: true, IsExternal: true}, "French - Forced - PGSSUB - External"},
	} {
		if got := DisplayTitle(c.in); got != c.want {
			t.Errorf("%+v: %q, want %q", c.in, got, c.want)
		}
	}
}
