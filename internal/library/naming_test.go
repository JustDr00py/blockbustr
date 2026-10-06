package library

import "testing"

func TestFolderTitle(t *testing.T) {
	cases := map[string]struct {
		title string
		year  int
	}{
		"Severance (2022)":       {"Severance", 2022},
		"Luca (2021)":            {"Luca", 2021},
		"Blade Runner [1982]":    {"Blade Runner", 1982},
		"The Office (US)":        {"The Office (US)", 0}, // strm.Parse would yield "The Office (US"
		"MF Ghost":               {"MF Ghost", 0},
		"2001 A Space Odyssey":   {"2001 A Space Odyssey", 0},
		"(2019)":                 {"(2019)", 0},
		"Futurama (1999) (2023)": {"Futurama (1999)", 2023},
		"Year One (0999)":        {"Year One (0999)", 0},
	}
	for in, want := range cases {
		if title, year := folderTitle(in); title != want.title || year != want.year {
			t.Errorf("folderTitle(%q) = %q, %d", in, title, year)
		}
	}
}

func TestSortNameRule(t *testing.T) {
	for in, want := range map[string]string{
		"The Matrix": "matrix", "A Quiet Place": "quiet place", "An  Education": "education",
		"Theodore Rex": "theodore rex", "The": "the", "MF Ghost": "mf ghost",
	} {
		if got := SortName(in); got != want {
			t.Errorf("SortName(%q) = %q", in, got)
		}
	}
}

func TestEpisodeTitle(t *testing.T) {
	for in, want := range map[string]string{
		"MF Ghost (2023) - S01E01.001 - The Challenger from England [Bluray-1080p Remux][8bit][h264][FLAC 2.0][JA+EN]-CRUCiBLE.mkv": "The Challenger from England",
		"Severance - S02E01 - Hello, Ms. Cobel.mkv": "Hello, Ms. Cobel",
		"Show - S01E01-E02 - Two Parter [WEB].mkv":  "Two Parter",
		"The.Last.of.Us.S01E03.2160p.WEB-DL.mkv":    "",
		"Show S01E04.mkv":                           "",
		"Some Movie (2010).mkv":                     "",
	} {
		if got := episodeTitle(in); got != want {
			t.Errorf("episodeTitle(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSeasonFromDirAndContainer(t *testing.T) {
	for in, want := range map[string]int{"Show/Season 02/x.mkv": 2, "Show/Specials/x.mkv": 0, "Show/S3/x.mkv": 3} {
		if n, ok := seasonFromDir(in); !ok || n != want {
			t.Errorf("seasonFromDir(%q) = %d, %v", in, n, ok)
		}
	}
	if _, ok := seasonFromDir("Show/x.mkv"); ok {
		t.Error("no season folder")
	}
	for in, want := range map[string]string{"matroska,webm": "mkv", "mov,mp4,m4a,3gp,3g2,mj2": "mp4", "mpegts": "ts", "avi": "avi", "": ""} {
		if got := containerName(in); got != want {
			t.Errorf("containerName(%q) = %q", in, got)
		}
	}
	if containerFromURL("http://jb/stream/Luca.MKV?sig=x") != "mkv" || containerFromURL("http://jb/stream/rd/ABC/1?sig=x") != "" {
		t.Error("containerFromURL")
	}
}
