package segments

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sysadmin/blockbustr/internal/media"
	"github.com/sysadmin/blockbustr/internal/testutil"
)

func mmss(m, s int) time.Duration { return time.Duration(m)*time.Minute + time.Duration(s)*time.Second }

func ch(start time.Duration, title string) media.Chapter {
	return media.Chapter{Start: start, Title: title}
}

func TestFromChapters(t *testing.T) {
	anime := []media.Chapter{
		{Start: 0, Title: "Prologue"}, {Start: mmss(1, 32), Title: "Opening"}, {Start: mmss(3, 2), Title: "Part A"},
		{Start: mmss(12, 40), Title: "Part B"}, {Start: mmss(21, 50), Title: "Ending"}, {Start: mmss(23, 20), Title: " Preview "},
	}
	got := FromChapters(anime, mmss(24, 0))
	want := []Segment{
		{Intro, mmss(1, 32), mmss(3, 2)},
		{Outro, mmss(21, 50), mmss(23, 20)},
		{Preview, mmss(23, 20), mmss(24, 0)},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("anime chapters:\n got %v\nwant %v", got, want)
	}

	generic := []media.Chapter{ch(0, "Chapter 1"), ch(mmss(5, 0), "Chapter 2"), ch(mmss(10, 0), "00:10:00.000"), ch(mmss(15, 0), "Part B")}
	if got := FromChapters(generic, mmss(24, 0)); len(got) != 0 {
		t.Errorf("generic chapters: %v", got)
	}

	// A 6-minute "Opening" is a mislabelled chapter; an "OP" past 40% of
	// the episode isn't an intro.
	long := []media.Chapter{ch(0, "Opening"), ch(mmss(6, 0), "Episode"), ch(mmss(15, 0), "OP2"), ch(mmss(16, 30), "Part B")}
	if got := FromChapters(long, mmss(24, 0)); len(got) != 0 {
		t.Errorf("out-of-limits chapters: %v", got)
	}

	// Neighbouring chapters of one kind are one segment; the last runs to
	// the runtime, and without one gives none.
	split := []media.Chapter{ch(0, "Cold open"), ch(mmss(1, 0), "OP"), ch(mmss(1, 50), "Opening Credits"), ch(mmss(2, 30), "Main"), ch(mmss(40, 0), "Credits")}
	if got := FromChapters(split, mmss(41, 30)); fmt.Sprint(got) != fmt.Sprint([]Segment{{Intro, mmss(1, 0), mmss(2, 30)}, {Outro, mmss(40, 0), mmss(41, 30)}}) {
		t.Errorf("joined and last: %v", got)
	}
	if got := FromChapters(split, 0); len(got) != 1 || got[0].Type != Intro {
		t.Errorf("no runtime: %v, want the intro only", got)
	}
}

// aniSkipServer answers AniSkip's skip-times API with an opening and an
// ending submitted against an episode of length seconds.
func aniSkipServer(t *testing.T, length float64, inflight, most *atomic.Int32, delay time.Duration) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if inflight != nil {
			n := inflight.Add(1)
			defer inflight.Add(-1)
			for m := most.Load(); n > m && !most.CompareAndSwap(m, n); m = most.Load() {
			}
		}
		time.Sleep(delay)
		if !strings.HasPrefix(r.URL.Path, "/v2/skip-times/") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = fmt.Fprintf(w, `{"found":true,"results":[
			{"interval":{"startTime":92.5,"endTime":182.5},"skipType":"op","skipId":"a","episodeLength":%[1]g},
			{"interval":{"startTime":1310,"endTime":1400},"skipType":"ed","skipId":"b","episodeLength":%[1]g},
			{"interval":{"startTime":10,"endTime":100},"skipType":"op","skipId":"c","episodeLength":%[1]g}],
			"message":"Successfully found skip times","statusCode":200}`, length)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// AniSkip's times are kept only when submitted against a file as long as
// the version (±3 s); a release with another length shifts them all.
func TestAniSkipDrift(t *testing.T) {
	srv := aniSkipServer(t, 1420, nil, nil, 0)
	a := &AniSkip{BaseURL: srv.URL}
	got := a.Segments(t.Context(), 52991, 1, 1422*time.Second)
	want := []Segment{{Intro, seconds(92.5), seconds(182.5)}, {Outro, 1310 * time.Second, 1400 * time.Second}}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("2 s off:\n got %v\nwant %v (the first opening only)", got, want)
	}
	if got := a.Segments(t.Context(), 52991, 2, 1450*time.Second); len(got) != 0 {
		t.Errorf("30 s off: %v, want none", got)
	}
	if got := a.Segments(t.Context(), 52991, 3, 0); len(got) != 0 {
		t.Errorf("no runtime: %v, want none", got)
	}
}

// A slow or failing AniSkip is no segments, not an error, and the miss is
// remembered rather than asked again on every call.
func TestAniSkipTimeoutCachedAsMiss(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		select {
		case <-time.After(2 * time.Second):
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(srv.Close)
	a := &AniSkip{BaseURL: srv.URL, HTTP: &http.Client{Timeout: 100 * time.Millisecond}, Cache: testutil.Cache(t), Log: testutil.Discard()}
	if got := a.Segments(t.Context(), 1, 1, 24*time.Minute); len(got) != 0 {
		t.Errorf("timeout: %v", got)
	}
	if got := a.Segments(t.Context(), 1, 1, 24*time.Minute); len(got) != 0 || hits.Load() != 1 {
		t.Errorf("again: %v, %d requests; want the miss remembered", got, hits.Load())
	}
	var cached []skipTime
	if ok, err := a.Cache.GetJSON(context.Background(), aniSkipKey(1, 1), &cached); err != nil || !ok || len(cached) != 0 {
		t.Errorf("miss cached: %v %v %v", ok, err, cached)
	}
}

// However many episodes are asked for at once, aniskip.com sees at most
// aniSkipParallel requests.
func TestAniSkipBounded(t *testing.T) {
	var inflight, most atomic.Int32
	srv := aniSkipServer(t, 1420, &inflight, &most, 30*time.Millisecond)
	a := &AniSkip{BaseURL: srv.URL}
	var wg sync.WaitGroup
	for ep := 1; ep <= 20; ep++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			a.Segments(t.Context(), 52991, ep, 1420*time.Second)
		}()
	}
	wg.Wait()
	if most.Load() > aniSkipParallel {
		t.Errorf("%d requests in flight, want at most %d", most.Load(), aniSkipParallel)
	}
}

// An episode maps to the MAL anime of its TVDB season's cour: a season's
// later episodes can be another anime's first ones.
func TestAnimeListLookup(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/master.xml":
			_, _ = w.Write([]byte(`<anime-list>
				<anime anidbid="10" tvdbid="500" defaulttvdbseason="1" episodeoffset=""><name>Show</name></anime>
				<anime anidbid="11" tvdbid="500" defaulttvdbseason="1" episodeoffset="11"><name>Show cour 2</name></anime>
				<anime anidbid="12" tvdbid="500" defaulttvdbseason="2" episodeoffset=""><name>Show 2</name></anime>
				<anime anidbid="13" tvdbid="500" defaulttvdbseason="0" episodeoffset=""><name>Show specials</name></anime>
				<anime anidbid="14" tvdbid="movie" defaulttvdbseason="1"><name>Film</name></anime>
				<anime anidbid="15" tvdbid="600" defaulttvdbseason="a"><name>Absolute</name></anime>
			</anime-list>`))
		case "/full.json":
			_, _ = w.Write([]byte(`[
				{"mal_id":100,"anidb_id":10,"tvdb_id":500,"imdb_id":["tt500"],"themoviedb_id":{"tv":5000},"season":{"tvdb":1}},
				{"mal_id":101,"anidb_id":11,"tvdb_id":500,"imdb_id":["tt500"],"themoviedb_id":{"tv":5000}},
				{"mal_id":102,"anidb_id":12,"tvdb_id":500,"imdb_id":null,"themoviedb_id":null},
				{"mal_id":103,"anidb_id":13},
				{"mal_id":104,"anidb_id":14,"imdb_id":"tt14"}]`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	l := &AnimeList{TVDBURL: srv.URL + "/master.xml", IDsURL: srv.URL + "/full.json"}
	if err := l.Load(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		imdb             string
		tmdb, season, ep int
		mal, malEp       int
		ok               bool
	}{
		{"tt500", 0, 1, 3, 100, 3, true},
		{"tt500", 0, 1, 14, 101, 3, true}, // the second cour
		{"tt500", 0, 2, 1, 102, 1, true},  // season 2's own anime, not season 1's
		{"", 5000, 2, 4, 102, 4, true},    // by TMDB id
		{"tt500", 0, 3, 1, 0, 0, false},   // a season none covers
		{"tt999", 0, 1, 1, 0, 0, false},   // not anime
	} {
		mal, ep, ok := l.Lookup(c.imdb, c.tmdb, c.season, c.ep)
		if mal != c.mal || ep != c.malEp || ok != c.ok {
			t.Errorf("Lookup(%q, %d, S%d E%d) = %d %d %v, want %d %d %v", c.imdb, c.tmdb, c.season, c.ep, mal, ep, ok, c.mal, c.malEp, c.ok)
		}
	}
}
