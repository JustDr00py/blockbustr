package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sysadmin/blockbustr/internal/jfapi"
	"github.com/sysadmin/blockbustr/internal/store/pg/db"
	"github.com/sysadmin/blockbustr/internal/testutil"
	"github.com/sysadmin/blockbustr/internal/transcode"
)

const mkii = "1f77a87a9603bff7d1589d1e076f060c"

// hlsServer is the integration server with HLS on (software encoder,
// Jellyfin's 3s segments).
func hlsServer(t *testing.T) (http.Handler, Deps) {
	t.Helper()
	_, d := newIntegrationServer(t)
	seedRecon(t, d)
	seedCaptureToken(t, d)
	d.Transcoding = &Transcoding{Sessions: transcode.NewManager(t.TempDir(), 2), Encoder: transcode.CapSoftware, SegmentSeconds: 3}
	t.Cleanup(d.Transcoding.Sessions.CloseAll)
	rt := jfapi.NewRouter(testutil.Discard(), jfapi.Options{LegacyAuth: true})
	Register(rt, d)
	return rt, d
}

// hlsCapture is a captured HLS request with its query in the order the
// client sent it (maps lose it, and playlist sizes depend on it).
type hlsCapture struct {
	name, path, rawQuery, auth string
	status                     int
	headers                    map[string]string
	body                       string
}

func hlsCaptures(t *testing.T, pattern string) []hlsCapture {
	t.Helper()
	files, _ := filepath.Glob("../../../testdata/jellyfin/*/*/" + pattern)
	var out []hlsCapture
	for _, f := range files {
		if strings.Contains(f, "/_raw/") {
			continue
		}
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var c struct {
			Request struct {
				Path    string
				Query   json.RawMessage
				Headers map[string]string
			}
			Response struct {
				Status  int
				Headers map[string]string
				Body    json.RawMessage
			}
		}
		if err := json.Unmarshal(raw, &c); err != nil {
			t.Fatal(err)
		}
		h := hlsCapture{name: filepath.Base(f), path: c.Request.Path, auth: c.Request.Headers["Authorization"],
			status: c.Response.Status, headers: c.Response.Headers}
		var text struct {
			Text string `json:"_text"`
		}
		_ = json.Unmarshal(c.Response.Body, &text)
		h.body = text.Text
		// Walk the query object in document order.
		dec := json.NewDecoder(bytes.NewReader(c.Request.Query))
		_, _ = dec.Token()
		var parts []string
		for dec.More() {
			k, _ := dec.Token()
			var v []string
			_ = dec.Decode(&v)
			parts = append(parts, k.(string)+"="+strings.ReplaceAll(url.QueryEscape(v[0]), "%2C", ","))
		}
		h.rawQuery = strings.Join(parts, "&")
		out = append(out, h)
	}
	if len(out) == 0 {
		t.Fatalf("no captures for %s", pattern)
	}
	return out
}

func (c hlsCapture) replay(t *testing.T, h http.Handler) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(context.Background(), "GET", c.path+"?"+c.rawQuery, nil)
	if c.auth != "" {
		req.Header.Set("Authorization", c.auth)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// streamInf parses an #EXT-X-STREAM-INF line's attributes.
func streamInf(line string) map[string]string {
	out := map[string]string{}
	rest := strings.TrimPrefix(line, "#EXT-X-STREAM-INF:")
	for rest != "" {
		k, v, _ := strings.Cut(rest, "=")
		if strings.HasPrefix(v, `"`) {
			end := strings.Index(v[1:], `"`) + 1
			out[k], rest = v[1:end], strings.TrimPrefix(v[end+1:], ",")
			continue
		}
		val, next, _ := strings.Cut(v, ",")
		out[k], rest = val, next
	}
	return out
}

func TestHLSPlaylistsMatchJellyfin(t *testing.T) {
	h, _ := hlsServer(t)
	for _, c := range hlsCaptures(t, "*-GET-videos-id-master-m3u8.json") {
		rec := c.replay(t, h)
		if rec.Code != 200 || rec.Header().Get("Content-Type") != "application/vnd.apple.mpegurl" {
			t.Fatalf("%s: %d %v %s", c.name, rec.Code, rec.Header(), rec.Body)
		}
		got, want := strings.Split(rec.Body.String(), "\n"), strings.Split(c.body, "\n")
		if len(got) != len(want) || got[0] != "#EXTM3U" {
			t.Fatalf("%s:\n%s\nJellyfin:\n%s", c.name, rec.Body, c.body)
		}
		g, w := streamInf(got[1]), streamInf(want[1])
		for _, k := range []string{"BANDWIDTH", "AVERAGE-BANDWIDTH", "VIDEO-RANGE", "FRAME-RATE"} {
			if g[k] != w[k] {
				t.Errorf("%s: %s=%q, Jellyfin %q", c.name, k, g[k], w[k])
			}
		}
		// Jellyfin scales down for a low bitrate (Streamyfin at 4 Mbps got
		// 1280x536); blockbustr doesn't scale yet (P2.5 follow-up).
		if g["RESOLUTION"] != "1920x804" {
			t.Errorf("%s: RESOLUTION %s", c.name, g["RESOLUTION"])
		}
		gu, _ := url.Parse(got[2])
		wu, _ := url.Parse(want[2])
		if gu.Path != "main.m3u8" || gu.Query().Encode() != wu.Query().Encode() {
			t.Errorf("%s: variant\n  %s\n  Jellyfin %s", c.name, got[2], want[2])
		}
	}
	for _, c := range hlsCaptures(t, "*-GET-videos-id-main-m3u8.json") {
		rec := c.replay(t, h)
		if rec.Code != 200 || rec.Header().Get("Content-Type") != "application/vnd.apple.mpegurl" {
			t.Fatalf("%s: %d", c.name, rec.Code)
		}
		body := rec.Body.String()
		if !strings.HasPrefix(body, "#EXTM3U\n#EXT-X-PLAYLIST-TYPE:VOD\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:3\n#EXT-X-MEDIA-SEQUENCE:0\n#EXTINF:3.000000, nodesc\nhls1/main/0.ts?") ||
			!strings.HasSuffix(body, "&runtimeTicks=63570000000&actualSegmentLengthTicks=15920000\n#EXT-X-ENDLIST\n") || strings.Count(body, "#EXTINF") != 2120 {
			t.Errorf("%s: playlist\n%s…%s", c.name, body[:400], body[len(body)-200:])
		}
		// Streamyfin's request reproduces Jellyfin's playlist to the byte
		// count (Jellyfin Android's raw query can't be recovered exactly).
		if strings.Contains(c.name, "0094") && rec.Header().Get("Content-Length") != c.headers["Content-Length"] {
			t.Errorf("%s: %s bytes, Jellyfin %s", c.name, rec.Header().Get("Content-Length"), c.headers["Content-Length"])
		}
	}
}

func TestServesSoon(t *testing.T) {
	dir := t.TempDir()
	s := &transcode.Session{Dir: dir, Opts: transcode.StartOptions{StartSegment: 10}}
	for _, n := range []int{3, 10, 11, 12} {
		if err := os.WriteFile(s.SegmentPath(n), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for n, want := range map[int]bool{
		3:  true,  // kept from an earlier run
		4:  false, // behind this run's start: restart there
		11: true,  // written
		13: true,  // next up
		17: true,  // within the look-ahead
		18: false, // a seek forward
	} {
		if got := servesSoon(s, n); got != want {
			t.Errorf("segment %d: %v", n, got)
		}
	}
}

func TestHLSTranscodeRealFFmpeg(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	h, d := hlsServer(t)
	src := filepath.Join(t.TempDir(), "mkii.mkv")
	if out, err := exec.CommandContext(t.Context(), "ffmpeg", "-v", "error", "-f", "lavfi", "-i", "testsrc=duration=30:size=320x240:rate=24",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=30", "-map", "0", "-map", "1",
		"-c:v", "libx264", "-preset", "ultrafast", "-c:a", "aac", "-ac", "2", src).CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	execSQL(t, `UPDATE media_sources SET path_or_url = $1, runtime_ticks = 300000000 WHERE item_id = $2`, src, mkii)

	// PlaybackInfo for a client that can't play AV1 hands out the HLS URL.
	rec := call(t, h, "POST", "/Items/"+mkii+"/PlaybackInfo", `MediaBrowser Token="`+captureToken+`"`, `{"DeviceProfile":{
		"DirectPlayProfiles":[{"Type":"Video","Container":"mp4","VideoCodec":"h264","AudioCodec":"aac"}],
		"TranscodingProfiles":[{"Type":"Video","Container":"ts","Protocol":"hls","VideoCodec":"h264","AudioCodec":"aac"}]}}`)
	var pi playbackResp
	_ = json.Unmarshal(rec.Body.Bytes(), &pi)
	if len(pi.MediaSources) != 1 || pi.MediaSources[0].TranscodingUrl == "" {
		t.Fatalf("PlaybackInfo: %s", rec.Body)
	}
	tu := pi.MediaSources[0].TranscodingUrl

	// Players fetch with the ApiKey in the URL and no headers.
	get := func(path string, hdr ...string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequestWithContext(context.Background(), "GET", path, nil)
		if len(hdr) == 2 {
			req.Header.Set(hdr[0], hdr[1])
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	master := get(tu)
	lines := strings.Split(strings.TrimSpace(master.Body.String()), "\n")
	if master.Code != 200 || len(lines) != 3 || !strings.Contains(lines[1], `CODECS="avc1.640029,mp4a.40.2"`) {
		t.Fatalf("master: %d %s", master.Code, master.Body)
	}
	base := strings.TrimSuffix(strings.SplitN(tu, "?", 2)[0], "master.m3u8")
	main := get(base + lines[2])
	segs := []string{}
	for _, l := range strings.Split(main.Body.String(), "\n") {
		if strings.HasPrefix(l, "hls1/") {
			segs = append(segs, base+l)
		}
	}
	if main.Code != 200 || len(segs) != 10 {
		t.Fatalf("main: %d, %d segments\n%s", main.Code, len(segs), main.Body)
	}
	fetch := func(i int) []byte {
		t.Helper()
		rec := get(segs[i], "Range", "bytes=0-")
		body, _ := io.ReadAll(rec.Body)
		if rec.Code != 206 || rec.Header().Get("Content-Type") != "video/mp2t" || len(body) == 0 {
			t.Fatalf("segment %d: %d %v", i, rec.Code, rec.Header())
		}
		return body
	}
	first := fetch(0)
	p := filepath.Join(t.TempDir(), "0.ts")
	_ = os.WriteFile(p, first, 0o644)
	out, err := exec.CommandContext(t.Context(), "ffprobe", "-v", "error", "-show_entries", "stream=codec_name", "-of", "csv=p=0", p).Output()
	if err != nil || strings.Fields(string(out))[0] != "h264" || !strings.Contains(string(out), "aac") {
		t.Errorf("segment 0 streams: %q %v", out, err)
	}
	fetch(9) // the last one (a seek, or already written)
	fetch(1)
	if n := d.Transcoding.Sessions.Count(); n != 1 {
		t.Errorf("one ffmpeg session per play session: %d", n)
	}
	q, _ := url.ParseQuery(strings.SplitN(tu, "?", 2)[1])

	// A player still reporting progress keeps its transcode though it
	// fetches no segment (a long buffer, the throttle holding ffmpeg); once
	// it goes quiet the idle reaper closes it.
	m := d.Transcoding.Sessions
	oldIdle := m.IdleTimeout
	m.IdleTimeout = 400 * time.Millisecond
	rctx, stopReaper := context.WithCancel(t.Context())
	reaped := make(chan struct{})
	go func() { m.Run(rctx); close(reaped) }()
	progress := `{"ItemId":"` + mkii + `","PlaySessionId":"` + q.Get("PlaySessionId") + `","PositionTicks":10000000}`
	for range 8 {
		if rec := call(t, h, "POST", "/Sessions/Playing/Progress", `MediaBrowser Token="`+captureToken+`"`, progress); rec.Code != 204 {
			t.Fatalf("progress = %d", rec.Code)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if n := m.Count(); n != 1 {
		t.Errorf("reporting player's transcode closed: %d sessions", n)
	}
	time.Sleep(800 * time.Millisecond)
	if n := m.Count(); n != 0 {
		t.Errorf("quiet player's transcode kept: %d sessions", n)
	}
	stopReaper()
	<-reaped // Run closes the rest on the way out
	m.IdleTimeout = oldIdle

	if rec := get(strings.Replace(segs[9], "/9.ts", "/10.ts", 1)); rec.Code != 404 {
		t.Errorf("past the end = %d", rec.Code)
	}
	if rec := call(t, h, "DELETE", "/Videos/ActiveEncodings?deviceId=x&playSessionId="+q.Get("PlaySessionId"), `MediaBrowser Token="`+captureToken+`"`, ""); rec.Code != 204 {
		t.Errorf("ActiveEncodings = %d", rec.Code)
	}
	if n := d.Transcoding.Sessions.Count(); n != 0 {
		t.Errorf("session still running after DELETE: %d", n)
	}
	// After a stop, the next segment request simply starts again.
	fetch(5)

	if rec := get(strings.Replace(tu, "PlaySessionId=", "X=", 1)); rec.Code != 400 {
		t.Errorf("no PlaySessionId = %d", rec.Code)
	}
	if rec := get(strings.Replace(tu, "ApiKey=", "X=", 1)); rec.Code != 401 {
		t.Errorf("no token = %d", rec.Code)
	}
}

func TestHLSCopyRules(t *testing.T) {
	str := func(s string) *string { return &s }
	ch := func(n int32) *int32 { return &n }
	job := func(video, audio, query string, channels int32) hlsJob {
		return hlsJob{video: &db.MediaStream{Codec: str(video)}, audio: &db.MediaStream{Codec: str(audio), Channels: ch(channels)},
			q: jfapi.QueryOf(httptest.NewRequestWithContext(context.Background(), "GET", "/?"+query, nil))}
	}
	for _, c := range []struct {
		j            hlsJob
		video, audio bool
	}{
		{job("h264", "aac", "VideoCodec=h264&AudioCodec=aac,mp3&AllowVideoStreamCopy=true", 2), true, true},
		{job("h264", "aac", "VideoCodec=h264&AudioCodec=aac", 2), false, true}, // no AllowVideoStreamCopy: encode
		{job("hevc", "eac3", "VideoCodec=h264&AudioCodec=aac&AllowVideoStreamCopy=true", 6), false, false},
		{job("h264", "flac", "VideoCodec=h264&AudioCodec=flac,aac&AllowVideoStreamCopy=true", 2), true, false}, // FLAC can't go in TS
		{job("vp9", "opus", "VideoCodec=vp9,h264&AudioCodec=opus&AllowVideoStreamCopy=true", 2), false, true},  // VP9 can't either
		{job("h264", "ac3", "AudioCodec=ac3&TranscodingMaxAudioChannels=2", 6), false, false},                  // over the channel limit
		{job("h264", "dts", "AudioCodec=copy", 6), false, true},                                                // main.m3u8's "copy"
	} {
		if v, a := c.j.copiesVideo(), c.j.copiesAudio(); v != c.video || a != c.audio {
			t.Errorf("%s/%s %v: copy video %v audio %v", deref(c.j.video.Codec), deref(c.j.audio.Codec), c.j.q, v, a)
		}
	}
	for profile, want := range map[string]string{"High": "avc1.640029", "Main": "avc1.4d0028", "Constrained Baseline": "avc1.42e01e", "?": "avc1.640029"} {
		level := map[string]float32{"High": 41, "Main": 40, "Constrained Baseline": 30, "?": 30}[profile]
		if got := avcCodec(profile, level); got != want {
			t.Errorf("%s@%v: %s, want %s", profile, level, got, want)
		}
	}
}

// A client that can't play Matroska gets the H.264 copied into HLS.
func TestHLSRemuxRealFFmpeg(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	h, d := hlsServer(t)
	src := filepath.Join(t.TempDir(), "episode.mkv")
	if out, err := exec.CommandContext(t.Context(), "ffmpeg", "-v", "error", "-f", "lavfi", "-i", "testsrc=duration=30:size=320x240:rate=24",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=30", "-map", "0", "-map", "1",
		"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p", "-g", "24", "-c:a", "aac", "-ac", "2", src).CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	execSQL(t, `UPDATE media_sources SET path_or_url = $1, runtime_ticks = 300000000, container = 'mkv' WHERE item_id = $2`, src, mkii)
	execSQL(t, `UPDATE media_streams SET codec = 'h264', profile = 'Constrained Baseline', level = 13, width = 320, height = 240
		WHERE type = 'Video' AND media_source_id IN (SELECT id FROM media_sources WHERE item_id = $1)`, mkii)

	rec := call(t, h, "POST", "/Items/"+mkii+"/PlaybackInfo", `MediaBrowser Token="`+captureToken+`"`, `{"DeviceProfile":{
		"DirectPlayProfiles":[{"Type":"Video","Container":"mp4","VideoCodec":"h264","AudioCodec":"aac"}],
		"TranscodingProfiles":[{"Type":"Video","Container":"ts","Protocol":"hls","VideoCodec":"h264","AudioCodec":"aac"}]}}`)
	var pi playbackResp
	_ = json.Unmarshal(rec.Body.Bytes(), &pi)
	if len(pi.MediaSources) != 1 {
		t.Fatalf("PlaybackInfo: %s", rec.Body)
	}
	ms := pi.MediaSources[0]
	tu := ms.TranscodingUrl
	if ms.SupportsDirectPlay || !ms.SupportsDirectStream || !strings.Contains(tu, "AllowVideoStreamCopy=true") ||
		!strings.Contains(tu, "TranscodeReasons=ContainerNotSupported") {
		t.Fatalf("remux decision: %+v", ms)
	}

	get := func(path string) *httptest.ResponseRecorder {
		t.Helper()
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), "GET", path, nil))
		return rec
	}
	master := get(tu)
	lines := strings.Split(strings.TrimSpace(master.Body.String()), "\n")
	if master.Code != 200 || !strings.Contains(lines[1], `CODECS="avc1.42e00d,mp4a.40.2"`) || !strings.Contains(lines[2], "AudioCodec=copy") {
		t.Fatalf("master: %d %s", master.Code, master.Body)
	}
	base := strings.TrimSuffix(strings.SplitN(tu, "?", 2)[0], "master.m3u8")
	main := get(base + lines[2])
	var segs []string
	for _, l := range strings.Split(main.Body.String(), "\n") {
		if strings.HasPrefix(l, "hls1/") {
			segs = append(segs, base+l)
		}
	}
	if len(segs) != 10 {
		t.Fatalf("main: %d segments", len(segs))
	}
	probeSegment := func(i int) (profile, start string) {
		t.Helper()
		rec := get(segs[i])
		if rec.Code != 200 {
			t.Fatalf("segment %d: %d %s", i, rec.Code, rec.Body)
		}
		p := filepath.Join(t.TempDir(), "s.ts")
		_ = os.WriteFile(p, rec.Body.Bytes(), 0o644)
		ffprobe := func(entries string) string {
			out, err := exec.CommandContext(t.Context(), "ffprobe", "-v", "error", "-select_streams", "v:0",
				"-show_entries", entries, "-of", "csv=p=0", p).Output()
			if err != nil {
				t.Fatalf("probe segment %d: %v", i, err)
			}
			first, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n") // TS lists streams per program too
			return first
		}
		return ffprobe("stream=profile"), ffprobe("format=start_time")
	}
	if profile, _ := probeSegment(0); profile != "Constrained Baseline" {
		t.Errorf("segment 0 was re-encoded (profile %q)", profile)
	}
	q, _ := url.ParseQuery(strings.SplitN(tu, "?", 2)[1])
	sess, ok := d.Transcoding.Sessions.Get(q.Get("PlaySessionId"))
	if !ok || !sess.Opts.CopyVideo || !sess.Opts.CopyAudio {
		t.Errorf("session copies: %+v", sess)
	}
	// A seek after a stop restarts at segment 7: copied video starts at the
	// keyframe nearest 21s (1s GOP), plus the MPEG-TS muxer's 1.4s delay.
	d.Transcoding.Sessions.Close(q.Get("PlaySessionId"))
	_, start := probeSegment(7)
	if s, _ := strconv.ParseFloat(start, 64); s < 20 || s > 23.5 {
		t.Errorf("segment 7 starts at %s, want ≈ 21s", start)
	}
}

// A video transcode is refused for a user whose policy forbids it, and on
// a server left with only the software encoder and transcode.software off.
func TestHLSTranscodeRefused(t *testing.T) {
	h, d := hlsServer(t)
	caps := hlsCaptures(t, "*-GET-videos-id-master-m3u8.json")
	if len(caps) == 0 {
		t.Skip("no captures")
	}
	c := caps[0]
	if rec := c.replay(t, h); rec.Code != 200 {
		t.Fatalf("baseline: %d", rec.Code)
	}
	admin := `MediaBrowser Token="` + captureToken + `"`
	setPolicy := func(body string) {
		t.Helper()
		if rec := call(t, h, "POST", "/Users/"+captureUserID+"/Policy", admin, body); rec.Code != 204 {
			t.Fatalf("policy: %d %s", rec.Code, rec.Body)
		}
	}
	setPolicy(`{"EnableVideoPlaybackTranscoding":false}`)
	if rec := c.replay(t, h); rec.Code != 403 {
		t.Errorf("transcode for a user without it: %d", rec.Code)
	}
	setPolicy(`{}`)
	if rec := c.replay(t, h); rec.Code != 200 {
		t.Errorf("allowed again: %d", rec.Code)
	}
	d.Transcoding.NoCPU = true // the test server's encoder is the software one
	if rec := c.replay(t, h); rec.Code != 403 {
		t.Errorf("software transcode with transcode.software off: %d", rec.Code)
	}
}
