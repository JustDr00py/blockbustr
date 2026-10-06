package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sysadmin/blockbustr/internal/jfapi"
	"github.com/sysadmin/blockbustr/internal/subtitles"
	"github.com/sysadmin/blockbustr/internal/testutil"
	"github.com/sysadmin/blockbustr/internal/transcode"
)

// subtitleServer serves MKII from a real 10s black video with an embedded
// SRT (stream 1, "Hello" 1–3s), an embedded ASS (2), a PGS track (3, only
// in the database) and a sidecar SRT (4).
func subtitleServer(t *testing.T) (http.Handler, Deps, string) {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	_, d := newIntegrationServer(t)
	seedRecon(t, d)
	seedCaptureToken(t, d)
	d.Transcoding = &Transcoding{Sessions: transcode.NewManager(t.TempDir(), 2), Encoder: transcode.CapSoftware, SegmentSeconds: 3}
	t.Cleanup(d.Transcoding.Sessions.CloseAll)
	d.Subtitles = &subtitles.Store{Dir: t.TempDir()}
	rt := jfapi.NewRouter(testutil.Discard(), jfapi.Options{LegacyAuth: true})
	Register(rt, d)

	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	srtFile := write("a.srt", "1\n00:00:01,000 --> 00:00:03,000\nHello\n\n2\n00:00:06,000 --> 00:00:08,000\nWorld\n")
	assFile := write("b.ass", "[Script Info]\nScriptType: v4.00+\n\n[V4+ Styles]\nFormat: Name, Fontname, Fontsize, PrimaryColour, SecondaryColour, OutlineColour, BackColour, Bold, Italic, Underline, StrikeOut, ScaleX, ScaleY, Spacing, Angle, BorderStyle, Outline, Shadow, Alignment, MarginL, MarginR, MarginV, Encoding\nStyle: Default,Arial,40,&H00FFFFFF,&H000000FF,&H00000000,&H00000000,0,0,0,0,100,100,0,0,1,2,0,2,10,10,10,1\n\n[Events]\nFormat: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text\nDialogue: 0,0:00:02.00,0:00:04.00,Default,,0,0,0,,Styled line\n")
	sidecar := write("Mortal Kombat II (2026).en.srt", "1\n00:00:00,500 --> 00:00:01,500\nFrom the sidecar\n")
	video := filepath.Join(dir, "movie.mkv")
	if out, err := exec.CommandContext(t.Context(), "ffmpeg", "-v", "error", "-f", "lavfi", "-i", "color=c=black:size=320x240:rate=10:duration=10",
		"-i", srtFile, "-i", assFile, "-map", "0", "-map", "1", "-map", "2", "-c:v", "libx264", "-preset", "ultrafast", "-c:s", "copy", video).CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	execSQL(t, `UPDATE media_sources SET path_or_url = $1, runtime_ticks = 100000000, container = 'mkv', bitrate = 100000, etag = 'subs-test' WHERE item_id = $2`, video, mkii)
	execSQL(t, `DELETE FROM media_streams WHERE media_source_id IN (SELECT id FROM media_sources WHERE item_id = $1)`, mkii)
	for _, st := range []struct {
		idx                    int
		typ, codec, lang, path string
	}{{0, "Video", "h264", "", ""}, {1, "Subtitle", "subrip", "eng", ""}, {2, "Subtitle", "ass", "eng", ""},
		{3, "Subtitle", "PGSSUB", "eng", ""}, {4, "Subtitle", "subrip", "eng", sidecar}} {
		var p *string
		if st.path != "" {
			p = &st.path
		}
		execSQL(t, `INSERT INTO media_streams (media_source_id, idx, type, codec, language, width, height, is_external, external_path)
			SELECT id, $2, $3, $4, nullif($5, ''), CASE WHEN $3 = 'Video' THEN 320 END, CASE WHEN $3 = 'Video' THEN 240 END, $6::text IS NOT NULL, $6
			FROM media_sources WHERE item_id = $1`, mkii, st.idx, st.typ, st.codec, st.lang, p)
	}
	return rt, d, sidecar
}

func playbackInfoWith(t *testing.T, h http.Handler, body string) playbackResp {
	t.Helper()
	rec := call(t, h, "POST", "/Items/"+mkii+"/PlaybackInfo", `MediaBrowser Token="`+captureToken+`"`, body)
	var pi playbackResp
	if err := json.Unmarshal(rec.Body.Bytes(), &pi); err != nil || len(pi.MediaSources) != 1 {
		t.Fatalf("PlaybackInfo: %d %s", rec.Code, rec.Body)
	}
	return pi
}

func fetch(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), "GET", path, nil))
	return rec
}

func TestSubtitleFiles(t *testing.T) {
	h, _, _ := subtitleServer(t)
	// Findroid's own profile: SRT and ASS as files, nothing playable.
	pi := playbackInfoWith(t, h, `{"DeviceProfile":{"SubtitleProfiles":[{"Format":"srt","Method":"External"},{"Format":"ass","Method":"External"}]}}`)
	urls := map[int]string{}
	for _, st := range pi.MediaSources[0].MediaStreams {
		if st.Type == "Subtitle" {
			urls[st.Index] = st.DeliveryUrl
			want := map[int]string{1: "External", 2: "External", 3: "Encode", 4: "External"}[st.Index]
			if st.DeliveryMethod != want {
				t.Errorf("stream %d: %s, want %s", st.Index, st.DeliveryMethod, want)
			}
		}
	}
	if !strings.HasSuffix(strings.SplitN(urls[2], "?", 2)[0], "/Subtitles/2/0/Stream.ass") || urls[3] != "" {
		t.Fatalf("delivery urls: %v", urls)
	}
	body := func(path string, want int) string {
		t.Helper()
		rec := fetch(t, h, path)
		if rec.Code != want {
			t.Fatalf("%s = %d %s", path, rec.Code, rec.Body)
		}
		return rec.Body.String()
	}
	if b := body(urls[1], 200); !strings.Contains(b, "00:00:01,000 --> 00:00:03,000\nHello") {
		t.Errorf("embedded SRT: %q", b)
	}
	if b := body(urls[2], 200); !strings.Contains(b, "[V4+ Styles]") || !strings.Contains(b, "Styled line") {
		t.Errorf("embedded ASS as ASS: %q", b)
	}
	base := "/Videos/" + mkii + "/" + mkii + "/Subtitles/"
	if rec := fetch(t, h, base+"2/Stream.vtt"); rec.Code != 200 || !strings.HasPrefix(rec.Body.String(), "WEBVTT") ||
		!strings.Contains(rec.Body.String(), "Styled line") || rec.Header().Get("Content-Type") != "text/vtt; charset=utf-8" {
		t.Errorf("ASS as WebVTT: %d %v %q", rec.Code, rec.Header(), rec.Body)
	}
	if b := body(base+"1/50000000/Stream.srt", 200); strings.Contains(b, "Hello") || !strings.Contains(b, "1\n00:00:01,000 --> 00:00:03,000\nWorld") {
		t.Errorf("from 5s: %q", b)
	}
	if b := body(urls[4], 200); !strings.Contains(b, "From the sidecar") {
		t.Errorf("sidecar: %q", b)
	}
	body(base+"3/Stream.srt", 404) // PGS can't be a file
	body(base+"9/Stream.srt", 404) // no such stream
	body(base+"1/Stream.mp4", 404) // not a subtitle format
	body("/Videos/"+mkii+"/00000000000000000000000000000042/Subtitles/1/Stream.srt", 404)
}

// lumaPeak is the brightest pixel in a video file (ffprobe signalstats).
func lumaPeak(t *testing.T, path string) int {
	t.Helper()
	out, err := exec.CommandContext(t.Context(), "ffprobe", "-v", "error", "-f", "lavfi", "-i", "movie="+path+",signalstats",
		"-show_entries", "frame_tags=lavfi.signalstats.YMAX", "-of", "csv=p=0").Output()
	if err != nil {
		t.Fatalf("signalstats: %v", err)
	}
	peak := 0
	for _, f := range strings.Fields(string(out)) {
		var v int
		_, _ = fmt.Sscan(f, &v)
		peak = max(peak, v)
	}
	return peak
}

func TestSubtitleBurnInThroughHLS(t *testing.T) {
	h, d, _ := subtitleServer(t)
	// A client that takes no subtitle formats, choosing the embedded SRT:
	// it has to be burned in, so the video is transcoded.
	pi := playbackInfoWith(t, h, `{"SubtitleStreamIndex":1,"DeviceProfile":{
		"DirectPlayProfiles":[{"Type":"Video","Container":"mkv","VideoCodec":"h264"}],
		"TranscodingProfiles":[{"Type":"Video","Container":"ts","Protocol":"hls","VideoCodec":"h264","AudioCodec":"aac"}]}}`)
	tu := pi.MediaSources[0].TranscodingUrl
	if pi.MediaSources[0].SupportsDirectPlay || !strings.Contains(tu, "SubtitleStreamIndex=1") || !strings.Contains(tu, "SubtitleMethod=Encode") ||
		!strings.Contains(tu, "SubtitleCodecNotSupported") || strings.Contains(tu, "AllowVideoStreamCopy") {
		t.Fatalf("burn-in decision: %+v", pi.MediaSources[0])
	}
	base := strings.TrimSuffix(strings.SplitN(tu, "?", 2)[0], "master.m3u8")
	seg := func(n int) string {
		t.Helper()
		rec := fetch(t, h, base+fmt.Sprintf("hls1/main/%d.ts?", n)+strings.SplitN(tu, "?", 2)[1])
		if rec.Code != 200 {
			t.Fatalf("segment %d: %d %s", n, rec.Code, rec.Body)
		}
		p := filepath.Join(t.TempDir(), fmt.Sprintf("%d.ts", n))
		_ = os.WriteFile(p, rec.Body.Bytes(), 0o644)
		return p
	}
	if peak := lumaPeak(t, seg(0)); peak < 100 { // "Hello", 1–3s
		t.Errorf("segment 0 has no burned-in text (peak luma %d)", peak)
	}
	if peak := lumaPeak(t, seg(1)); peak > 100 { // 3–6s: nothing on screen
		t.Errorf("segment 1 should be black (peak luma %d)", peak)
	}
	q, _ := url.ParseQuery(strings.SplitN(tu, "?", 2)[1])
	if s, ok := d.Transcoding.Sessions.Get(q.Get("PlaySessionId")); !ok || s.Opts.BurnText == "" || s.Opts.CopyVideo {
		t.Errorf("session: %+v", s)
	}
}
