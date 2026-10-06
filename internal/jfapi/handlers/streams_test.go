package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sysadmin/blockbustr/internal/cache"
	"github.com/sysadmin/blockbustr/internal/jfapi/dto"
	"github.com/sysadmin/blockbustr/internal/media"
	"github.com/sysadmin/blockbustr/internal/resolve"
	"github.com/sysadmin/blockbustr/internal/store/pg/db"
	"github.com/sysadmin/blockbustr/internal/stremio"
	"github.com/sysadmin/blockbustr/internal/testutil"
)

type fakeStreams struct {
	offers map[string][]stremio.Offer // by "type/id"
	calls  atomic.Int32
}

func (f *fakeStreams) Collect(_ context.Context, typ, id string) ([]stremio.Offer, error) {
	f.calls.Add(1)
	return f.offers[typ+"/"+id], nil
}

func fileIdx(i int) *int { return &i }

type playbackSources struct {
	MediaSources []struct {
		Id, Name, Path, Protocol, Container string
		IsRemote                            bool
		Size                                int64
	}
	PlaySessionId string
}

// h264Profile is a client that plays H.264 in MKV/MP4 but not HEVC.
const h264Profile = `{"DeviceProfile":{"MaxStreamingBitrate":120000000,
	"DirectPlayProfiles":[{"Type":"Video","Container":"mkv,mp4","VideoCodec":"h264","AudioCodec":"aac,ac3"}],
	"TranscodingProfiles":[{"Type":"Video","Container":"ts","VideoCodec":"h264","AudioCodec":"aac","Protocol":"hls"}]}}`

func TestCatalogStreamsAsMediaSources(t *testing.T) {
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "video/x-matroska")
		_, _ = w.Write([]byte("matroska bytes"))
	}))
	t.Cleanup(cdn.Close)
	fs := &fakeStreams{offers: map[string][]stremio.Offer{"movie/tt0133093": {
		{Addon: "Torrentio", Stream: stremio.Stream{Name: "Torrentio\n4k HDR", Title: "The.Matrix.1999.2160p.UHD.BluRay.x265.HDR-GRP\n👤 99 💾 35.09 GB", InfoHash: "aaaa", FileIdx: fileIdx(2),
			Hints: stremio.StreamHints{Filename: "The.Matrix.1999.2160p.UHD.BluRay.x265.HDR-GRP.mkv"}}},
		{Addon: "Torrentio", Stream: stremio.Stream{Name: "Torrentio\n720p", Title: "The.Matrix.1999.720p.BluRay.x264-GRP\n💾 1.1 GB", InfoHash: "bbbb"}},
		{Addon: "Torrentio", Stream: stremio.Stream{Name: "Torrentio\n1080p", Title: "The.Matrix.1999.1080p.BluRay.x264-BAD", InfoHash: "cccc"}},
		{Addon: "Torrentio", Stream: stremio.Stream{Name: "Torrentio\n1080p", Title: "The.Matrix.1999.HDCAM.x264-CAMGRP", InfoHash: "dddd"}},
		{Addon: "Direct", Stream: stremio.Stream{Name: "Direct 1080p", Title: "The Matrix 1080p x264 💾 1.5 GB", URL: cdn.URL + "/matrix.mkv"}},
	}}}
	var rc *cache.Cache
	sf := newSyncFixture(t, func(d *Deps) {
		d.Streams = fs
		d.Resolver = &resolve.Resolver{Cache: d.Cache, Log: testutil.Discard()}
		d.Config.Stremio.Streams.DenyGroups = []string{"bad"}
		rc = d.Cache
	})
	sf.syncAll(t)
	moviesID := sf.views(t)["Cinemeta Popular"].Id
	matrix := sf.children(t, moviesID)[0]
	if matrix.Name != "The Matrix" {
		t.Fatalf("first movie: %+v", matrix)
	}
	auth := `MediaBrowser Token="` + captureToken + `"`
	playback := func(body string) playbackSources {
		t.Helper()
		rec := call(t, sf.h, "POST", "/Items/"+matrix.Id+"/PlaybackInfo", auth, body)
		if rec.Code != 200 {
			t.Fatalf("PlaybackInfo: %d %s", rec.Code, rec.Body)
		}
		var out playbackSources
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}

	// The top three, ranked for an H.264 client: the direct URL (ready),
	// then 4K HEVC (penalised, still the better picture), then 720p. The
	// denied group and the CAM rip are left out.
	got := playback(h264Profile)
	var names []string
	for _, s := range got.MediaSources {
		names = append(names, s.Name)
	}
	want := []string{
		"1080p H.264 • 1.5 GB • Direct",
		"2160p HDR HEVC • 35.1 GB • Torrentio",
		"720p H.264 • 1.1 GB • Torrentio",
	}
	if strings.Join(names, " | ") != strings.Join(want, " | ") {
		t.Fatalf("sources:\n %q\nwant\n %q", names, want)
	}
	srcs := got.MediaSources
	if srcs[0].Id != matrix.Id || srcs[1].Id == matrix.Id || srcs[1].Id == srcs[2].Id {
		t.Errorf("ids: item %s, sources %s %s %s", matrix.Id, srcs[0].Id, srcs[1].Id, srcs[2].Id)
	}
	for _, s := range srcs {
		if !s.IsRemote || s.Protocol != "Http" || !strings.Contains(s.Path, "/Videos/"+matrix.Id+"/stream?static=true&MediaSourceId="+s.Id) {
			t.Errorf("source %s: remote %v, %s, path %s", s.Name, s.IsRemote, s.Protocol, s.Path)
		}
		if strings.Contains(s.Path, "magnet:") || strings.Contains(s.Path, cdn.URL) {
			t.Errorf("source %s leaks its target: %s", s.Name, s.Path)
		}
	}
	if srcs[0].Container != "mkv" || srcs[1].Container != "mkv" || srcs[1].Size == 0 {
		t.Errorf("container/size: %q %q %d", srcs[0].Container, srcs[1].Container, srcs[1].Size)
	}

	// Asking again gives the same ids (they're derived from the streams).
	again := playback(h264Profile)
	if again.MediaSources[1].Id != srcs[1].Id {
		t.Errorf("ids changed: %s, then %s", srcs[1].Id, again.MediaSources[1].Id)
	}
	// The item id (the details' placeholder) gets every choice; a choice's
	// own id gets just that one.
	if n := len(playback(`{"MediaSourceId":"` + matrix.Id + `"}`).MediaSources); n != 3 {
		t.Errorf("by item id: %d sources", n)
	}
	one := playback(`{"MediaSourceId":"` + srcs[2].Id + `"}`)
	if len(one.MediaSources) != 1 || one.MediaSources[0].Id != srcs[2].Id {
		t.Errorf("by choice id: %+v", one.MediaSources)
	}

	// Streams: the direct URL plays (by the item id); the torrent is found
	// but can't be resolved before P3.8; unknown ids 404.
	stream := func(ms string) int {
		return call(t, sf.h, "GET", "/Videos/"+matrix.Id+"/stream?static=true&MediaSourceId="+ms, "", "").Code
	}
	if code := stream(matrix.Id); code != 200 && code != 302 {
		t.Errorf("stream by item id: %d", code)
	}
	if code := stream(srcs[1].Id); code != http.StatusBadGateway {
		t.Errorf("torrent stream: %d, want 502 until P3.8", code)
	}
	if code := stream(strings.Repeat("0", 31) + "1"); code != 404 {
		t.Errorf("unknown source: %d", code)
	}

	// Details list the remembered choices as versions, without asking the
	// addons again; lists don't either.
	calls := fs.calls.Load()
	var detail struct{ MediaSources []struct{ Id, Name string } }
	getJSON(t, sf.h, "/Items/"+matrix.Id, &detail)
	if len(detail.MediaSources) != 3 || detail.MediaSources[0].Name != want[0] || detail.MediaSources[0].Id != matrix.Id {
		t.Errorf("details: %+v", detail.MediaSources)
	}
	var list struct{ Items []struct{ Name string } }
	getJSON(t, sf.h, "/Items?ParentId="+moviesID+"&Fields=MediaSources", &list)
	if fs.calls.Load() != calls {
		t.Error("details or lists asked the addons")
	}

	// Expired choices: a stream request collects again.
	id, err := dto.ParseID(matrix.Id)
	if err != nil {
		t.Fatal(err)
	}
	if err := rc.Delete(t.Context(), cache.StreamSetKey(id.UUID().String())); err != nil {
		t.Fatal(err)
	}
	if code := stream(matrix.Id); code != 200 && code != 302 {
		t.Errorf("stream after expiry: %d", code)
	}
	if n := fs.calls.Load() - calls; n != 1 {
		t.Errorf("collections after expiry: %d, want 1", n)
	}
}

func TestCatalogWithoutStreamsKeepsPlaceholder(t *testing.T) {
	fs := &fakeStreams{}
	sf := newSyncFixture(t, func(d *Deps) { d.Streams = fs })
	sf.syncAll(t)
	matrix := sf.children(t, sf.views(t)["Cinemeta Popular"].Id)[0]
	rec := call(t, sf.h, "POST", "/Items/"+matrix.Id+"/PlaybackInfo", `MediaBrowser Token="`+captureToken+`"`, h264Profile)
	var out playbackSources
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if rec.Code != 200 || len(out.MediaSources) != 1 || out.MediaSources[0].Id != matrix.Id || out.MediaSources[0].Name != "The Matrix" {
		t.Errorf("PlaybackInfo: %d %+v", rec.Code, out.MediaSources)
	}
	if fs.calls.Load() != 1 {
		t.Errorf("collector called %d times", fs.calls.Load())
	}
}

func TestStreamPrefs(t *testing.T) {
	cases := []struct {
		name string
		p    media.DeviceProfile
		max  int64
		want stremio.Prefs
	}{
		{"no profile plays anything", media.DeviceProfile{}, 0, stremio.Prefs{HEVC: true, AV1: true, HDR: true}},
		{"h264 only", media.DeviceProfile{MaxStreamingBitrate: 8_000_000, DirectPlayProfiles: []media.DirectPlayProfile{{Type: "Video", VideoCodec: "h264"}, {Type: "Audio"}}},
			0, stremio.Prefs{MaxBitrate: 8_000_000, HDR: true}},
		{"any codec", media.DeviceProfile{DirectPlayProfiles: []media.DirectPlayProfile{{Type: "Video", Container: "mkv"}}},
			20_000_000, stremio.Prefs{MaxBitrate: 20_000_000, HEVC: true, AV1: true, HDR: true}},
		{"SDR, 1080p wide", media.DeviceProfile{
			DirectPlayProfiles: []media.DirectPlayProfile{{Type: "Video", VideoCodec: "h264,hevc"}},
			CodecProfiles: []media.CodecProfile{{Type: "Video", Codec: "hevc", Conditions: []media.ProfileCondition{
				{Condition: "EqualsAny", Property: "VideoRangeType", Value: "SDR"},
				{Condition: "LessThanEqual", Property: "Width", Value: "1920"},
			}}},
		}, 0, stremio.Prefs{HEVC: true, MaxHeight: 1080}},
		{"HDR10 and DV, height cap", media.DeviceProfile{
			DirectPlayProfiles: []media.DirectPlayProfile{{Type: "Video", VideoCodec: "hevc,av1"}},
			CodecProfiles: []media.CodecProfile{
				{Type: "Video", Conditions: []media.ProfileCondition{{Condition: "EqualsAny", Property: "VideoRangeType", Value: "SDR|HDR10|DOVI"}}},
				{Type: "Video", Conditions: []media.ProfileCondition{{Condition: "LessThanEqual", Property: "Height", Value: "2160"}}},
			},
		}, 0, stremio.Prefs{HEVC: true, AV1: true, HDR: true, MaxHeight: 2160}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := streamPrefs(c.p, c.max)
			if got.HEVC != c.want.HEVC || got.AV1 != c.want.AV1 || got.HDR != c.want.HDR || got.MaxHeight != c.want.MaxHeight || got.MaxBitrate != c.want.MaxBitrate {
				t.Errorf("streamPrefs = %+v, want %+v", got, c.want)
			}
		})
	}
}

func TestStreamLabel(t *testing.T) {
	cases := []struct {
		r    stremio.Ranked
		want string
	}{
		{stremio.Ranked{Offer: stremio.Offer{Addon: "Torrentio", Stream: stremio.Stream{InfoHash: "a"}},
			Info: stremio.Info{Height: 2160, DV: true, HDR: true, Codec: "hevc", Remux: true, Size: 29 << 30, Cached: true}},
			"2160p DV HEVC Remux • 29.0 GB • Torrentio • cached"},
		{stremio.Ranked{Offer: stremio.Offer{Addon: "Torrentio", Stream: stremio.Stream{InfoHash: "a"}}, Info: stremio.Info{Height: 1080, Uncached: true}},
			"1080p • Torrentio • not cached"},
		{stremio.Ranked{Offer: stremio.Offer{Addon: "Direct", Stream: stremio.Stream{URL: "https://x"}}, Info: stremio.Info{Cached: true}},
			"Stream • Direct"},
		{stremio.Ranked{Info: stremio.Info{Height: 720, Cam: true}}, "720p CAM"},
	}
	for _, c := range cases {
		if got := streamLabel(c.r); got != c.want {
			t.Errorf("streamLabel = %q, want %q", got, c.want)
		}
	}
}

func TestStremioRef(t *testing.T) {
	item := func(kind, path string) db.Item { return db.Item{SourceKind: kind, Path: &path} }
	for path, want := range map[string]string{
		"stremio:movie:tt0133093":      "movie/tt0133093",
		"stremio:series:tt0944947:1:2": "series/tt0944947:1:2",
		"stremio:movie:":               "",
		"/media/Movies/Matrix.mkv":     "",
	} {
		typ, id, ok := stremioRef(item("stremio", path))
		got := ""
		if ok {
			got = typ + "/" + id
		}
		if got != want {
			t.Errorf("stremioRef(%q) = %q, want %q", path, got, want)
		}
	}
	if _, _, ok := stremioRef(item("file", "stremio:movie:tt1")); ok {
		t.Error("a file item counted as a catalog title")
	}
}
