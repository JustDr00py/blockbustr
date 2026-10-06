package strm

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRead(t *testing.T) {
	const jb = "http://jellybird:8097/stream/realdebrid/TORRENTID/1?sig=REDACTED"
	cases := []struct {
		name, in, want string
		err            bool
	}{
		{"jellybird url with newline", jb + "\n", jb, false},
		{"no trailing newline", jb, jb, false},
		{"CRLF, BOM, whitespace", "\xef\xbb\xbf  " + jb + "  \r\n", jb, false},
		{"leading comments and blanks", "# made by jellybird\n\n   \n" + jb + "\nhttp://ignored/second", jb, false},
		{"https", "https://cdn.example/x.mkv", "https://cdn.example/x.mkv", false},
		{"rtsp", "rtsp://cam.local:554/stream", "rtsp://cam.local:554/stream", false},
		{"absolute local path", "/media/Movies/Luca (2021)/Luca.mkv", "/media/Movies/Luca (2021)/Luca.mkv", false},
		{"relative path rejected", "Movies/Luca.mkv", "", true},
		{"bare host rejected", "jellybird:8097/stream", "", true},
		{"empty", "", "", true},
		{"only comments", "# nothing here\n#\n", "", true},
	}
	for _, c := range cases {
		got, err := Read(strings.NewReader(c.in))
		if (err != nil) != c.err || got != c.want {
			t.Errorf("%s: Read = %q, %v", c.name, got, err)
		}
	}
	if _, err := Read(strings.NewReader("")); !errors.Is(err, ErrEmpty) {
		t.Errorf("empty file error = %v", err)
	}
	if _, err := Read(strings.NewReader("http://x/" + strings.Repeat("a", MaxFileSize))); err == nil {
		t.Error("oversized file accepted")
	}
}

func TestReadFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "Luca (2021).strm")
	if err := os.WriteFile(p, []byte("http://jellybird:8097/stream/x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := ReadFile(p); err != nil || got != "http://jellybird:8097/stream/x" {
		t.Errorf("ReadFile = %q, %v", got, err)
	}
	if _, err := ReadFile(p + ".missing"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("missing file: %v", err)
	}
	if !IsRemote("http://x/y") || IsRemote("/media/x.mkv") {
		t.Error("IsRemote")
	}
}

// The recon test library (~/Videos) as the scanner will see it.
func TestParseReconLibrary(t *testing.T) {
	movies := map[string]Parsed{
		"Little Fockers (2010).strm":  {Kind: KindMovie, Title: "Little Fockers", Year: 2010},
		"Luca (2021).strm":            {Kind: KindMovie, Title: "Luca", Year: 2021},
		"Mortal Kombat II (2026).mp4": {Kind: KindMovie, Title: "Mortal Kombat II", Year: 2026},
	}
	for name, want := range movies {
		if got := Parse(name); got != want {
			t.Errorf("Parse(%q) = %+v, want %+v", name, got, want)
		}
	}
	titles := []string{"The Challenger from England", "The Shocking New MFG Generation", "The Kamaboko Straight", "Tire Management", "Teamwork"}
	for i, title := range titles {
		ep := i + 1
		name := "MF Ghost (2023) - S01E0" + string(rune('0'+ep)) + ".00" + string(rune('0'+ep)) + " - " + title +
			" [Bluray-1080p Remux][8bit][h264][FLAC 2.0][JA+EN]-CRUCiBLE.mkv"
		want := Parsed{Kind: KindTV, Title: "MF Ghost", ShowTitle: "MF Ghost", Year: 2023, Season: 1, Episode: ep}
		if got := Parse(name); got != want {
			t.Errorf("Parse(%q) = %+v, want %+v", name, got, want)
		}
		rel := "Series/MF Ghost/Season 01/" + name
		if s, ok := SeasonFromPath(rel); !ok || s != 1 {
			t.Errorf("SeasonFromPath(%q) = %d, %v", rel, s, ok)
		}
		if IsExtra(rel) {
			t.Errorf("episode classified as extra: %q", rel)
		}
	}
}
