package subtitles

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const srt = "1\n00:00:01,000 --> 00:00:03,000\nHello\n\n2\n00:00:06,000 --> 00:00:08,000\nWorld\n"

const ass = `[Script Info]
ScriptType: v4.00+

[V4+ Styles]
Format: Name, Fontname, Fontsize, PrimaryColour, SecondaryColour, OutlineColour, BackColour, Bold, Italic, Underline, StrikeOut, ScaleX, ScaleY, Spacing, Angle, BorderStyle, Outline, Shadow, Alignment, MarginL, MarginR, MarginV, Encoding
Style: Default,Arial,40,&H00FFFFFF,&H000000FF,&H00000000,&H00000000,0,0,0,0,100,100,0,0,1,2,0,2,10,10,10,1

[Events]
Format: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text
Dialogue: 0,0:00:02.00,0:00:04.00,Default,,0,0,0,,Styled line
`

// mkvWithSubs makes a 10s video with an SRT track (stream 1) and an ASS
// track (stream 2).
func mkvWithSubs(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.srt"), []byte(srt), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "b.ass"), []byte(ass), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "movie.mkv")
	if b, err := exec.CommandContext(t.Context(), "ffmpeg", "-v", "error", "-f", "lavfi", "-i", "testsrc=duration=10:size=160x120:rate=10",
		"-i", filepath.Join(dir, "a.srt"), "-i", filepath.Join(dir, "b.ass"),
		"-map", "0", "-map", "1", "-map", "2", "-c:v", "libx264", "-preset", "ultrafast", "-c:s", "copy", out).CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, b)
	}
	return out
}

// countRuns counts ffmpeg runs for the test's duration.
func countRuns(t *testing.T) *atomic.Int32 {
	t.Helper()
	var n atomic.Int32
	orig := runFFmpeg
	runFFmpeg = func(ctx context.Context, args ...string) ([]byte, error) {
		n.Add(1)
		return orig(ctx, args...)
	}
	t.Cleanup(func() { runFFmpeg = orig })
	return &n
}

func TestExtractAndConvert(t *testing.T) {
	input := mkvWithSubs(t)
	runs := countRuns(t)
	s := &Store{Dir: t.TempDir()}
	sidecar := filepath.Join(t.TempDir(), "movie.en.srt")
	if err := os.WriteFile(sidecar, []byte(srt), 0o644); err != nil {
		t.Fatal(err)
	}
	src := Source{Key: "k1", Input: input, Tracks: []Track{
		{Index: 1, Codec: "subrip"}, {Index: 2, Codec: "ass"}, {Index: 3, Codec: "hdmv_pgs_subtitle"}, {Index: 4, Codec: "subrip", Path: sidecar},
	}}

	// Parallel first requests share one extraction of both text tracks.
	var wg sync.WaitGroup
	for _, idx := range []int{1, 2, 1, 2} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.File(t.Context(), src, idx); err != nil {
				t.Errorf("track %d: %v", idx, err)
			}
		}()
	}
	wg.Wait()
	if n := runs.Load(); n != 1 {
		t.Errorf("ffmpeg ran %d times, want one pass", n)
	}
	srtFile, _ := s.File(t.Context(), src, 1)
	assFile, _ := s.File(t.Context(), src, 2)
	if runs.Load() != 1 || filepath.Ext(srtFile) != ".srt" || filepath.Ext(assFile) != ".ass" {
		t.Errorf("cached files: %s %s (%d runs)", srtFile, assFile, runs.Load())
	}
	if b, _ := os.ReadFile(assFile); !strings.Contains(string(b), "Styled line") || !strings.Contains(string(b), "[V4+ Styles]") {
		t.Errorf("ASS kept as is: %s", b)
	}

	// Same format: the file as is. Other formats and offsets: converted.
	if b, _ := Convert(t.Context(), srtFile, "srt", 0); !strings.Contains(string(b), "00:00:01,000 --> 00:00:03,000") {
		t.Errorf("srt: %s", b)
	}
	vtt, err := Convert(t.Context(), srtFile, "webvtt", 0)
	if err != nil || !strings.HasPrefix(string(vtt), "WEBVTT") || !strings.Contains(string(vtt), "00:01.000 --> 00:03.000") {
		t.Errorf("vtt: %v %s", err, vtt)
	}
	late, err := Convert(t.Context(), srtFile, "srt", 5*time.Second)
	if err != nil || strings.Contains(string(late), "Hello") || !strings.Contains(string(late), "1\n00:00:01,000 --> 00:00:03,000\nWorld") {
		t.Errorf("from 5s: %v %s", err, late)
	}

	if f, err := s.File(t.Context(), src, 4); err != nil || f != sidecar {
		t.Errorf("sidecar: %s %v", f, err)
	}
	if _, err := s.File(t.Context(), src, 3); !errors.Is(err, ErrNotText) {
		t.Errorf("PGS: %v", err)
	}
	if _, err := s.File(t.Context(), src, 9); err == nil {
		t.Error("missing track")
	}

	// A copy with a safe name, stable for the same file.
	c1, err := s.Copy(sidecar)
	c2, _ := s.Copy(sidecar)
	if err != nil || c1 != c2 || !strings.HasPrefix(c1, s.Dir) || filepath.Ext(c1) != ".srt" {
		t.Errorf("copy: %s %s %v", c1, c2, err)
	}
	if !IsText("SUBRIP") || IsText("dvd_subtitle") || ContentType("webvtt") != "text/vtt; charset=utf-8" {
		t.Error("helpers")
	}
}

func TestShiftCues(t *testing.T) {
	srtIn := "1\n00:00:01,000 --> 00:00:03,000\nHello\n\n2\n00:00:04,000 --> 00:00:07,500\nSpans\n\n3\n01:00:06,250 --> 01:00:08,000\nLater\n"
	if got := string(shiftCues([]byte(srtIn), "srt", 5*time.Second)); got != "1\n00:00:00,000 --> 00:00:02,500\nSpans\n\n2\n01:00:01,250 --> 01:00:03,000\nLater\n" {
		t.Errorf("srt:\n%q", got)
	}
	vttIn := "WEBVTT\n\n00:01.000 --> 00:03.000\nHello\n\n00:06.000 --> 00:08.000 align:start\nWorld\n"
	if got := string(shiftCues([]byte(vttIn), "webvtt", 5*time.Second)); got != "WEBVTT\n\n00:00:01.000 --> 00:00:03.000 align:start\nWorld\n" {
		t.Errorf("vtt:\n%q", got)
	}
	assIn := "[Events]\nFormat: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text\nDialogue: 0,0:00:02.00,0:00:04.00,Default,,0,0,0,,Gone\nDialogue: 0,0:00:09.50,0:00:12.00,Default,,0,0,0,,Kept, with commas\n"
	if got := string(shiftCues([]byte(assIn), "ass", 5*time.Second)); got != "[Events]\nFormat: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text\nDialogue: 0,0:00:04.50,0:00:07.00,Default,,0,0,0,,Kept, with commas\n" {
		t.Errorf("ass:\n%q", got)
	}
}

func TestRemoteTrack(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.URL.Path != "/sub/1" {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, "1\n00:00:01,000 --> 00:00:02,000\nHello\n")
	}))
	t.Cleanup(srv.Close)
	s := &Store{Dir: t.TempDir()}
	src := Source{Key: "k", Tracks: []Track{{Index: 100, Codec: "subrip", Path: srv.URL + "/sub/1"}, {Index: 101, Codec: "subrip", Path: srv.URL + "/missing"}}}
	for range 2 {
		f, err := s.File(t.Context(), src, 100)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasSuffix(f, ".srt") || !strings.HasPrefix(f, s.Dir) {
			t.Errorf("file = %s", f)
		}
		body, err := Convert(t.Context(), f, "srt", 0)
		if err != nil || !strings.Contains(string(body), "Hello") {
			t.Errorf("Convert = %q, %v", body, err)
		}
	}
	if hits.Load() != 1 {
		t.Errorf("downloaded %d times, want once", hits.Load())
	}
	if _, err := s.File(t.Context(), src, 101); err == nil {
		t.Error("a 404 subtitle gave a file")
	}
}
