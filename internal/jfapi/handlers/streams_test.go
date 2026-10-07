package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sysadmin/blockbustr/internal/cache"
	"github.com/sysadmin/blockbustr/internal/jfapi/dto"
	"github.com/sysadmin/blockbustr/internal/media"
	"github.com/sysadmin/blockbustr/internal/provider"
	"github.com/sysadmin/blockbustr/internal/resolve"
	"github.com/sysadmin/blockbustr/internal/store/pg/db"
	"github.com/sysadmin/blockbustr/internal/stremio"
	"github.com/sysadmin/blockbustr/internal/subtitles"
	"github.com/sysadmin/blockbustr/internal/testutil"
	"github.com/sysadmin/blockbustr/internal/urlsign"
)

// fakeAccount is a debrid account holding one torrent, downloading until
// ready is set; its file links point at cdn.
type fakeAccount struct {
	provider.Provider
	cdn   string
	added atomic.Value
	ready atomic.Bool
}

func (f *fakeAccount) Name() provider.Name { return provider.RealDebrid }

func (f *fakeAccount) AddMagnet(_ context.Context, magnet string) (string, bool, error) {
	f.added.Store(strings.TrimPrefix(magnet, "magnet:?xt=urn:btih:"))
	return "T1", false, nil
}

func (f *fakeAccount) InstantCheck(context.Context, []string) ([]provider.InstantResult, error) {
	return nil, nil
}

func (f *fakeAccount) Torrent(context.Context, string) (provider.Torrent, error) {
	if !f.ready.Load() {
		return provider.Torrent{ID: "T1", Status: provider.StatusDownloading}, nil
	}
	return provider.Torrent{ID: "T1", Status: provider.StatusReady, Files: []provider.File{
		{ID: "1", Path: "info.txt"}, {ID: "2", Path: "sample.mkv", SizeBytes: 1}, {ID: "3", Path: "The.Matrix.1999.2160p.UHD.BluRay.x265.HDR-GRP.mkv", SizeBytes: 9},
	}}, nil
}

func (f *fakeAccount) FileLink(_ context.Context, _, fileID string) (string, time.Time, error) {
	if fileID != "3" {
		return "", time.Time{}, fmt.Errorf("wrong file %s", fileID)
	}
	return f.cdn + "/matrix.mkv", time.Now().Add(time.Hour), nil
}

type fakeStreams struct {
	offers map[string][]stremio.Offer         // by "type/id"
	subs   map[string][]stremio.SubtitleOffer // by "type/id"
	calls  atomic.Int32
	mu     sync.Mutex
	asked  []string // "streams type/id", "subtitles type/id"
}

func (f *fakeStreams) Subtitles(_ context.Context, typ, id string) ([]stremio.SubtitleOffer, error) {
	f.mu.Lock()
	f.asked = append(f.asked, "subtitles "+typ+"/"+id)
	f.mu.Unlock()
	return f.subs[typ+"/"+id], nil
}

func (f *fakeStreams) Collect(_ context.Context, typ, id string) ([]stremio.Offer, error) {
	f.calls.Add(1)
	f.mu.Lock()
	f.asked = append(f.asked, "streams "+typ+"/"+id)
	f.mu.Unlock()
	return f.offers[typ+"/"+id], nil
}

func fileIdx(i int) *int { return &i }

type playbackSources struct {
	MediaSources []struct {
		Id, Name, Path, Protocol, Container string
		IsRemote                            bool
		Size                                int64
		SupportsDirectPlay                  bool
		MediaStreams                        []struct {
			Index                                       int
			Type, Language, DeliveryMethod, DeliveryUrl string
			IsExternal                                  bool
		}
	}
	PlaySessionId string
}

// h264Profile is a client that plays H.264 in MKV/MP4 but not HEVC.
const h264Profile = `{"DeviceProfile":{"MaxStreamingBitrate":120000000,
	"DirectPlayProfiles":[{"Type":"Video","Container":"mkv,mp4","VideoCodec":"h264","AudioCodec":"aac,ac3"}],
	"TranscodingProfiles":[{"Type":"Video","Container":"ts","VideoCodec":"h264","AudioCodec":"aac","Protocol":"hls"}]}}`

func TestCatalogStreamsAsMediaSources(t *testing.T) {
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/subs/") {
			_, _ = w.Write([]byte("1\n00:00:01,000 --> 00:00:02,000\nWake up, " + strings.TrimPrefix(r.URL.Path, "/subs/") + "\n"))
			return
		}
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
	}}, subs: map[string][]stremio.SubtitleOffer{"movie/tt0133093": {
		{Addon: "OpenSubtitles v3", Subtitle: stremio.Subtitle{Lang: "ger", URL: cdn.URL + "/subs/de1"}},
		{Addon: "OpenSubtitles v3", Subtitle: stremio.Subtitle{Lang: "eng", URL: cdn.URL + "/subs/en1"}},
		{Addon: "OpenSubtitles v3", Subtitle: stremio.Subtitle{Lang: "pob", URL: cdn.URL + "/subs/pt1"}},
		{Addon: "OpenSubtitles v3", Subtitle: stremio.Subtitle{Lang: "eng", URL: cdn.URL + "/subs/en2"}},
	}}}
	var rc *cache.Cache
	rd := &fakeAccount{cdn: cdn.URL}
	sf := newSyncFixture(t, func(d *Deps) {
		d.Streams = fs
		d.Resolver = &resolve.Resolver{Cache: d.Cache, Log: testutil.Discard(),
			Providers: map[provider.Name]provider.Provider{provider.RealDebrid: rd}}
		d.Config.Stremio.Streams.DenyGroups = []string{"bad"}
		d.Config.Stremio.Subtitles.Languages = []string{"en", "pt"}
		d.Subtitles = &subtitles.Store{Dir: t.TempDir()}
		d.StreamSigner = &urlsign.Signer{Key: []byte("test stream key, thirty-two byte"), TTL: time.Hour}
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

	// Subtitles in the configured languages (en first, then pt from
	// OpenSubtitles' "pob"), as external tracks of every choice, delivered
	// as files.
	for _, src := range srcs {
		var subs []string
		for _, st := range src.MediaStreams {
			if st.Type != "Subtitle" || !st.IsExternal || st.DeliveryMethod != "External" || !strings.Contains(st.DeliveryUrl, "/"+src.Id+"/Subtitles/") {
				t.Errorf("%s: subtitle %+v", src.Name, st)
			}
			subs = append(subs, fmt.Sprintf("%d:%s", st.Index, st.Language))
		}
		if strings.Join(subs, " ") != "100:eng 101:eng 102:por" {
			t.Errorf("%s: subtitles %v", src.Name, subs)
		}
		if !src.SupportsDirectPlay {
			t.Errorf("%s: an undecided source lost direct play", src.Name)
		}
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
	// Addon links are always proxied (they may carry the addon's
	// credentials), even a plain CDN URL whose scheme matches.
	if rec := call(t, sf.h, "GET", "/Videos/"+matrix.Id+"/stream?static=true&MediaSourceId="+matrix.Id, "", ""); rec.Code != 200 || rec.Header().Get("Location") != "" {
		t.Errorf("stream by item id: %d, Location %q; want proxied", rec.Code, rec.Header().Get("Location"))
	}
	// Paths are signed: as given they play; altered, moved to another
	// source or expired they're refused; unsigned ones play (Findroid).
	for _, src := range srcs {
		if !strings.Contains(src.Path, "&Expires=") || !strings.Contains(src.Path, "&Signature=") {
			t.Errorf("%s: unsigned path %s", src.Name, src.Path)
		}
	}
	_, rest, found := strings.Cut(srcs[0].Path, "/Videos/")
	if !found {
		t.Fatalf("path %s", srcs[0].Path)
	}
	signed := "/Videos/" + rest
	if code := call(t, sf.h, "GET", signed, "", "").Code; code != 200 {
		t.Errorf("signed path: %d", code)
	}
	if code := call(t, sf.h, "GET", signed+"x", "", "").Code; code != 403 {
		t.Errorf("tampered signature: %d", code)
	}
	if code := call(t, sf.h, "GET", strings.Replace(signed, "MediaSourceId="+srcs[0].Id, "MediaSourceId="+srcs[2].Id, 1), "", "").Code; code != 403 {
		t.Errorf("signature moved to another source: %d", code)
	}
	expired := regexp.MustCompile(`Expires=\d+`).ReplaceAllString(signed, "Expires=1000")
	if code := call(t, sf.h, "GET", expired, "", "").Code; code != 403 {
		t.Errorf("expired/altered expiry: %d", code)
	}
	// The torrent goes to the debrid account; while it downloads the
	// stream is 503 with Retry-After, then it plays.
	rec := call(t, sf.h, "GET", "/Videos/"+matrix.Id+"/stream?static=true&MediaSourceId="+srcs[1].Id, "", "")
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") == "" {
		t.Errorf("downloading torrent: %d, Retry-After %q", rec.Code, rec.Header().Get("Retry-After"))
	}
	if rd.added.Load() != "aaaa" {
		t.Errorf("added %v, want the stream's hash", rd.added.Load())
	}
	// Its subtitles don't wait for the torrent.
	subURL := srcs[1].MediaStreams[1].DeliveryUrl
	if rec := call(t, sf.h, "GET", subURL, "", ""); rec.Code != 200 || !strings.Contains(rec.Body.String(), "Wake up, en2") {
		t.Errorf("subtitle while downloading: %d %q", rec.Code, rec.Body.String())
	}
	if rec := call(t, sf.h, "GET", strings.Replace(subURL, "Stream.srt", "Stream.vtt", 1), "", ""); rec.Code != 200 || !strings.HasPrefix(rec.Body.String(), "WEBVTT") {
		t.Errorf("subtitle as WebVTT: %d %q", rec.Code, rec.Body.String())
	}
	rd.ready.Store(true)
	// A debrid link never reaches the client: proxied.
	if rec := call(t, sf.h, "GET", "/Videos/"+matrix.Id+"/stream?static=true&MediaSourceId="+srcs[1].Id, "", ""); rec.Code != 200 || rec.Body.String() != "matroska bytes" || rec.Header().Get("Location") != "" {
		t.Errorf("ready torrent: %d %q, Location %q", rec.Code, rec.Body.String(), rec.Header().Get("Location"))
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
			Info: stremio.Info{Height: 2160, DV: true, HDR: true, Codec: "hevc", Remux: true, Size: 29 << 30, Cached: true, Debrid: true}},
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

func TestPickSubtitles(t *testing.T) {
	offer := func(lang, url string) stremio.SubtitleOffer {
		return stremio.SubtitleOffer{Addon: "A", Subtitle: stremio.Subtitle{Lang: lang, URL: url}}
	}
	offers := []stremio.SubtitleOffer{
		offer("ger", "de1"), offer("eng", "en1"), offer("en", "en2"), offer("eng", "en1"), // a duplicate URL
		offer("eng", "en3"), offer("eng", "en4"), offer("pob", "pt1"), offer("xx-bogus", "x"), offer("", "y"),
	}
	cases := []struct {
		want []string
		per  int
		got  string
	}{
		{nil, 3, "eng:en1 eng:en2 eng:en3"},
		{[]string{"de", "en"}, 1, "deu:de1 eng:en1"},
		{[]string{"pt"}, 3, "por:pt1"},
		{[]string{"fr"}, 3, ""},
	}
	for _, c := range cases {
		var got []string
		for _, s := range pickSubtitles(offers, c.want, c.per) {
			got = append(got, s.Language+":"+s.URL)
		}
		if strings.Join(got, " ") != c.got {
			t.Errorf("pickSubtitles(%v, %d) = %q, want %q", c.want, c.per, strings.Join(got, " "), c.got)
		}
	}
	var many []stremio.SubtitleOffer
	for i := range 50 {
		many = append(many, offer("eng", fmt.Sprint(i)))
	}
	if n := len(pickSubtitles(many, nil, 10)); n != 10 {
		t.Errorf("per-language cap: %d", n)
	}
}

// A version that fails falls back to the next when the player didn't pick
// one, and isn't offered again; one the player picked fails as itself.
func TestCatalogStreamFallback(t *testing.T) {
	var blockedHits atomic.Int32
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/blocked.mkv" {
			blockedHits.Add(1)
			w.WriteHeader(http.StatusUnavailableForLegalReasons)
			return
		}
		_, _ = w.Write([]byte("matroska bytes"))
	}))
	t.Cleanup(cdn.Close)
	fs := &fakeStreams{offers: map[string][]stremio.Offer{"movie/tt0133093": {
		{Addon: "Torrentio RD", Stream: stremio.Stream{Name: "[RD+] Torrentio\n4k", Title: "Blocked.2160p.mkv", URL: cdn.URL + "/blocked.mkv"}},
		{Addon: "Torrentio RD", Stream: stremio.Stream{Name: "[RD+] Torrentio\n1080p", Title: "Works.1080p.mkv", URL: cdn.URL + "/works.mkv"}},
		{Addon: "Torrentio RD", Stream: stremio.Stream{Name: "[RD download] Torrentio\n4k", Title: "Uncached.2160p.mkv", URL: cdn.URL + "/uncached.mkv"}},
	}}}
	sf := newSyncFixture(t, func(d *Deps) {
		d.Streams = fs
		d.Resolver = &resolve.Resolver{Cache: d.Cache, Log: testutil.Discard()}
	})
	sf.syncAll(t)
	matrix := sf.children(t, sf.views(t)["Cinemeta Popular"].Id)[0]
	itemID, err := dto.ParseID(matrix.Id)
	if err != nil {
		t.Fatal(err)
	}
	blocked := dto.IDFromUUID(uuid.NewSHA1(itemID.UUID(), []byte("url:"+cdn.URL+"/blocked.mkv"))).String()
	auth := `MediaBrowser Token="` + captureToken + `"`
	offered := func() []string {
		rec := call(t, sf.h, "POST", "/Items/"+matrix.Id+"/PlaybackInfo", auth, `{}`)
		var out playbackSources
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		var names []string
		for _, s := range out.MediaSources {
			names = append(names, s.Name)
		}
		return names
	}
	stream := func(ms string) *httptest.ResponseRecorder {
		return call(t, sf.h, "GET", "/Videos/"+matrix.Id+"/stream?static=true&MediaSourceId="+ms, "", "")
	}

	// [RD+] links rank as cached; the [RD download] one is offered last.
	if names := offered(); len(names) != 3 || !strings.HasPrefix(names[0], "2160p") || !strings.HasSuffix(names[2], "not cached") {
		t.Fatalf("offered %q", names)
	}
	// Picked in a version picker, the blocked one fails as itself.
	if code := stream(blocked).Code; code != http.StatusBadGateway {
		t.Errorf("picked blocked version: %d", code)
	}
	// Remembered as bad: the default (item id) goes straight to the 1080p,
	// and it's no longer offered.
	hits := blockedHits.Load()
	if rec := stream(matrix.Id); rec.Code != 200 || rec.Body.String() != "matroska bytes" {
		t.Errorf("default stream: %d %q", rec.Code, rec.Body.String())
	}
	if blockedHits.Load() != hits {
		t.Error("a version known bad was tried again")
	}
	if names := offered(); len(names) != 2 || strings.HasPrefix(names[0], "2160p") {
		t.Errorf("offered after the failure: %q", names)
	}
}

// Without anything remembered, the default stream tries the versions in
// order and plays the first that works.
func TestCatalogStreamFallsBack(t *testing.T) {
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/blocked.mkv" {
			w.WriteHeader(http.StatusUnavailableForLegalReasons)
			return
		}
		_, _ = w.Write([]byte("matroska bytes"))
	}))
	t.Cleanup(cdn.Close)
	fs := &fakeStreams{offers: map[string][]stremio.Offer{"movie/tt0133093": {
		{Addon: "A", Stream: stremio.Stream{Name: "[RD+] 4k", URL: cdn.URL + "/blocked.mkv"}},
		{Addon: "A", Stream: stremio.Stream{Name: "[RD+] 1080p", URL: cdn.URL + "/works.mkv"}},
	}}}
	sf := newSyncFixture(t, func(d *Deps) {
		d.Streams = fs
		d.Resolver = &resolve.Resolver{Cache: d.Cache, Log: testutil.Discard()}
	})
	sf.syncAll(t)
	matrix := sf.children(t, sf.views(t)["Cinemeta Popular"].Id)[0]
	if rec := call(t, sf.h, "POST", "/Items/"+matrix.Id+"/PlaybackInfo", `MediaBrowser Token="`+captureToken+`"`, `{}`); rec.Code != 200 {
		t.Fatal(rec.Code)
	}
	if rec := call(t, sf.h, "GET", "/Videos/"+matrix.Id+"/stream?static=true&MediaSourceId="+matrix.Id, "", ""); rec.Code != 200 || rec.Body.String() != "matroska bytes" {
		t.Errorf("default stream: %d %q", rec.Code, rec.Body.String())
	}
}

// Catalogs keyed by TMDB (AIOStreams' "tmdb:603"): addons are asked by the
// IMDb id first (most, and the services behind aggregators, only know
// that), then by the catalog's own id; episodes as "tt…:season:episode".
func TestAddonIDsPreferIMDb(t *testing.T) {
	fs := &fakeStreams{offers: map[string][]stremio.Offer{
		"movie/tt0133093":      {{Addon: "A", Stream: stremio.Stream{Name: "1080p", URL: "https://cdn.example/m.mkv"}}},
		"series/tt0944947:1:1": {{Addon: "A", Stream: stremio.Stream{Name: "1080p", URL: "https://cdn.example/e.mkv"}}},
		"movie/tmdb:604":       {{Addon: "A", Stream: stremio.Stream{Name: "720p", URL: "https://cdn.example/r.mkv"}}},
	}}
	sf := newSyncFixture(t, func(d *Deps) { d.Streams = fs })
	sf.syncAll(t)
	// Re-key titles the way a TMDB-keyed catalog stores them.
	for _, q := range []string{
		`UPDATE items SET path = 'stremio:movie:tmdb:603' WHERE name = 'The Matrix'`,
		`UPDATE items SET path = 'stremio:movie:tmdb:604', provider_ids = '{}' WHERE name = 'The Matrix Reloaded'`,
		`UPDATE items SET path = 'stremio:series:tmdb:1399:1:1' WHERE type = 'Episode' AND name = 'Winter Is Coming'`,
	} {
		if _, err := testPool.Exec(t.Context(), q); err != nil {
			t.Fatal(err)
		}
	}
	movies := map[string]string{}
	for _, it := range sf.children(t, sf.views(t)["Cinemeta Popular"].Id) {
		movies[it.Name] = it.Id
	}
	var episode string
	if err := testPool.QueryRow(t.Context(), `SELECT replace(id::text, '-', '') FROM items WHERE type = 'Episode' AND name = 'Winter Is Coming'`).Scan(&episode); err != nil {
		t.Fatal(err)
	}
	auth := `MediaBrowser Token="` + captureToken + `"`
	play := func(id string) []string {
		t.Helper()
		fs.mu.Lock()
		fs.asked = nil
		fs.mu.Unlock()
		rec := call(t, sf.h, "POST", "/Items/"+id+"/PlaybackInfo", auth, `{}`)
		var out playbackSources
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		var names []string
		for _, s := range out.MediaSources {
			names = append(names, s.Name)
		}
		fs.mu.Lock()
		defer fs.mu.Unlock()
		slices.Sort(fs.asked)
		return append(names, fs.asked...)
	}
	cases := map[string]string{
		// IMDb known: asked by it, which answers; subtitles by it too.
		movies["The Matrix"]: "1080p • A | streams movie/tt0133093 | subtitles movie/tt0133093",
		// No IMDb id: the catalog's own.
		movies["The Matrix Reloaded"]: "720p • A | streams movie/tmdb:604 | subtitles movie/tmdb:604",
		// Episode of a series with an IMDb id.
		episode: "1080p • A | streams series/tt0944947:1:1 | subtitles series/tt0944947:1:1",
	}
	for id, want := range cases {
		if got := strings.Join(play(id), " | "); got != want {
			t.Errorf("got  %s\nwant %s", got, want)
		}
	}
}

// When the IMDb id finds nothing, the catalog's own id is tried.
func TestAddonIDsFallBackToCatalogID(t *testing.T) {
	fs := &fakeStreams{offers: map[string][]stremio.Offer{
		"movie/tmdb:603": {{Addon: "A", Stream: stremio.Stream{Name: "2160p", URL: "https://cdn.example/m.mkv"}}},
	}}
	sf := newSyncFixture(t, func(d *Deps) { d.Streams = fs })
	sf.syncAll(t)
	if _, err := testPool.Exec(t.Context(), `UPDATE items SET path = 'stremio:movie:tmdb:603' WHERE name = 'The Matrix'`); err != nil {
		t.Fatal(err)
	}
	var id string
	for _, it := range sf.children(t, sf.views(t)["Cinemeta Popular"].Id) {
		if it.Name == "The Matrix" {
			id = it.Id
		}
	}
	rec := call(t, sf.h, "POST", "/Items/"+id+"/PlaybackInfo", `MediaBrowser Token="`+captureToken+`"`, `{}`)
	var out playbackSources
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if len(out.MediaSources) != 1 || out.MediaSources[0].Name != "2160p • A" {
		t.Errorf("sources: %+v", out.MediaSources)
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if !slices.Contains(fs.asked, "streams movie/tt0133093") || !slices.Contains(fs.asked, "streams movie/tmdb:603") {
		t.Errorf("asked %v", fs.asked)
	}
}
