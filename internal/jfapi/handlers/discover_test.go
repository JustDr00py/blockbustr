package handlers

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sysadmin/blockbustr/internal/jfapi/dto"
	"github.com/sysadmin/blockbustr/internal/stremio"
	"github.com/sysadmin/blockbustr/internal/testutil"
)

// fakeSearchAddon is an addon with search-only catalogs: "matrix" finds The
// Matrix (in the synced library) and two titles that aren't (a movie and a
// series), "thrones" finds Game of Thrones (in the library). Its series has
// meta with two episodes.
func fakeSearchAddon(t *testing.T) (string, *atomic.Int32) {
	t.Helper()
	var searches atomic.Int32
	manifest := `{"id":"test.search","name":"Search","version":"1","types":["movie","series"],"idPrefixes":["tt"],
		"resources":["catalog","meta"],
		"catalogs":[{"type":"movie","id":"find","name":"Find","extra":[{"name":"search","isRequired":true}]},
		            {"type":"series","id":"find","name":"Find","extra":[{"name":"search","isRequired":true}]}]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, _ := url.PathUnescape(r.URL.EscapedPath())
		switch {
		case strings.HasSuffix(p, "/manifest.json"):
			_, _ = w.Write([]byte(manifest))
		case strings.HasPrefix(p, "/catalog/movie/find/search=matrix"):
			searches.Add(1)
			_, _ = w.Write([]byte(`{"metas":[
				{"id":"tt0133093","type":"movie","name":"The Matrix","releaseInfo":"1999"},
				{"id":"tt9999901","type":"movie","name":"Matrix Fan Film","releaseInfo":"2025","poster":"https://img.example/p.jpg","description":"Not in any library."},
				{"id":"kitsu:1","type":"movie","name":"No IMDb id"}]}`))
		case strings.HasPrefix(p, "/catalog/series/find/search=matrix"):
			searches.Add(1)
			_, _ = w.Write([]byte(`{"metas":[{"id":"tt9999902","type":"series","name":"Matrix: The Series","releaseInfo":"2024–"}]}`))
		case strings.HasPrefix(p, "/catalog/series/find/search=thrones"):
			_, _ = w.Write([]byte(`{"metas":[{"id":"tt0944947","type":"series","name":"Game of Thrones"}]}`))
		case strings.HasPrefix(p, "/catalog/"):
			_, _ = w.Write([]byte(`{"metas":[]}`))
		case p == "/meta/series/tt9999902.json":
			_, _ = w.Write([]byte(`{"meta":{"id":"tt9999902","type":"series","name":"Matrix: The Series","videos":[
				{"id":"tt9999902:1:1","season":1,"episode":1,"title":"Pilot","released":"2024-01-01T00:00:00.000Z"},
				{"id":"tt9999902:1:2","season":1,"episode":2,"title":"Second","released":"2024-01-08T00:00:00.000Z"}]}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/manifest.json", &searches
}

type searchHit struct {
	Id, Name, Type string
	ProviderIds    map[string]string
	ImageTags      map[string]string
}

func TestSearchBeyondLibrary(t *testing.T) {
	var disc *stremio.Discover
	sf := newSyncFixture(t, func(d *Deps) {
		disc = &stremio.Discover{Registry: d.Addons, Cache: d.Cache, Log: testutil.Discard(), Timeout: 5 * time.Second}
		d.RemoteSearch = disc
	})
	sf.syncAll(t)
	addonURL, searches := fakeSearchAddon(t)
	if _, err := disc.Registry.Add(t.Context(), addonURL, 5); err != nil {
		t.Fatal(err)
	}
	auth := `MediaBrowser Token="` + captureToken + `"`
	search := func(term, types string) []searchHit {
		t.Helper()
		var out struct{ Items []searchHit }
		getJSON(t, sf.h, "/Items?recursive=true&limit=20&fields=ProviderIds&searchTerm="+url.QueryEscape(term)+"&includeItemTypes="+types, &out)
		return out.Items
	}
	names := func(hits []searchHit) []string {
		var out []string
		for _, h := range hits {
			out = append(out, h.Name)
		}
		return out
	}
	library := map[string]string{}
	for _, it := range sf.children(t, sf.views(t)["Cinemeta Popular"].Id) {
		library[it.Name] = it.Id
	}

	// The library's matches first, then the remote titles that aren't in
	// it, movies and series interleaved by rank. The Matrix, which both
	// have, appears once as the library's item; titles without an IMDb id
	// are left out.
	hits := search("matrix", "Movie,Series")
	got := names(hits)
	want := []string{"The Matrix", "The Matrix Reloaded", "The Matrix Resurrections", "Matrix: The Series", "Matrix Fan Film"}
	if strings.Join(got, " | ") != strings.Join(want, " | ") {
		t.Fatalf("hits:\n %q\nwant\n %q", got, want)
	}
	if hits[0].Id != library["The Matrix"] {
		t.Errorf("The Matrix is %s, want the library's %s", hits[0].Id, library["The Matrix"])
	}
	series, fan := hits[3], hits[4]
	if fan.Id != dto.IDFromUUID(stremio.DiscoverID("tt9999901")).String() || fan.Type != "Movie" || fan.ProviderIds["Imdb"] != "tt9999901" || fan.ImageTags["Primary"] == "" {
		t.Errorf("fan film: %+v", fan)
	}
	if series.Type != "Series" {
		t.Errorf("series: %+v", series)
	}

	// The same search again: the same ids, and the addon isn't asked again.
	before := searches.Load()
	again := search("Matrix", "Movie,Series")
	if len(again) != 5 || again[3].Id != series.Id || again[4].Id != fan.Id {
		t.Errorf("again: %q", names(again))
	}
	if searches.Load() != before {
		t.Error("the repeated search asked the addon again")
	}
	// Type filters apply to remote results too.
	if movies := names(search("matrix", "Movie")); slices.Contains(movies, "Matrix: The Series") || !slices.Contains(movies, "Matrix Fan Film") {
		t.Errorf("movies only: %q", movies)
	}

	// A found title opens like any other, but browsing never shows it and
	// the discover library isn't a view.
	var detail struct{ Name, Overview string }
	getJSON(t, sf.h, "/Items/"+fan.Id, &detail)
	if detail.Name != "Matrix Fan Film" || detail.Overview != "Not in any library." {
		t.Errorf("detail: %+v", detail)
	}
	var browse struct{ Items []searchHit }
	getJSON(t, sf.h, "/Items?recursive=true&includeItemTypes=Movie,Series&limit=500", &browse)
	for _, it := range browse.Items {
		if it.Id == fan.Id || it.Id == series.Id {
			t.Errorf("browsing shows a search result: %s", it.Name)
		}
	}
	if len(sf.views(t)) != 2 {
		t.Errorf("views: %v", sf.views(t))
	}

	// A found series gets its episodes when opened.
	var seasons struct{ Items []searchHit }
	getJSON(t, sf.h, "/Shows/"+series.Id+"/Seasons", &seasons)
	if len(seasons.Items) != 1 || seasons.Items[0].Name != "Season 1" {
		t.Fatalf("seasons: %+v", seasons.Items)
	}
	var eps struct{ Items []searchHit }
	getJSON(t, sf.h, "/Shows/"+series.Id+"/Episodes?seasonId="+seasons.Items[0].Id, &eps)
	if strings.Join(names(eps.Items), ",") != "Pilot,Second" {
		t.Errorf("episodes: %q", names(eps.Items))
	}

	// A series in the library is found as itself, not as a discover copy.
	if got := search("thrones", "Series"); len(got) != 1 || got[0].Id == dto.IDFromUUID(stremio.DiscoverID("tt0944947")).String() {
		t.Errorf("thrones: %+v", got)
	}

	// Cleanup: titles not found again within the retention go, unless
	// someone favourited (or played) them.
	if rec := call(t, sf.h, "POST", "/UserFavoriteItems/"+fan.Id, auth, ""); rec.Code != 200 {
		t.Fatalf("favourite: %d %s", rec.Code, rec.Body)
	}
	if _, err := testPool.Exec(t.Context(), `UPDATE items SET date_last_refreshed = now() - interval '30 days' WHERE id = ANY($1::uuid[])`,
		[]string{stremio.DiscoverID("tt9999901").String(), stremio.DiscoverID("tt9999902").String()}); err != nil {
		t.Fatal(err)
	}
	if n, err := disc.Cleanup(t.Context(), 7*24*time.Hour); err != nil || n != 1 {
		t.Errorf("Cleanup = %d, %v; want the series only", n, err)
	}
	if rec := call(t, sf.h, "GET", "/Items/"+fan.Id, auth, ""); rec.Code != 200 {
		t.Errorf("favourited title cleaned up: %d", rec.Code)
	}
	if rec := call(t, sf.h, "GET", "/Items/"+series.Id, auth, ""); rec.Code != 404 {
		t.Errorf("stale series kept: %d", rec.Code)
	}
	// Found again later: the same id, so clients' cached ids keep working.
	if again := search("matrix", "Series"); len(again) != 1 || again[0].Id != series.Id {
		t.Errorf("found again: %+v, want id %s", again, series.Id)
	}
}
