package media

import (
	"context"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func load(t *testing.T, name string) *Info {
	t.Helper()
	data, err := os.ReadFile("testdata/" + name + ".ffprobe.json")
	if err != nil {
		t.Fatal(err)
	}
	info, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	return info
}

func near(a, b float64) bool { return math.Abs(a-b) < 0.001 }

// The four golden files are real ffprobe 8.1 output for the recon library
// (format.filename removed): one local MKV, one local MP4, two remote .strm targets.

func TestParseMFGhostMKV(t *testing.T) {
	i := load(t, "mfghost-s01e01")
	if i.Container != "matroska,webm" || i.Size != 6791180872 || i.Bitrate != 38207302 ||
		i.Duration != 1421965*time.Millisecond || i.Attachments != 20 || len(i.Streams) != 24 {
		t.Fatalf("format: %+v (streams %d)", *i, len(i.Streams))
	}
	if len(i.Chapters) != 5 || i.Chapters[0] != (Chapter{Start: 0, Title: "Prologue"}) || i.Chapters[1].Start != 89006*time.Millisecond {
		t.Errorf("chapters: %+v", i.Chapters)
	}
	v := i.Streams[0]
	if v.Type != StreamVideo || v.Codec != "h264" || v.Profile != "High" || v.Level != 41 || v.Width != 1920 || v.Height != 1080 ||
		v.BitDepth != 8 || v.PixelFormat != "yuv420p" || !near(v.AverageFrameRate, 23.976) || v.VideoRange != "SDR" ||
		v.VideoRangeType != "SDR" || v.Bitrate != 36600631 || !v.IsDefault || v.Title != "JPBD" {
		t.Errorf("video: %+v", v)
	}
	a := i.Streams[1]
	if a.Type != StreamAudio || a.Codec != "flac" || a.Language != "jpn" || a.Title != "Japanese FLAC 2.0" || a.Channels != 2 ||
		a.ChannelLayout != "stereo" || a.SampleRate != 48000 || !a.IsDefault || !a.IsOriginal || a.BitDepth != 0 || a.Bitrate != 1330821 {
		t.Errorf("flac audio: %+v", a)
	}
	if e := i.Streams[2]; e.Codec != "eac3" || e.Bitrate != 224000 || e.IsDefault {
		t.Errorf("eac3 audio: %+v", e)
	}
	subs := 0
	for _, s := range i.Streams {
		if s.Type == StreamSubtitle {
			subs++
			if !s.IsTextSubtitle() {
				t.Errorf("ASS/SRT stream %d not text: %s", s.Index, s.Codec)
			}
		}
	}
	if subs != 21 || i.Streams[3].Codec != "ass" || !i.Streams[3].IsDefault || i.Streams[3].Language != "eng" {
		t.Errorf("subtitles: %d, first %+v", subs, i.Streams[3])
	}
}

func TestParseAV1MP4(t *testing.T) {
	i := load(t, "mortal-kombat-ii")
	if i.Container != "mov,mp4,m4a,3gp,3g2,mj2" || len(i.Streams) != 2 || len(i.Chapters) != 0 {
		t.Fatalf("format: %+v", *i)
	}
	v, a := i.Streams[0], i.Streams[1]
	if v.Codec != "av1" || v.Profile != "Main" || v.Height != 804 || v.Bitrate != 9464833 || v.ColorTransfer != "bt709" ||
		v.VideoRangeType != "SDR" || v.Language != "und" || !near(v.RealFrameRate, 24) {
		t.Errorf("video: %+v", v)
	}
	if a.Codec != "aac" || a.Profile != "LC" || a.Channels != 2 || a.Bitrate != 160461 {
		t.Errorf("audio: %+v", a)
	}
}

func TestParseDolbyVisionRemote(t *testing.T) {
	i := load(t, "luca-strm")
	v := i.Streams[0]
	if v.Codec != "hevc" || v.Profile != "Main 10" || v.Width != 3840 || v.BitDepth != 10 || v.ColorTransfer != "smpte2084" ||
		v.ColorPrimaries != "bt2020" || v.VideoRange != "HDR" || v.VideoRangeType != "DOVIWithHDR10" {
		t.Errorf("video: %+v", v)
	}
	if v.DoVi == nil || *v.DoVi != (DolbyVision{Profile: 8, Level: 6, BLSignalCompatibilityID: 1, VersionMajor: 1, RPU: true, BL: true}) {
		t.Errorf("dovi: %+v", v.DoVi)
	}
	if a := i.Streams[1]; a.Codec != "ac3" || a.Channels != 6 || a.ChannelLayout != "5.1(side)" || a.Language != "ita" {
		t.Errorf("audio: %+v", a)
	}
	if len(i.Chapters) != 37 || i.Chapters[0].Title != "Capitolo 01" || i.Bitrate != 7904052 {
		t.Errorf("chapters %d, bitrate %d", len(i.Chapters), i.Bitrate)
	}
}

func TestParsePGSRemote(t *testing.T) {
	i := load(t, "little-fockers-strm")
	s := i.Streams[2]
	if s.Type != StreamSubtitle || s.Codec != "hdmv_pgs_subtitle" || s.IsTextSubtitle() || s.Title != "English SDH" || s.Language != "eng" {
		t.Errorf("PGS subtitle: %+v", s)
	}
	if v := i.Streams[0]; v.Codec != "hevc" || v.Profile != "Main" || v.VideoRangeType != "SDR" || v.Bitrate != 0 {
		t.Errorf("video: %+v (no bit_rate and no BPS tag → 0)", v)
	}
}

func TestVideoRange(t *testing.T) {
	cases := []struct {
		transfer   string
		dv         *DolbyVision
		rng, rtype string
	}{
		{"", nil, "SDR", "SDR"},
		{"bt709", nil, "SDR", "SDR"},
		{"smpte2084", nil, "HDR", "HDR10"},
		{"arib-std-b67", nil, "HDR", "HLG"},
		{"smpte2084", &DolbyVision{Profile: 8, BLSignalCompatibilityID: 1}, "HDR", "DOVIWithHDR10"},
		{"smpte2084", &DolbyVision{Profile: 7, BLSignalCompatibilityID: 6, EL: true}, "HDR", "DOVIWithEL"},
		{"arib-std-b67", &DolbyVision{Profile: 8, BLSignalCompatibilityID: 4}, "HDR", "DOVIWithHLG"},
		{"bt709", &DolbyVision{Profile: 8, BLSignalCompatibilityID: 2}, "SDR", "DOVIWithSDR"},
		{"", &DolbyVision{Profile: 5}, "HDR", "DOVI"},
	}
	for _, c := range cases {
		if r, rt := videoRange(c.transfer, c.dv); r != c.rng || rt != c.rtype {
			t.Errorf("videoRange(%q, %+v) = %s/%s, want %s/%s", c.transfer, c.dv, r, rt, c.rng, c.rtype)
		}
	}
}

func TestHelpers(t *testing.T) {
	if !near(rate("24000/1001"), 23.976) || rate("0/0") != 0 || rate("25") != 25 || rate("x/y") != 0 {
		t.Error("rate")
	}
	for in, want := range map[[2]string]int{
		{"8", "yuv420p"}: 8, {"", "yuv420p10le"}: 10, {"", "yuv444p12le"}: 12, {"", "yuv420p"}: 8, {"", ""}: 0,
	} {
		if got := bitDepth(in[0], in[1]); got != want {
			t.Errorf("bitDepth(%q,%q) = %d", in[0], in[1], got)
		}
	}
	if seconds("-1") != 0 || seconds("nope") != 0 || seconds("1.5") != 1500*time.Millisecond {
		t.Error("seconds")
	}
	if _, err := Parse([]byte("{")); err == nil {
		t.Error("bad JSON accepted")
	}
	if i, err := Parse([]byte(`{"format":{},"streams":[{"index":0,"codec_type":"video","codec_name":"mjpeg","disposition":{"attached_pic":1}}]}`)); err != nil || i.Streams[0].Type != StreamEmbeddedImage || i.Streams[0].VideoRange != "" {
		t.Errorf("cover art: %+v %v", i, err)
	}
}

func TestProbeArgs(t *testing.T) {
	var gotBin string
	var gotArgs []string
	p := Prober{Binary: "/usr/lib/jellyfin-ffmpeg/ffprobe", run: func(_ context.Context, bin string, args []string) ([]byte, error) {
		gotBin, gotArgs = bin, args
		return []byte(`{"format":{"format_name":"matroska,webm"},"streams":[]}`), nil
	}}
	info, err := p.Probe(t.Context(), "http://jellybird:8097/stream/x", Options{Remote: true})
	if err != nil || info.Container != "matroska,webm" {
		t.Fatal(info, err)
	}
	if gotBin != "/usr/lib/jellyfin-ffmpeg/ffprobe" || !slices.Contains(gotArgs, "-probesize") || !slices.Contains(gotArgs, "-show_chapters") ||
		gotArgs[len(gotArgs)-1] != "http://jellybird:8097/stream/x" {
		t.Errorf("%s %v", gotBin, gotArgs)
	}
	_, _ = Prober{run: p.run}.Probe(t.Context(), "/media/x.mkv", Options{})
	if gotBin != "ffprobe" || slices.Contains(gotArgs, "-probesize") {
		t.Errorf("local defaults: %s %v", gotBin, gotArgs)
	}
	boom := errors.New("boom")
	if _, err := (Prober{run: func(context.Context, string, []string) ([]byte, error) { return nil, boom }}).Probe(t.Context(), "x", Options{}); !errors.Is(err, boom) {
		t.Errorf("runner error lost: %v", err)
	}
}

func needFFmpeg(t *testing.T) {
	t.Helper()
	for _, b := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(b); err != nil {
			t.Skip(b + " not installed")
		}
	}
}

// Ported from jollyrogarr: generate a tiny MP4 with a mov_text subtitle,
// serve it over HTTP with Range support (as a remote .strm target would be),
// and probe it with the real binary.
func TestProbeRealFFprobe(t *testing.T) {
	needFFmpeg(t)
	dir := t.TempDir()
	mp4 := filepath.Join(dir, "test.mp4")
	srt := filepath.Join(dir, "sub.srt")
	if err := os.WriteFile(srt, []byte("1\n00:00:00,000 --> 00:00:00,900\nhello\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	gen := exec.CommandContext(ctx, "ffmpeg", "-y",
		"-f", "lavfi", "-i", "testsrc=duration=1:size=64x48:rate=5",
		"-f", "lavfi", "-i", "sine=duration=1:sample_rate=8000",
		"-i", srt,
		"-map", "0", "-map", "1", "-map", "2",
		"-c:v", "libx264", "-pix_fmt", "yuv420p", "-c:a", "aac", "-c:s", "mov_text",
		"-metadata:s:s:0", "language=eng", "-disposition:s:0", "default",
		"-shortest", mp4)
	if out, err := gen.CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg: %v\n%s", err, out)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, mp4)
	}))
	defer srv.Close()

	info, err := Prober{}.Probe(ctx, srv.URL+"/test.mp4", Options{Remote: true})
	if err != nil {
		t.Fatal(err)
	}
	if info.Container != "mov,mp4,m4a,3gp,3g2,mj2" || len(info.Streams) != 3 || info.Size == 0 || info.Duration <= 0 {
		t.Fatalf("info: %+v", *info)
	}
	v, a, s := info.Streams[0], info.Streams[1], info.Streams[2]
	if v.Codec != "h264" || v.Width != 64 || v.Height != 48 || v.BitDepth != 8 || v.VideoRangeType != "SDR" {
		t.Errorf("video: %+v", v)
	}
	if a.Codec != "aac" || a.Channels != 1 || a.SampleRate != 8000 {
		t.Errorf("audio: %+v", a)
	}
	if s.Codec != "mov_text" || !s.IsTextSubtitle() || s.Language != "eng" || !s.IsDefault {
		t.Errorf("subtitle: %+v", s)
	}
}

// A remote target that never answers must not hang the scanner: the
// caller's context deadline kills ffprobe (DESIGN §6: 15s for remote probes).
func TestProbeHonoursContextDeadline(t *testing.T) {
	needFFmpeg(t)
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-block }))
	defer srv.Close()
	defer close(block)

	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	start := time.Now()
	_, err := Prober{}.Probe(ctx, srv.URL+"/stuck.mkv", Options{Remote: true})
	if err == nil || !strings.Contains(err.Error(), "deadline") {
		t.Errorf("want deadline error, got %v", err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("probe took %v after a 1s deadline", took)
	}
}
