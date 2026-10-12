package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sysadmin/blockbustr/internal/config"
	"github.com/sysadmin/blockbustr/internal/media"
	"github.com/sysadmin/blockbustr/internal/resolve"
	"github.com/sysadmin/blockbustr/internal/segments"
	"github.com/sysadmin/blockbustr/internal/stremio"
	"github.com/sysadmin/blockbustr/internal/testutil"
)

// chapterProber probes each version (by its URL's path) as given.
type chapterProber struct {
	mu    sync.Mutex
	files map[string]media.Info
	calls int
	asked []media.Options
}

func (p *chapterProber) Probe(_ context.Context, target string, opts media.Options) (*media.Info, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	p.asked = append(p.asked, opts)
	u, _ := url.Parse(target)
	in, ok := p.files[u.Path]
	if !ok {
		return nil, fmt.Errorf("no file %s", u.Path)
	}
	in.Streams = []media.Stream{{Index: 0, Type: media.StreamVideo, Codec: "h264", Width: 1920, Height: 1080}}
	if opts.SkipChapters {
		in.Chapters = nil
	}
	return &in, nil
}

func (p *chapterProber) called() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

// animeGoT maps Game of Thrones (as if it were anime) to MAL anime 1.
type animeGoT struct{}

func (animeGoT) Lookup(imdb string, _, season, episode int) (int, int, bool) {
	return 1, episode, imdb == "tt0944947" && season == 1
}

type segmentsAnswer struct {
	Items []struct {
		Id, ItemId, Type     string
		StartTicks, EndTicks int64
	}
	TotalRecordCount int
}

func chapterAt(s int, title string) media.Chapter {
	return media.Chapter{Start: time.Duration(s) * time.Second, Title: title}
}

// An episode's segments are its played version's chapters: two versions
// with their intros at different times give the one the user streams.
// Anime without such chapters get AniSkip's, only when submitted against a
// file of the version's length.
func TestMediaSegments(t *testing.T) {
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { serveVideo(w, "x") }))
	t.Cleanup(cdn.Close)
	aniskip := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"found":true,"results":[{"interval":{"startTime":30,"endTime":120},"skipType":"op","episodeLength":1420}]}`))
	}))
	t.Cleanup(aniskip.Close)
	offer := func(path, height string) stremio.Offer {
		return stremio.Offer{Addon: "AIO", Cached: true, Stream: stremio.Stream{Name: "AIO " + height, URL: cdn.URL + path,
			Hints: stremio.StreamHints{Filename: "GoT" + strings.ReplaceAll(path, "/", ".")}}}
	}
	offers := map[string][]stremio.Offer{
		"series/tt0944947:1:1": {offer("/e1a.mkv", "1080p"), offer("/e1b.mkv", "720p")},
		"series/tt0944947:1:2": {offer("/e2.mkv", "1080p")},
		"series/tt0944947:1:3": {offer("/e3.mkv", "1080p")},
	}
	prober := &chapterProber{files: map[string]media.Info{
		"/e1a.mkv": {Duration: 3000 * time.Second, Chapters: []media.Chapter{chapterAt(0, "Cold Open"), chapterAt(60, "Opening"), chapterAt(150, "Episode")}},
		"/e1b.mkv": {Duration: 3000 * time.Second, Chapters: []media.Chapter{chapterAt(0, "Recap"), chapterAt(110, "Intro"),
			chapterAt(200, "Episode"), chapterAt(2800, "End Credits")}},
		"/e2.mkv": {Duration: 1422 * time.Second}, // AniSkip's submission: 1420 s
		"/e3.mkv": {Duration: 1500 * time.Second}, // another release
	}}
	sf := newSyncFixture(t, func(d *Deps) {
		d.Streams = &fakeStreams{offers: offers}
		d.ChoiceProber = prober
		d.Resolver = &resolve.Resolver{Cache: d.Cache, Log: testutil.Discard()}
		d.SkipTimes = &segments.AniSkip{BaseURL: aniskip.URL}
		d.AnimeList = animeGoT{}
		d.Config.Segments = config.Segments{Chapters: true, AniSkip: true}
	})
	sf.syncAll(t)
	auth := `MediaBrowser Token="` + captureToken + `"`
	var series string
	for _, v := range sf.views(t) {
		if v.CollectionType == "tvshows" {
			for _, it := range sf.children(t, v.Id) {
				if it.Name == "Game of Thrones" {
					series = it.Id
				}
			}
		}
	}
	var seasons struct{ Items []viewItem }
	getJSON(t, sf.h, "/Shows/"+series+"/Seasons", &seasons)
	var eps struct{ Items []viewItem }
	for _, s := range seasons.Items {
		if s.Name == "Season 1" {
			getJSON(t, sf.h, "/Shows/"+series+"/Episodes?seasonId="+s.Id, &eps)
		}
	}
	if len(eps.Items) < 3 {
		t.Fatalf("episodes: %v", eps.Items)
	}
	e1, e2, e3 := eps.Items[0].Id, eps.Items[1].Id, eps.Items[2].Id
	get := func(item, query string) segmentsAnswer {
		t.Helper()
		rec := call(t, sf.h, "GET", "/MediaSegments/"+item+query, auth, "")
		var out segmentsAnswer
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || rec.Code != 200 {
			t.Fatalf("MediaSegments: %d %s", rec.Code, rec.Body)
		}
		return out
	}
	describe := func(a segmentsAnswer) string {
		var out []string
		for _, s := range a.Items {
			out = append(out, fmt.Sprintf("%s %d-%d", s.Type, s.StartTicks/10_000_000, s.EndTicks/10_000_000))
		}
		return strings.Join(out, ", ")
	}
	waitProbes := func(n int) {
		for deadline := time.Now().Add(10 * time.Second); prober.called() < n && time.Now().Before(deadline); {
			time.Sleep(20 * time.Millisecond)
		}
	}

	// E1: PlaybackInfo offers both versions, best (1080p) first, and
	// probes them with their chapters.
	rec := call(t, sf.h, "POST", "/Items/"+e1+"/PlaybackInfo", auth, `{}`)
	var pb struct {
		MediaSources  []struct{ Id, Name string }
		PlaySessionId string
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &pb); err != nil || len(pb.MediaSources) != 2 {
		t.Fatalf("PlaybackInfo: %d %s", rec.Code, rec.Body)
	}
	waitProbes(2)
	prober.mu.Lock()
	for _, o := range prober.asked {
		if o.SkipChapters {
			t.Error("an episode's version was probed without its chapters")
		}
	}
	prober.mu.Unlock()
	if got := describe(get(e1, "")); got != "Intro 60-150" {
		t.Errorf("E1 before play (the session's first version): %q", got)
	}
	// The user plays the 720p: its chapters, its intro and credits.
	var b string
	for _, ms := range pb.MediaSources {
		if strings.Contains(ms.Name, "720p") {
			b = ms.Id
		}
	}
	if code := call(t, sf.h, "GET", "/Videos/"+e1+"/stream?static=true&MediaSourceId="+b+"&PlaySessionId="+pb.PlaySessionId, "", "").Code; code != 200 {
		t.Fatalf("stream the 720p: %d", code)
	}
	first := get(e1, "")
	if got := describe(first); got != "Recap 0-110, Intro 110-200, Outro 2800-3000" {
		t.Errorf("E1 playing the 720p: %q", got)
	}
	if again := get(e1, ""); len(again.Items) != len(first.Items) || len(first.Items) == 0 || again.Items[0].Id != first.Items[0].Id {
		t.Error("segment ids changed between calls")
	}
	if got := describe(get(e1, "?includeSegmentTypes=Outro")); got != "Outro 2800-3000" {
		t.Errorf("only outros: %q", got)
	}

	// E2 and E3 have no chapters and nobody played them: AniSkip's times,
	// submitted against 1420 s, fit E2's 1422 s version, not E3's 1500 s.
	for _, ep := range []string{e2, e3} {
		var detail struct{ Id string }
		getJSON(t, sf.h, "/Items/"+ep, &detail) // collects and probes its versions
	}
	waitProbes(4)
	if got := describe(get(e2, "")); got != "Intro 30-120" {
		t.Errorf("E2 (AniSkip, same length): %q", got)
	}
	if got := describe(get(e3, "")); got != "" {
		t.Errorf("E3 (AniSkip, another release): %q", got)
	}
}
