package strm

import "testing"

func TestIsExtra(t *testing.T) {
	for path, want := range map[string]bool{
		"Featurettes/Feature-Length Storyboards.mkv": true,
		"Movie (2020)/Behind_The_Scenes/making.mkv":  true,
		"Extras/x.mkv":                        true,
		"Deleted.Scenes/cut.mkv":              true,
		"Movie-trailer.mkv":                   true,
		"sample.mkv":                          true,
		"Show/Specials/Show S00E01.mkv":       false,
		"Show/Season 01/Show S01E01.mkv":      false,
		"Short Circuit (1986).mkv":            false,
		"The.Making.of.a.Murderer.S01E01.mkv": false,
		"Dune.Part.Two.2024.1080p.mkv":        false,
	} {
		if got := IsExtra(path); got != want {
			t.Errorf("IsExtra(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestResolution(t *testing.T) {
	cases := map[string]string{
		"Tulsa.King.S01E01.720p.AMZN.WEBRip.x264-GalaxyTV.mkv": "720p",
		"Moana.2016.1080p.BluRay.DDP.7.1.x265-EDGE2020.mkv":    "1080p",
		"Dune.Part.Two.2024.2160p.UHD.BluRay.x265.mkv":         "2160p",
		"Dune Part Two 2024 4K HDR.mkv":                        "2160p",
		"Old.Show.S01E01.1080i.HDTV.mkv":                       "1080p",
		"Show.S01E01.1920x1080.mkv":                            "1080p",
		"[SubsPlease] Sousou no Frieren S2 - 04 (1080p).mkv":   "1080p",
		"Movie.2020.DVDRip.mkv":                                "",
		"Movie.2020.x265.mkv":                                  "",
	}
	for in, want := range cases {
		if got := Resolution(in); got != want {
			t.Errorf("Resolution(%q) = %q, want %q", in, got, want)
		}
	}
}
