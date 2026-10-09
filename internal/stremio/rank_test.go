package stremio

import (
	"encoding/json"
	"os"
	"slices"
	"testing"
	"time"
)

func fixtureStreams(t *testing.T, name string) []Stream {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	var out struct{ Streams []Stream }
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out.Streams
}

const gb = 1 << 30

// size is n GB as parseSize computes it.
func size(n float64) int64 { return int64(n * gb) }

func TestParseTorrentio(t *testing.T) {
	movie := fixtureStreams(t, "torrentio-stream-movie.json")
	episode := fixtureStreams(t, "torrentio-stream-episode.json")
	cases := []struct {
		name string
		s    Stream
		want Info
	}{
		{"remux without filename", movie[0], Info{
			Height: 2160, HDR: true, Size: size(51.3), Seeders: 100, Group: "DTOne", Remux: true,
			Languages: []string{"multi", "en", "it", "pt", "ko", "zh", "fr", "de", "ar"},
		}},
		{"x265 group from filename", movie[1], Info{
			Height: 2160, HDR: true, Codec: "hevc", Size: size(35.09), Seeders: 99, Group: "IAMABLE",
		}},
		{"vp9 web-dl", movie[2], Info{
			Height: 2160, HDR: true, Codec: "vp9", Size: size(19.47), Seeders: 96, Group: "SomniWare",
			Languages: []string{"en", "ru", "it", "pt", "es", "ko", "zh", "fr", "de", "nl", "hu", "da", "sv", "no", "tr", "ar", "id"},
		}},
		{"bracketed group isn't a group", movie[3], Info{
			Height: 2160, HDR: true, Codec: "hevc", Size: size(4.98), Seeders: 80,
		}},
		{"season pack file", episode[0], Info{
			Height: 2160, HDR: true, Codec: "hevc", Size: size(15.98), Seeders: 23,
		}},
		{"multi with group after a path", episode[1], Info{
			Height: 2160, HDR: true, Codec: "hevc", Size: size(4.25), Seeders: 17, Group: "QTZ",
			Languages: []string{"multi", "fr"},
		}},
		{"dolby vision remux", episode[2], Info{
			Height: 2160, HDR: true, DV: true, Codec: "hevc", Size: size(29.2), Seeders: 8, Group: "PB69", Remux: true,
		}},
		{"spaced H 265", episode[3], Info{
			Height: 2160, HDR: true, DV: true, Codec: "hevc", Size: size(10.56), Seeders: 6, Group: "Kitsune",
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Parse(c.s)
			if !infoEqual(got, c.want) {
				t.Errorf("Parse =\n %+v\nwant\n %+v", got, c.want)
			}
		})
	}
}

func infoEqual(a, b Info) bool {
	return slices.Equal(a.Languages, b.Languages) &&
		a.Height == b.Height && a.HDR == b.HDR && a.DV == b.DV && a.Codec == b.Codec && a.Size == b.Size &&
		a.Seeders == b.Seeders && a.Group == b.Group && a.Cached == b.Cached && a.Uncached == b.Uncached &&
		a.Cam == b.Cam && a.Remux == b.Remux
}

func TestParseMarkers(t *testing.T) {
	cases := []struct {
		name string
		s    Stream
		want func(Info) bool
	}{
		{"RD+ is cached", Stream{Name: "[RD+] Torrentio\n1080p", InfoHash: "a"}, func(i Info) bool { return i.Cached && i.Debrid && !i.Uncached }},
		{"lightning is cached", Stream{Name: "TB ⚡\n1080p", InfoHash: "a"}, func(i Info) bool { return i.Cached }},
		{"RD download is uncached", Stream{Name: "[RD download] Torrentio\n1080p", InfoHash: "a"}, func(i Info) bool { return !i.Cached && i.Uncached }},
		{"RD-configured cached URL", Stream{Name: "[RD+] Torrentio\n4k", URL: "https://torrentio.strem.fun/resolve/realdebrid/K/h/null/0/x.mkv"}, func(i Info) bool { return i.Cached && i.Debrid && !i.Uncached }},
		{"RD-configured uncached URL", Stream{Name: "[RD download] Torrentio\n4k", URL: "https://torrentio.strem.fun/resolve/realdebrid/K/h/null/0/x.mkv"}, func(i Info) bool { return !i.Cached && !i.Debrid && i.Uncached }},
		{"direct URL is ready", Stream{Name: "Addon", URL: "https://cdn.example/x.mkv"}, func(i Info) bool { return i.Cached && !i.Debrid }},
		{"plain torrent is neither", Stream{Name: "Torrentio\n1080p", InfoHash: "a"}, func(i Info) bool { return !i.Cached && !i.Uncached }},
		{"HDCAM", Stream{Title: "Movie.2026.HDCAM.x264-GRP\n👤 5"}, func(i Info) bool { return i.Cam && i.Codec == "h264" && i.Group == "GRP" }},
		{"TELESYNC filename", Stream{Hints: StreamHints{Filename: "Movie 2026 TELESYNC 720p.mkv"}}, func(i Info) bool { return i.Cam && i.Height == 720 }},
		{"DTS isn't TS", Stream{Title: "Movie.2026.1080p.BluRay.DTS-HD.MA.5.1.x264-GRP"}, func(i Info) bool { return !i.Cam && i.Height == 1080 }},
		{"4k from the name", Stream{Name: "Torrentio\n4k"}, func(i Info) bool { return i.Height == 2160 }},
		{"AIOStreams FHD", Stream{Name: "🚀 FHD", Description: "🎬 Conclave (2024)\n🎥 BluRay 🎞️ AVC\n⚡Ready (DG)"}, func(i Info) bool { return i.Height == 1080 && i.Debrid }},
		{"av1", Stream{Title: "Show.S01E01.1080p.WEB.AV1-GRP"}, func(i Info) bool { return i.Codec == "av1" }},
		{"videoSize wins", Stream{Title: "x 💾 1.5 GB", Hints: StreamHints{VideoSize: 123}}, func(i Info) bool { return i.Size == 123 }},
		{"MB size", Stream{Title: "x 💾 700 MB"}, func(i Info) bool { return i.Size == 700<<20 }},
		{"spaced group", Stream{Title: "The Matrix (1999) 1080p BrRip x264 - 1.85GB - YIFY"}, func(i Info) bool { return i.Group == "YIFY" }},
		{"tracker tag after group", Stream{Title: "The.Matrix.1999.2160p.H.265-PiRaTeS[TGx]"}, func(i Info) bool { return i.Group == "PiRaTeS" }},
		{"no labels", Stream{InfoHash: "a"}, func(i Info) bool { return i.Height == 0 && i.Codec == "" && i.Languages == nil }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Parse(c.s); !c.want(got) {
				t.Errorf("Parse = %+v", got)
			}
		})
	}
}

func TestScore(t *testing.T) {
	base := Info{Height: 1080, Codec: "h264"}
	all := Prefs{HEVC: true, AV1: true, HDR: true}
	better := []struct {
		name    string
		a, b    Info
		prefs   Prefs
		comment string
	}{
		{"cached beats resolution", Info{Height: 720, Cached: true}, Info{Height: 2160}, all, ""},
		{"2160 beats 1080", Info{Height: 2160}, Info{Height: 1080}, all, ""},
		{"1080 beats 720", Info{Height: 1080}, Info{Height: 720}, all, ""},
		{"unknown height beats 480", Info{}, Info{Height: 480}, all, ""},
		{"720 beats unknown", Info{Height: 720}, Info{}, all, ""},
		{"no-HEVC client prefers h264", base, Info{Height: 1080, Codec: "hevc"}, Prefs{}, ""},
		{"HEVC client doesn't mind", Info{Height: 2160, Codec: "hevc"}, Info{Height: 1080, Codec: "h264"}, all, ""},
		{"no-AV1 client", base, Info{Height: 1080, Codec: "av1"}, Prefs{HEVC: true}, ""},
		{"SDR client prefers SDR at same height", Info{Height: 2160}, Info{Height: 2160, HDR: true}, Prefs{HEVC: true}, ""},
		{"capped client prefers 1080 to 2160", Info{Height: 1080}, Info{Height: 2160}, Prefs{MaxHeight: 1080}, ""},
		{"over bitrate budget", Info{Height: 2160, Size: 10 * gb}, Info{Height: 2160, Size: 60 * gb},
			Prefs{MaxBitrate: 40_000_000, Runtime: 2 * time.Hour, HEVC: true, HDR: true}, "(60 GB over 2h ≈ 72 Mb/s)"},
		{"all over budget: nearest above the cap wins", Info{Height: 1080, Size: 8 * gb}, Info{Height: 2160, Size: 60 * gb},
			Prefs{MaxBitrate: 4_000_000, Runtime: 2 * time.Hour, HEVC: true, HDR: true}, "(8 GB ≈ 9½ Mb/s vs 72: the 4K is the transcode input to avoid)"},
		{"farther over budget loses at the same height", Info{Height: 2160, Size: 20 * gb}, Info{Height: 2160, Size: 70 * gb},
			Prefs{MaxBitrate: 8_000_000, Runtime: 2 * time.Hour, HEVC: true, HDR: true}, ""},
		{"cached far over budget still beats uncached in budget", Info{Height: 2160, Size: 60 * gb, Cached: true}, Info{Height: 2160, Size: 1 * gb},
			Prefs{MaxBitrate: 4_000_000, Runtime: 2 * time.Hour, HEVC: true, HDR: true}, "the floor keeps playable-right-away first"},
		{"allowed group", Info{Height: 1080, Group: "FLUX"}, Info{Height: 1080, Group: "OTHER"}, Prefs{Allow: []string{"flux"}}, ""},
		{"preferred language", Info{Height: 1080, Languages: []string{"de"}}, Info{Height: 1080, Languages: []string{"fr"}}, Prefs{Languages: []string{"de"}}, ""},
		{"wanted language only beats it among others", Info{Height: 1080, Languages: []string{"en"}}, Info{Height: 1080, Languages: []string{"en", "it"}}, Prefs{Languages: []string{"en"}}, "(iTA-ENG releases often default to Italian)"},
		{"…but not a resolution step", Info{Height: 2160, Languages: []string{"en", "it"}}, Info{Height: 1080, Languages: []string{"en"}}, Prefs{Languages: []string{"en"}, HEVC: true, HDR: true}, ""},
		{"multi beats wrong language", Info{Height: 1080, Languages: []string{"multi", "it"}}, Info{Height: 1080, Languages: []string{"fr"}}, Prefs{Languages: []string{"de"}}, ""},
		{"unflagged counts as English", Info{Height: 720}, Info{Height: 2160, Languages: []string{"ru"}}, Prefs{Languages: []string{"en"}}, ""},
		{"seeders break ties", Info{Height: 1080, Seeders: 200}, Info{Height: 1080, Seeders: 3}, all, ""},
	}
	for _, c := range better {
		t.Run(c.name, func(t *testing.T) {
			sa, oka := Score(c.a, c.prefs)
			sb, okb := Score(c.b, c.prefs)
			if !oka || !okb {
				t.Fatalf("ok = %v, %v", oka, okb)
			}
			if sa <= sb {
				t.Errorf("score(a) = %d, score(b) = %d; want a > b %s", sa, sb, c.comment)
			}
		})
	}

	t.Run("seeders never outweigh a resolution step", func(t *testing.T) {
		a, _ := Score(Info{Height: 1080, Seeders: 100000}, all)
		b, _ := Score(Info{Height: 2160}, all)
		if a >= b {
			t.Errorf("%d >= %d", a, b)
		}
	})
	t.Run("CAM rips are never offered, even cached", func(t *testing.T) {
		if _, ok := Score(Info{Height: 2160, Cached: true, Cam: true}, all); ok {
			t.Error("ok = true")
		}
	})
	t.Run("denied group is dropped, case-insensitively", func(t *testing.T) {
		if _, ok := Score(Info{Height: 2160, Group: "YIFY"}, Prefs{Deny: []string{"yify"}}); ok {
			t.Error("ok = true")
		}
	})
	t.Run("no language preference is neutral", func(t *testing.T) {
		a, _ := Score(Info{Height: 1080, Languages: []string{"ru"}}, Prefs{})
		b, _ := Score(Info{Height: 1080}, Prefs{})
		if a != b {
			t.Errorf("%d != %d", a, b)
		}
	})
	t.Run("langScore doesn't write into the caller's slice", func(t *testing.T) {
		have := make([]string, 1, 4)
		have[0] = "multi"
		langScore(have, []string{"en"})
		if got := have[:2][1]; got != "" {
			t.Errorf("caller's backing array written: %q", got)
		}
	})
}

func idx(i int) *int { return &i }

func TestRank(t *testing.T) {
	offers := []Offer{
		{Stream: Stream{Name: "A\n720p", InfoHash: "h1", FileIdx: idx(0)}, Addon: "first"},
		{Stream: Stream{Name: "A\n4k", InfoHash: "h2"}, Addon: "first"},
		{Stream: Stream{Name: "external", ExternalURL: "https://example.com"}, Addon: "first"},
		{Stream: Stream{Name: "B\n1080p", InfoHash: "h1", FileIdx: idx(0)}, Addon: "second"}, // duplicate of the first
		{Stream: Stream{Name: "B\n1080p", InfoHash: "h1", FileIdx: idx(1)}, Addon: "second"}, // another file: kept
		{Stream: Stream{Name: "B 1080p", URL: "https://cdn.example/a.mkv"}, Addon: "second"},
		{Stream: Stream{Name: "B\n1080p", InfoHash: "h3"}, Addon: "second", Cached: true},
		{Stream: Stream{Title: "Movie.2160p.x264-BAD", InfoHash: "h4"}, Addon: "second"},
		{Stream: Stream{Name: "C\n720p", InfoHash: "h5"}, Addon: "third"},
	}
	got := Rank(offers, Prefs{Deny: []string{"bad"}, HDR: true, HEVC: true})
	var order []string
	for _, r := range got {
		order = append(order, r.Addon+" "+r.Stream.Name+" "+r.Stream.InfoHash)
	}
	want := []string{
		"second B 1080p ",    // direct URL: ready, and offered before the cached torrent
		"second B\n1080p h3", // reported cached by debrid
		"first A\n4k h2",     // best picture among the rest
		"second B\n1080p h1", // file 1 of h1
		"first A\n720p h1",   // file 0 of h1: the first copy is kept
		"third C\n720p h5",   // equal score: addon order kept
	}
	if !slices.Equal(order, want) {
		t.Errorf("order =\n%q\nwant\n%q", order, want)
	}
	if len(got) > 1 && !got[1].Info.Cached {
		t.Error("offer.Cached not carried into Info")
	}
}

func TestRankEmpty(t *testing.T) {
	if got := Rank(nil, Prefs{}); len(got) != 0 {
		t.Errorf("Rank(nil) = %v", got)
	}
}

// AIOStreams' formatter output (the user's own, 2026-10-08).
func TestParseAIOStreamsFormatter(t *testing.T) {
	s := Stream{
		Name: "🔥4K UHD",
		Description: "🎬 Movie Title (2023) \n🎥 BluRay 📺 DV 🎞️ HEVC ⏱️ 2h:32m:0s \n🎧 Atmos | TrueHD 🔊 7.1 🗣️ 🇬🇧 / 🇮🇹\n" +
			"📦 62.5 GB / 125 GB 📊 54.8 Mbps \n🔍Torrentio \nℹ️ This is a message",
		URL: "https://x/play",
	}
	in := Parse(s)
	if in.Height != 2160 || !in.DV || in.Codec != "hevc" || in.Audio != "Atmos TrueHD 7.1" ||
		in.Duration != 2*time.Hour+32*time.Minute || in.Size != int64(62.5*(1<<30)) || !slices.Equal(in.Languages, []string{"en", "it"}) {
		t.Errorf("Parse = %+v", in)
	}
	// Exact bytes (behaviorHints.videoSize) give AIOStreams' own figure.
	in.Size = 62_500_000_000
	if b := in.Bitrate(0); b/100_000 != 548 {
		t.Errorf("bitrate over the addon's duration = %d", b)
	}
}

func TestParseAudio(t *testing.T) {
	for text, want := range map[string]string{
		"Movie.2023.2160p.UHD.BluRay.TrueHD.Atmos.7.1.DV.HEVC-FLUX": "Atmos TrueHD 7.1",
		"Movie 2023 1080p WEB-DL DDP5.1 H 264-NTb":                  "DD+ 5.1",
		"Movie.2023.1080p.BluRay.DTS-HD.MA.5.1.x264":                "DTS-HD MA 5.1",
		"Movie 2023 1080p BluRay DTS-X 7.1":                         "DTS:X 7.1",
		"Movie.2023.720p.WEBRip.AAC2.0.x264":                        "AAC 2.0",
		"Movie 2023 1080p WEB EAC3 5.1":                             "DD+ 5.1",
		"Movie 2023 1080p x265 💾 2.0 GB":                            "",
		"Movie.2023.1080p.WEB.h264-ADDS":                            "",
	} {
		if got := parseAudio(text); got != want {
			t.Errorf("parseAudio(%q) = %q, want %q", text, got, want)
		}
	}
}

func TestParseDuration(t *testing.T) {
	for text, want := range map[string]time.Duration{
		"⏱️ 2h:32m:0s":    2*time.Hour + 32*time.Minute,
		"⏱ 1h 05m":        time.Hour + 5*time.Minute,
		"⏱️ 95m":          95 * time.Minute,
		"⏱️ 0h:45m:30s":   45*time.Minute + 30*time.Second,
		"2160p x265 3m":   0, // too short to be a runtime
		"no runtime here": 0,
	} {
		if got := parseDuration(text); got != want {
			t.Errorf("parseDuration(%q) = %v, want %v", text, got, want)
		}
	}
}

// With no runtime for the title, the bitrate cap uses the addon's duration.
func TestScoreBitrateFromAddonDuration(t *testing.T) {
	p := Prefs{MaxBitrate: 20_000_000, HEVC: true, AV1: true, HDR: true}
	big := Info{Height: 2160, Size: 60_000_000_000, Duration: 2 * time.Hour}   // ~67 Mbps
	small := Info{Height: 2160, Size: 10_000_000_000, Duration: 2 * time.Hour} // ~11 Mbps
	sb, _ := Score(big, p)
	ss, _ := Score(small, p)
	if sb >= ss {
		t.Errorf("over the cap %d, under it %d", sb, ss)
	}
	big.Duration = 0
	if s, _ := Score(big, p); s != ss {
		t.Errorf("no runtime at all: no penalty, got %d want %d", s, ss)
	}
}
