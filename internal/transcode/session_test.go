package transcode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestBuildArgs(t *testing.T) {
	dir := "/cache/transcode/abc"
	for _, c := range []struct {
		name      string
		opts      StartOptions
		want, not []string
	}{
		{"remux local, chosen audio copied", StartOptions{Input: "/media/a.mkv", CopyVideo: true, CopyAudio: true, AudioStream: 2},
			[]string{"-i /media/a.mkv -map 0:v:0 -map 0:2 -c:v copy -c:a copy", "-hls_time 3 ", "-start_number 0 ", "-hls_flags temp_file", dir + "/%d.ts " + dir + "/index.m3u8"},
			[]string{"-ss", "-force_key_frames", "-reconnect", "-init_hw_device"}},
		{"qsv at the client's bitrate from segment 10", StartOptions{Input: "/media/a.mp4", Encoder: CapQSV, Device: "/dev/dri/renderD128",
			VideoBitrate: 5_839_539, AudioStream: -1, AudioCodec: "aac", AudioBitrate: 160_461, AudioChannels: 2, StartSegment: 10},
			[]string{"-ss 30.000 -init_hw_device qsv=qs:/dev/dri/renderD128 -filter_hw_device qs -i /media/a.mp4 -map 0:v:0 -map 0:a:0?",
				"-vf format=nv12,hwupload=extra_hw_frames=64 -c:v h264_qsv -b:v 5839539 -maxrate 5839539 -bufsize 11679078",
				"-force_key_frames expr:gte(t,n_forced*3) -forced_idr 1", "-c:a aac -b:a 160461 -ac 2", "-output_ts_offset 30.000", "-start_number 10 "},
			[]string{"-global_quality", "-preset"}},
		{"remote input reconnects", StartOptions{Input: "https://cdn/x.mkv", Remote: true, CopyVideo: true, AudioStream: 1},
			[]string{"-reconnect 1 -reconnect_streamed 1 -reconnect_on_network_error 1 -reconnect_delay_max 5 -i https://cdn/x.mkv"}, nil},
		{"software HEVC→H.264 with tonemap, quality mode", StartOptions{Input: "/m/luca.mkv", Tonemap: true, AudioStream: 1, SegmentSeconds: 4},
			[]string{"-vf " + tonemapChain + ",format=yuv420p -c:v libx264 -crf 21 -preset veryfast", "-force_key_frames expr:gte(t,n_forced*4)", "-hls_time 4 "}, nil},
		{"PGS overlay before the QSV upload, after the tonemap", StartOptions{Input: "/m/x.mkv", Encoder: CapQSV, Device: "/dev/dri/renderD128",
			Tonemap: true, BurnImage: ptrInt(4), AudioStream: 1},
			[]string{"-filter_complex [0:v:0]" + tonemapChain + "[base];[base][0:4]overlay=eof_action=pass,format=nv12,hwupload=extra_hw_frames=64[vout] -map [vout] -map 0:1"},
			[]string{"-vf", "-map 0:v:0"}},
		{"text burn after a seek keeps source times", StartOptions{Input: "/m/x.mkv", BurnText: "/c/subs/3.ass", AudioStream: 1, StartSegment: 4},
			[]string{"-vf setpts=PTS+12.000/TB,subtitles=filename=/c/subs/3.ass,setpts=PTS-12.000/TB,format=yuv420p"}, nil},
		{"vaapi hevc", StartOptions{Input: "/m/a.mkv", Encoder: CapVAAPI, Device: "/dev/dri/renderD128", VideoCodec: "hevc", AudioStream: 1, CopyAudio: true},
			[]string{"-init_hw_device vaapi=va:/dev/dri/renderD128", "-vf format=nv12,hwupload -c:v hevc_vaapi -qp 23"}, []string{"forced_idr", "forced-idr"}},
		{"qsv decodes hevc on the gpu, frames never leave it", StartOptions{Input: "/m/a.mkv", Encoder: CapQSV, Device: "/dev/dri/renderD128", SourceCodec: "hevc", AudioStream: 1},
			[]string{"-init_hw_device vaapi=va:/dev/dri/renderD128 -init_hw_device qsv=qs@va -filter_hw_device qs -hwaccel qsv -hwaccel_output_format qsv -i /m/a.mkv",
				"-vf scale_qsv=format=nv12 -c:v h264_qsv"}, []string{"hwupload"}},
		{"vaapi decodes h264 on the gpu", StartOptions{Input: "/m/a.mkv", Encoder: CapVAAPI, Device: "/dev/dri/renderD128", SourceCodec: "h264", VideoBitrate: 4_000_000, AudioStream: -1},
			[]string{"-hwaccel vaapi -hwaccel_output_format vaapi -i /m/a.mkv", "-vf scale_vaapi=format=nv12 -c:v h264_vaapi"}, []string{"hwupload"}},
		{"tonemap keeps software decode", StartOptions{Input: "/m/a.mkv", Encoder: CapQSV, Device: "/dev/dri/renderD128", SourceCodec: "hevc", Tonemap: true, AudioStream: 1},
			[]string{"-vf " + tonemapChain + ",format=nv12,hwupload=extra_hw_frames=64"}, []string{"-hwaccel"}},
		{"a codec the gpu doesn't decode", StartOptions{Input: "/m/a.mkv", Encoder: CapQSV, Device: "/dev/dri/renderD128", SourceCodec: "mpeg4", AudioStream: 1},
			[]string{"-init_hw_device qsv=qs:/dev/dri/renderD128", "-vf format=nv12,hwupload=extra_hw_frames=64"}, []string{"-hwaccel"}},
	} {
		got := strings.Join(buildArgs(c.opts, dir), " ") + " "
		for _, w := range c.want {
			if !strings.Contains(got, w) {
				t.Errorf("%s: missing %q in\n%s", c.name, w, got)
			}
		}
		for _, n := range c.not {
			if strings.Contains(got, n) {
				t.Errorf("%s: unexpected %q in\n%s", c.name, n, got)
			}
		}
	}
}

func needFFmpeg(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
}

// testSource makes a 20s 320×240 H.264 video with two stereo AAC tracks
// (stream 1: 440 Hz, stream 2: 880 Hz) in Matroska.
func testSource(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "source.mkv")
	out, err := exec.CommandContext(t.Context(), "ffmpeg", "-v", "error", "-f", "lavfi", "-i", "testsrc=duration=20:size=320x240:rate=24",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=20", "-f", "lavfi", "-i", "sine=frequency=880:duration=20",
		"-map", "0", "-map", "1", "-map", "2", "-c:v", "libx264", "-preset", "ultrafast", "-g", "24",
		"-c:a", "aac", "-ac", "2", p).CombinedOutput()
	if err != nil {
		t.Fatalf("make test source: %v: %s", err, out)
	}
	return p
}

type probed struct {
	Streams []struct {
		CodecName, CodecType string
		Channels             int
	} `json:"streams"`
	Format struct{ StartTime, Duration string } `json:"format"`
}

func probe(t *testing.T, path string) probed {
	t.Helper()
	out, err := exec.CommandContext(t.Context(), "ffprobe", "-v", "error", "-show_streams", "-show_format", "-of", "json", path).Output()
	if err != nil {
		t.Fatalf("ffprobe %s: %v", path, err)
	}
	var p probed
	// ffprobe uses snake_case.
	out = bytes.ReplaceAll(out, []byte(`"codec_name"`), []byte(`"CodecName"`))
	out = bytes.ReplaceAll(out, []byte(`"codec_type"`), []byte(`"CodecType"`))
	out = bytes.ReplaceAll(out, []byte(`"start_time"`), []byte(`"StartTime"`))
	if err := json.Unmarshal(out, &p); err != nil {
		t.Fatal(err)
	}
	return p
}

func waitDone(t *testing.T, s *Session) {
	t.Helper()
	select {
	case <-s.done:
	case <-time.After(60 * time.Second):
		t.Fatal("ffmpeg didn't finish")
	}
	if st, err := s.State(); st != StateDone {
		t.Fatalf("state %s: %v", st, err)
	}
}

func TestSessionRemuxRealFFmpeg(t *testing.T) {
	needFFmpeg(t)
	src := testSource(t)
	m := NewManager(t.TempDir(), 0)
	s, err := m.Start(t.Context(), "remux1", StartOptions{Input: src, CopyVideo: true, CopyAudio: true, AudioStream: 2, SegmentSeconds: 2}, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close(s.ID) })
	waitDone(t, s)
	if n := s.Next(); n < 9 || n > 11 {
		t.Errorf("about 10 two-second segments: %d", n)
	}
	p := probe(t, s.SegmentPath(0))
	if len(p.Streams) != 2 || p.Streams[0].CodecName != "h264" || p.Streams[1].CodecName != "aac" {
		t.Errorf("remuxed segment: %+v", p.Streams)
	}
	if s.HasSegment(99) {
		t.Error("segment 99")
	}
}

func TestSessionTranscodeAndRestartRealFFmpeg(t *testing.T) {
	needFFmpeg(t)
	src := testSource(t)
	m := NewManager(t.TempDir(), 0)
	opts := StartOptions{Input: src, Encoder: CapSoftware, VideoBitrate: 300_000, AudioStream: 2, AudioBitrate: 64_000, AudioChannels: 1, SegmentSeconds: 2}
	s, err := m.Start(t.Context(), "tc1", opts, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close("tc1") })
	waitDone(t, s)
	p := probe(t, s.SegmentPath(1))
	if len(p.Streams) != 2 || p.Streams[0].CodecName != "h264" || p.Streams[1].Channels != 1 {
		t.Errorf("transcoded segment: %+v", p.Streams)
	}
	// Forced keyframes: every segment but the last is exactly 2s.
	for _, n := range []int{0, 3, 7} {
		if d := probe(t, s.SegmentPath(n)).Format.Duration; !strings.HasPrefix(d, "2.0") && !strings.HasPrefix(d, "1.99") {
			t.Errorf("segment %d lasts %s", n, d)
		}
	}

	// A seek restarts at segment 6 on the same timeline; earlier segments stay.
	if err := os.Remove(s.SegmentPath(6)); err != nil {
		t.Fatal(err)
	}
	s, ok, err := m.Restart(t.Context(), "tc1", 6, 30*time.Second)
	if !ok || err != nil {
		t.Fatal(ok, err)
	}
	waitDone(t, s)
	if !s.HasSegment(0) || !s.HasSegment(6) || s.Opts.StartSegment != 6 || s.Opts.VideoBitrate != 300_000 {
		t.Errorf("restart: %+v", s.Opts)
	}
	start := probe(t, s.SegmentPath(6)).Format.StartTime
	if !strings.HasPrefix(start, "12.") && !strings.HasPrefix(start, "13.") { // 6×2s plus the mpegts muxer delay
		t.Errorf("segment 6 starts at %s, want ≈12s", start)
	}
}

func TestSessionFailureRealFFmpeg(t *testing.T) {
	needFFmpeg(t)
	m := NewManager(t.TempDir(), 0)
	start := time.Now()
	_, err := m.Start(t.Context(), "bad", StartOptions{Input: "/nonexistent/x.mkv", CopyVideo: true, CopyAudio: true, AudioStream: -1}, 30*time.Second)
	if err == nil || !strings.Contains(err.Error(), "No such file") || time.Since(start) > 10*time.Second {
		t.Errorf("missing input: %v after %s", err, time.Since(start))
	}
	if m.Count() != 0 {
		t.Errorf("failed session kept")
	}
	if _, err := m.Start(t.Context(), "../escape", StartOptions{}, time.Second); !errors.Is(err, ErrBadSessionID) {
		t.Errorf("unsafe id: %v", err)
	}
}

// stubFFmpeg replaces ffmpeg with a process that writes the first segment
// and then waits to be killed.
func stubFFmpeg(t *testing.T) {
	t.Helper()
	orig := runFFmpegSession
	t.Cleanup(func() { runFFmpegSession = orig })
	runFFmpegSession = func(ctx context.Context, args []string, stderr *bytes.Buffer) (*exec.Cmd, error) {
		i := slices.Index(args, "-hls_segment_filename")
		start := args[slices.Index(args, "-start_number")+1]
		seg := strings.Replace(args[i+1], "%d", start, 1)
		cmd := exec.CommandContext(ctx, "sh", "-c", "printf x > '"+seg+"'; exec sleep 60")
		cmd.Stderr = stderr
		return cmd, cmd.Start()
	}
}

func TestManagerLimitsAndIdle(t *testing.T) {
	stubFFmpeg(t)
	orig := pollInterval
	pollInterval = 5 * time.Millisecond
	t.Cleanup(func() { pollInterval = orig })
	m := NewManager(t.TempDir(), 2)
	m.IdleTimeout = 200 * time.Millisecond
	opts := StartOptions{Input: "x", CopyVideo: true, AudioStream: -1}
	for _, id := range []string{"a", "b"} {
		if _, err := m.Start(t.Context(), id, opts, 5*time.Second); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := m.Start(t.Context(), "c", opts, 5*time.Second); !errors.Is(err, ErrTooManySessions) {
		t.Errorf("third session: %v", err)
	}
	if _, ok, err := m.Restart(t.Context(), "a", 4, 5*time.Second); !ok || err != nil { // replacing one is fine
		t.Errorf("restart at the limit: %v %v", ok, err)
	}
	other := opts
	other.Owner = "tv"
	m.Close("b")
	if _, err := m.Start(t.Context(), "b", other, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	m.CloseOwner("phone") // nobody's
	if m.Count() != 2 {
		t.Errorf("CloseOwner closed someone else's session")
	}
	m.CloseOwner("tv")
	if _, ok := m.Get("b"); ok || m.Count() != 1 {
		t.Errorf("CloseOwner(tv) left b")
	}
	if _, err := m.Start(t.Context(), "b", opts, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	dirA := filepath.Join(m.baseDir, "a")

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { m.Run(ctx); close(done) }()
	for range 50 { // keep b busy while a goes idle
		m.Get("b")
		time.Sleep(10 * time.Millisecond)
	}
	if _, ok := m.Get("a"); ok {
		t.Error("idle session a still running")
	}
	if _, err := os.Stat(dirA); !os.IsNotExist(err) {
		t.Errorf("idle session's directory kept: %v", err)
	}
	if _, ok := m.Get("b"); !ok {
		t.Error("busy session b reaped")
	}
	cancel()
	<-done
	if m.Count() != 0 {
		t.Errorf("sessions left after shutdown: %d", m.Count())
	}
}

func TestRemoveStale(t *testing.T) {
	m := NewManager(t.TempDir(), 0)
	if err := os.MkdirAll(filepath.Join(m.baseDir, "old", "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := m.RemoveStale(); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(m.baseDir); len(entries) != 0 {
		t.Errorf("left: %v", entries)
	}
	if err := NewManager(filepath.Join(m.baseDir, "missing"), 0).RemoveStale(); err != nil {
		t.Errorf("missing base dir: %v", err)
	}
}

// Real hardware: an H.264 QSV encode, when this machine has QSV.
func TestSessionQSVRealHardware(t *testing.T) {
	needFFmpeg(t)
	caps := Probe(t.Context(), "")
	if !caps.Has(CapQSV) {
		t.Skip("no QSV")
	}
	src := testSource(t)
	m := NewManager(t.TempDir(), 0)
	s, err := m.Start(t.Context(), "qsv", StartOptions{Input: src, Encoder: CapQSV, Device: caps.Device, SourceCodec: "h264", VideoBitrate: 1_000_000, AudioStream: 1, SegmentSeconds: 2}, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close("qsv") })
	waitDone(t, s)
	if p := probe(t, s.SegmentPath(0)); p.Streams[0].CodecName != "h264" {
		t.Errorf("qsv segment: %+v", p.Streams)
	}
	// Forced keyframes must be IDR for QSV, or segments run long.
	for _, n := range []int{0, 3} {
		if d := probe(t, s.SegmentPath(n)).Format.Duration; !strings.HasPrefix(d, "2.0") && !strings.HasPrefix(d, "1.99") {
			t.Errorf("qsv segment %d lasts %s", n, d)
		}
	}
}

// A session meant to decode on the GPU that can't start (a codec profile
// or driver quirk the hardware refuses) is retried once with software
// decode instead of failing the play.
func TestStartFallsBackToSoftwareDecode(t *testing.T) {
	orig := runFFmpegSession
	t.Cleanup(func() { runFFmpegSession = orig })
	var calls int
	runFFmpegSession = func(ctx context.Context, args []string, stderr *bytes.Buffer) (*exec.Cmd, error) {
		calls++
		if slices.Contains(args, "-hwaccel") { // die at once, like a refused decoder
			cmd := exec.CommandContext(ctx, "sh", "-c", "exit 1")
			cmd.Stderr = stderr
			return cmd, cmd.Start()
		}
		i := slices.Index(args, "-hls_segment_filename")
		start := args[slices.Index(args, "-start_number")+1]
		seg := strings.Replace(args[i+1], "%d", start, 1)
		cmd := exec.CommandContext(ctx, "sh", "-c", "printf x > '"+seg+"'; exec sleep 60")
		cmd.Stderr = stderr
		return cmd, cmd.Start()
	}
	m := NewManager(t.TempDir(), 0)
	s, err := m.Start(t.Context(), "fb", StartOptions{Input: "/m/a.mkv", Encoder: CapQSV, Device: "/dev/dri/renderD128", SourceCodec: "hevc", AudioStream: -1}, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close("fb") })
	if calls != 2 || !s.Opts.swDecode {
		t.Errorf("calls %d, swDecode %v; want one hw attempt then software", calls, s.Opts.swDecode)
	}
}

// An input that wouldn't open (a proxied URL answering 5XX) fails the
// same way in software, so the fallback leaves it alone: seen live, one
// flaky 5XX pinned a whole session to software decode for no reason.
func TestStartDoesNotFallBackOnInputFailure(t *testing.T) {
	orig := runFFmpegSession
	t.Cleanup(func() { runFFmpegSession = orig })
	var calls int
	runFFmpegSession = func(ctx context.Context, args []string, stderr *bytes.Buffer) (*exec.Cmd, error) {
		calls++
		cmd := exec.CommandContext(ctx, "sh", "-c", "echo 'Error opening input: Server returned 5XX Server Error reply' >&2; exit 1")
		cmd.Stderr = stderr
		return cmd, cmd.Start()
	}
	m := NewManager(t.TempDir(), 0)
	_, err := m.Start(t.Context(), "fbx", StartOptions{Input: "https://x/y.mkv", Encoder: CapQSV, Device: "/dev/dri/renderD128", SourceCodec: "hevc", AudioStream: -1}, 5*time.Second)
	if err == nil {
		t.Fatal("session started")
	}
	if calls != 1 {
		t.Errorf("calls %d; want the failure returned, not retried", calls)
	}
}

// 10-bit HEVC decodes on the GPU and scale_qsv makes NV12 of it there:
// validated against a 4K remux, 0.89 CPU-seconds per 6s of video where
// software decode costs 11.1.
func TestSessionQSVHWDecodeRealHardware(t *testing.T) {
	needFFmpeg(t)
	caps := Probe(t.Context(), "")
	if !caps.Has(CapQSV) {
		t.Skip("no QSV")
	}
	p := filepath.Join(t.TempDir(), "hevc10.mkv")
	if out, err := exec.CommandContext(t.Context(), "ffmpeg", "-v", "error", "-f", "lavfi",
		"-i", "testsrc2=duration=8:size=1280x720:rate=24", "-c:v", "libx265", "-preset", "ultrafast",
		"-pix_fmt", "yuv420p10le", p).CombinedOutput(); err != nil {
		t.Fatalf("make hevc source: %v: %s", err, out)
	}
	m := NewManager(t.TempDir(), 0)
	s, err := m.Start(t.Context(), "qsvhw", StartOptions{Input: p, Encoder: CapQSV, Device: caps.Device,
		SourceCodec: "hevc", VideoBitrate: 1_000_000, AudioStream: -1, SegmentSeconds: 2}, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close("qsvhw") })
	waitDone(t, s)
	if got := probe(t, s.SegmentPath(0)).Streams[0].CodecName; got != "h264" {
		t.Errorf("segment codec %s", got)
	}
}

// burnSource makes a 12s black video and a subtitle file showing a white
// line from 6.5s to 8s. (ffmpeg can't render text into image subtitles, so
// PGS overlay is covered by TestBuildArgs and a live check, DESIGN §8.3.)
func burnSource(t *testing.T) (video, srtFile string) {
	t.Helper()
	dir := t.TempDir()
	srtFile = filepath.Join(dir, "it's, a: [test].srt") // quotes, commas, colons, brackets
	if err := os.WriteFile(srtFile, []byte("1\n00:00:06,500 --> 00:00:08,000\nBURNED IN\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	video = filepath.Join(dir, "black.mkv")
	if out, err := exec.CommandContext(t.Context(), "ffmpeg", "-v", "error", "-f", "lavfi", "-i", "color=c=black:size=320x240:rate=10:duration=12",
		"-c:v", "libx264", "-preset", "ultrafast", video).CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	return video, srtFile
}

// brightest is the highest luma in a segment: ~16 for black, far more where
// white subtitles were burned in.
func brightest(t *testing.T, segment string) int {
	t.Helper()
	out, err := exec.CommandContext(t.Context(), "ffprobe", "-v", "error", "-f", "lavfi", "-i", "movie="+filterQuote(segment)+",signalstats",
		"-show_entries", "frame_tags=lavfi.signalstats.YMAX", "-of", "csv=p=0").Output()
	if err != nil {
		t.Fatalf("signalstats %s: %v", segment, err)
	}
	peak := 0
	for _, l := range strings.Fields(string(out)) {
		var v int
		_, _ = fmt.Sscan(l, &v)
		peak = max(peak, v)
	}
	return peak
}

func TestSessionBurnInRealFFmpeg(t *testing.T) {
	needFFmpeg(t)
	video, srtFile := burnSource(t)
	for _, c := range []struct {
		name  string
		opts  StartOptions
		start int
	}{
		{"none", StartOptions{}, 0},
		{"text", StartOptions{BurnText: srtFile}, 0},
		{"text after a seek", StartOptions{BurnText: srtFile}, 3}, // libass still sees source times
	} {
		o := c.opts
		o.Input, o.Encoder, o.AudioStream, o.SegmentSeconds, o.StartSegment = video, CapSoftware, -1, 2, c.start
		m := NewManager(t.TempDir(), 0)
		s, err := m.Start(t.Context(), "burn", o, 30*time.Second)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		waitDone(t, s)
		// Segment 3 covers 6–8s, where the line shows.
		lit, dark := brightest(t, s.SegmentPath(3)), brightest(t, s.SegmentPath(4))
		burned := c.name != "none"
		if (lit > 100) != burned || dark > 100 {
			t.Errorf("%s: segment 3 peak luma %d, segment 4 %d", c.name, lit, dark)
		}
		m.Close("burn")
	}
}

func ptrInt(n int) *int { return &n }

// A session racing ahead of the player is paused, writes nothing while
// held, and resumes when the player catches up (throttle.go).
func TestThrottle(t *testing.T) {
	orig := runFFmpegSession
	t.Cleanup(func() { runFFmpegSession = orig })
	runFFmpegSession = func(ctx context.Context, args []string, stderr *bytes.Buffer) (*exec.Cmd, error) {
		dir := filepath.Dir(args[slices.Index(args, "-hls_segment_filename")+1])
		start := args[slices.Index(args, "-start_number")+1]
		// A segment every 5 ms: far faster than real time, like a GPU encode.
		cmd := exec.CommandContext(ctx, "sh", "-c", `n=`+start+`; while :; do printf x > "`+dir+`/$n.ts"; n=$((n+1)); sleep 0.005; done`)
		cmd.Stderr = stderr
		return cmd, cmd.Start()
	}
	for _, v := range []*time.Duration{&throttleAhead, &throttleResume, &throttleEvery} {
		old := *v
		t.Cleanup(func() { *v = old })
	}
	throttleAhead, throttleResume, throttleEvery = 30*time.Second, 15*time.Second, 10*time.Millisecond // 10 and 5 three-second segments
	op := pollInterval
	pollInterval = 5 * time.Millisecond
	t.Cleanup(func() { pollInterval = op })

	m := NewManager(t.TempDir(), 0)
	m.IdleTimeout = time.Minute
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go m.Run(ctx)
	s, err := m.Start(t.Context(), "th", StartOptions{Input: "x", CopyVideo: true, AudioStream: -1, StartSegment: 100}, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	waitFor := func(what string, ok func() bool) {
		t.Helper()
		for deadline := time.Now().Add(5 * time.Second); !ok(); {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting: %s", what)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	waitFor("pause", s.Paused)
	held := s.Next()
	if held < 100+10 {
		t.Errorf("paused at segment %d, before 10 segments ahead of the start", held)
	}
	time.Sleep(150 * time.Millisecond)
	if got := s.Next(); got > held+1 { // at most the segment being written when stopped
		t.Errorf("wrote on while paused: %d → %d", held, got)
	}
	// The player gets within 5 segments: ffmpeg runs again, and is held
	// again once 10 ahead of it.
	s.Requested(held - 3)
	if s.Paused() {
		t.Error("still paused after the player caught up")
	}
	waitFor("progress after resume", func() bool { return s.Next() > held+3 })
	waitFor("pause again", s.Paused)
	if got := s.Next(); got < held-3+10 {
		t.Errorf("paused again at %d, before 10 ahead of segment %d", got, held-3)
	}
	m.Close("th") // a paused ffmpeg still stops
}
