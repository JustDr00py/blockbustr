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
	"strings"
	"testing"

	"github.com/sysadmin/blockbustr/internal/jfapi"
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
