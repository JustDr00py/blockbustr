package stremio

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sysadmin/blockbustr/internal/testutil"
)

// testdata/ holds real responses from Cinemeta and unconfigured Torrentio
// (fetched 2026-10-06, trimmed to a few entries).

// secretConfig stands in for a configured addon's settings, which often
// include a debrid key; it must never appear in an error.
const secretConfig = "sort=qualitysize|realdebrid=SECRETKEY123"

// fakeAddon serves testdata files by request path under /{secretConfig}/
// and records the raw paths it was asked for.
func fakeAddon(t *testing.T, routes map[string]string) (base string, paths *[]string, hits *atomic.Int32) {
	t.Helper()
	var seen []string
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		p := strings.TrimPrefix(r.URL.EscapedPath(), "/"+strings.ReplaceAll(secretConfig, "|", "%7C"))
		seen = append(seen, p)
		file, ok := routes[p]
		if !ok {
			http.NotFound(w, r)
			return
		}
		b, err := os.ReadFile("testdata/" + file)
		if err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(b)
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/" + secretConfig, &seen, &n
}

func TestBaseURL(t *testing.T) {
	for in, want := range map[string]string{
		"https://torrentio.strem.fun/manifest.json":               "https://torrentio.strem.fun",
		"  https://torrentio.strem.fun/sort=size/manifest.json  ": "https://torrentio.strem.fun/sort=size",
		"stremio://torrentio.strem.fun/sort=size/manifest.json":   "https://torrentio.strem.fun/sort=size",
		"https://v3-cinemeta.strem.io/":                           "https://v3-cinemeta.strem.io",
		"http://comet.lan:8000/abc/manifest.json?x=1#frag":        "http://comet.lan:8000/abc",
		"https://h.example/a%7Cb/manifest.json":                   "https://h.example/a%7Cb",
	} {
		if got, err := BaseURL(in); err != nil || got != want {
			t.Errorf("%q: %q %v, want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "torrentio.strem.fun/manifest.json", "ftp://x/manifest.json", "https:///manifest.json"} {
		if _, err := BaseURL(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if got := Redact("https://torrentio.strem.fun/" + secretConfig); got != "https://torrentio.strem.fun" {
		t.Errorf("Redact: %q", got)
	}
	if AddonKey("https://a/x") == AddonKey("https://a/y") || len(AddonKey("https://a/x")) != 16 {
		t.Error("AddonKey must tell configurations apart, in 16 hex")
	}
}

func TestManifests(t *testing.T) {
	base, _, _ := fakeAddon(t, map[string]string{
		"/manifest.json": "torrentio-manifest.json",
	})
	c := &Client{}
	tio, err := c.Manifest(t.Context(), base)
	if err != nil || tio.ID == "" || !tio.BehaviorHints.Configurable {
		t.Fatalf("%+v %v", tio, err)
	}
	var cm Manifest
	b, _ := os.ReadFile("testdata/cinemeta-manifest.json")
	if err := json.Unmarshal(b, &cm); err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct {
		m                 Manifest
		resource, typ, id string
		want              bool
	}{
		{tio, "stream", "movie", "tt0133093", true}, // object form: types and prefixes of its own
		{tio, "stream", "series", "tt0944947:1:2", true},
		{tio, "stream", "anime", "kitsu:1", true},
		{tio, "stream", "movie", "yt_id:abc", false},
		{tio, "stream", "channel", "tt1", false},
		{tio, "meta", "movie", "tt0133093", false},
		{cm, "meta", "series", "tt0944947", true}, // string form: the manifest's types and prefixes
		{cm, "catalog", "movie", "top", false},    // "top" isn't an IMDb id
		{cm, "stream", "movie", "tt0133093", false},
	} {
		if got := c.m.Supports(c.resource, c.typ, c.id); got != c.want {
			t.Errorf("%s Supports(%s, %s, %s) = %v", c.m.ID, c.resource, c.typ, c.id, got)
		}
	}

	byID := map[string]CatalogDef{}
	for _, cat := range cm.Catalogs {
		byID[cat.Type+"/"+cat.ID] = cat
	}
	top, year, last := byID["movie/top"], byID["movie/year"], byID["series/last-videos"]
	if !top.Accepts("search") || !top.Accepts("skip") || len(top.Requires()) != 0 {
		t.Errorf("movie/top: %+v", top)
	}
	if year.Accepts("search") || strings.Join(year.Requires(), ",") != "genre" {
		t.Errorf("movie/year: requires %v", year.Requires())
	}
	if strings.Join(last.Requires(), ",") != "lastVideosIds" {
		t.Errorf("series/last-videos: requires %v", last.Requires())
	}
	// Legacy-only declarations still count.
	legacy := CatalogDef{ExtraSupported: []string{"search"}, ExtraRequired: []string{"search"}}
	if !legacy.Accepts("search") || strings.Join(legacy.Requires(), ",") != "search" {
		t.Errorf("legacy extras: %v", legacy.Requires())
	}
}

func TestCatalogExtrasEncoding(t *testing.T) {
	base, paths, _ := fakeAddon(t, map[string]string{
		"/catalog/movie/top/search=the%20matrix.json":              "cinemeta-catalog-search.json",
		"/catalog/movie/top.json":                                  "cinemeta-catalog-search.json",
		"/catalog/movie/year/genre=Sci-Fi&skip=100.json":           "cinemeta-catalog-search.json",
		"/catalog/movie/top/search=50%25%20%26%20more%2Fless.json": "cinemeta-catalog-search.json",
	})
	c := &Client{}
	metas, err := c.Catalog(t.Context(), base, "movie", "top", Extra{Search: "the matrix"})
	if err != nil || len(metas) != 3 {
		t.Fatalf("%v %v (asked %v)", len(metas), err, *paths)
	}
	if metas[0].ID != "tt0133093" || metas[0].Name != "The Matrix" || metas[0].Year() != 1999 || metas[0].Poster == "" {
		t.Errorf("first: %+v", metas[0])
	}
	for _, e := range []Extra{{}, {Genre: "Sci-Fi", Skip: 100}, {Search: "50% & more/less"}} {
		typ, id := "movie", "top"
		if e.Genre != "" {
			id = "year"
		}
		if _, err := c.Catalog(t.Context(), base, typ, id, e); err != nil {
			t.Errorf("%+v: %v (asked %s)", e, err, (*paths)[len(*paths)-1])
		}
	}
}

func TestMetaSeries(t *testing.T) {
	base, _, hits := fakeAddon(t, map[string]string{"/meta/series/tt0944947.json": "cinemeta-meta-series.json"})
	c := &Client{Cache: testutil.Cache(t)}
	m, err := c.Meta(t.Context(), base, "series", "tt0944947")
	if err != nil {
		t.Fatal(err)
	}
	if m.Name != "Game of Thrones" || m.Year() != 2011 || m.ReleaseInfo != "2011–2019" || m.IMDbRating != "9.2" || m.MovieDBID != "1399" {
		t.Errorf("meta: %q %d %q %q %q", m.Name, m.Year(), m.ReleaseInfo, m.IMDbRating, m.MovieDBID)
	}
	if len(m.Videos) == 0 {
		t.Fatal("no videos")
	}
	v := m.Videos[0]
	if v.ID != "tt0944947:0:1" || v.Season != 0 || v.EpisodeNumber() != 1 || v.Name != "Inside Game of Thrones" || v.Released == "" {
		t.Errorf("video: %+v", v)
	}
	if _, err := c.Meta(t.Context(), base, "series", "tt0944947"); err != nil || hits.Load() != 1 {
		t.Errorf("cached meta refetched: hits %d, %v", hits.Load(), err)
	}
	if _, err := c.Meta(t.Context(), base, "series", "tt0000000"); err == nil {
		t.Error("missing meta: no error")
	}
}

func TestStreams(t *testing.T) {
	base, paths, hits := fakeAddon(t, map[string]string{
		"/stream/movie/tt0133093.json":      "torrentio-stream-movie.json",
		"/stream/series/tt0944947:1:2.json": "torrentio-stream-episode.json",
		"/stream/movie/tt0000001.json":      "empty-streams.json",
	})
	if err := os.WriteFile("testdata/empty-streams.json", []byte(`{"streams":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove("testdata/empty-streams.json") })
	c := &Client{Cache: testutil.Cache(t)}

	movie, err := c.Streams(t.Context(), base, "movie", "tt0133093")
	if err != nil || len(movie) != 4 {
		t.Fatalf("%d %v", len(movie), err)
	}
	s := movie[0]
	if s.Name != "Torrentio\n4k HDR" || len(s.InfoHash) != 40 || s.InfoHash != strings.ToLower(s.InfoHash) || s.FileIdx != nil ||
		!s.Playable() || s.Details() == "" || s.Hints.BingeGroup == "" {
		t.Errorf("movie stream: %+v", s)
	}
	if movie[1].FileIdx == nil || *movie[1].FileIdx != 2 || !strings.HasSuffix(movie[1].Hints.Filename, ".mkv") {
		t.Errorf("fileIdx / filename: %+v", movie[1])
	}

	ep, err := c.Streams(t.Context(), base, "series", "tt0944947:1:2")
	if err != nil || len(ep) == 0 || ep[0].Hints.Filename != "S01E02 - The Kingsroad.mkv" {
		t.Errorf("episode: %v %v (asked %v)", ep, err, *paths)
	}

	before := hits.Load()
	if again, err := c.Streams(t.Context(), base, "movie", "tt0133093"); err != nil || len(again) != 4 || hits.Load() != before {
		t.Errorf("cached streams refetched: hits %d→%d, %v", before, hits.Load(), err)
	}
	// "None" is cached too, and is an empty list, not nil.
	for range 2 {
		none, err := c.Streams(t.Context(), base, "movie", "tt0000001")
		if err != nil || none == nil || len(none) != 0 {
			t.Errorf("empty: %v %v", none, err)
		}
	}
	if hits.Load() != before+1 {
		t.Errorf("empty result refetched: %d", hits.Load()-before)
	}

	if (Stream{ExternalURL: "https://x"}).Playable() || (Stream{YtID: "abc"}).Playable() {
		t.Error("external and YouTube streams aren't playable")
	}
	if (Stream{Title: "t", Description: "d"}).Details() != "d" {
		t.Error("description wins over title")
	}
}

func TestSubtitles(t *testing.T) {
	if err := os.WriteFile("testdata/subs.json", []byte(`{"subtitles":[{"id":"1","url":"https://subs.example/1.srt","lang":"eng"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove("testdata/subs.json") })
	base, _, _ := fakeAddon(t, map[string]string{"/subtitles/movie/tt0133093.json": "subs.json"})
	subs, err := (&Client{}).Subtitles(t.Context(), base, "movie", "tt0133093")
	if err != nil || len(subs) != 1 || subs[0].Lang != "eng" || subs[0].URL == "" {
		t.Errorf("%+v %v", subs, err)
	}
}

// No error may carry the addon's configuration.
func TestErrorsNeverLeakTheBaseURL(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "slow") {
			select {
			case <-r.Context().Done():
			case <-time.After(2 * time.Second):
			}
			return
		}
		if strings.Contains(r.URL.Path, "garbage") {
			_, _ = w.Write([]byte("<html>not json"))
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(slow.Close)
	closed := httptest.NewServer(http.NotFoundHandler())
	closedURL := closed.URL
	closed.Close()

	c := &Client{Timeout: 100 * time.Millisecond}
	base := slow.URL + "/" + secretConfig
	cases := map[string]error{}
	_, cases["500"] = c.Streams(t.Context(), base, "movie", "tt1")
	_, cases["timeout"] = c.Streams(t.Context(), base, "movie", "slow")
	_, cases["decode"] = c.Streams(t.Context(), base, "movie", "garbage")
	_, cases["refused"] = c.Manifest(t.Context(), closedURL+"/"+secretConfig)
	_, cases["no meta"] = c.Meta(t.Context(), base, "movie", "tt1")
	for name, err := range cases {
		if err == nil {
			t.Errorf("%s: no error", name)
			continue
		}
		msg := err.Error()
		if strings.Contains(msg, "SECRETKEY") || strings.Contains(msg, "realdebrid=") || strings.Contains(msg, "sort=") {
			t.Errorf("%s leaks the config: %s", name, msg)
		}
		if !strings.Contains(msg, "stremio http://127.0.0.1") {
			t.Errorf("%s doesn't name the addon host: %s", name, msg)
		}
	}
	if !strings.Contains(cases["500"].Error(), "stream/movie/tt1: HTTP 500") {
		t.Errorf("500: %v", cases["500"])
	}
	if !strings.Contains(cases["timeout"].Error(), "deadline exceeded") {
		t.Errorf("timeout: %v", cases["timeout"])
	}
}

func TestText(t *testing.T) {
	var v struct{ A, B, C, D Text }
	if err := json.Unmarshal([]byte(`{"A":"2011–2019","B":7.5,"C":603,"D":null}`), &v); err != nil {
		t.Fatal(err)
	}
	if v.A != "2011–2019" || v.B != "7.5" || v.C != "603" || v.D != "" {
		t.Errorf("%+v", v)
	}
	if err := json.Unmarshal([]byte(`{"A":{"x":1}}`), &v); err == nil {
		t.Error("object accepted as text")
	}
}
