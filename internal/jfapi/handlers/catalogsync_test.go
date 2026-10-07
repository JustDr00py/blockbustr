package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sysadmin/blockbustr/internal/config"
	"github.com/sysadmin/blockbustr/internal/jfapi"
	"github.com/sysadmin/blockbustr/internal/library"
	"github.com/sysadmin/blockbustr/internal/secret"
	"github.com/sysadmin/blockbustr/internal/stremio"
	"github.com/sysadmin/blockbustr/internal/testutil"
)

// fakeCinemeta serves Cinemeta's real manifest, a 3-movie catalog (The
// Matrix trilogy, paged), a 1-series catalog (Game of Thrones) and the
// series' meta: the real specials plus synthetic Season 1 episodes, one of
// them not aired yet.
type fakeCinemeta struct {
	mu      sync.Mutex
	movies  []map[string]any
	failAll bool // every request but the manifest 500s
	noMeta  bool // meta requests 500
	paths   []string
}

func newFakeCinemeta(t *testing.T) (*fakeCinemeta, string) {
	t.Helper()
	read := func(name string) []byte {
		b, err := os.ReadFile("../../stremio/testdata/" + name)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	var cat struct{ Metas []map[string]any }
	_ = json.Unmarshal(read("cinemeta-catalog-search.json"), &cat)
	var series struct{ Meta map[string]any }
	_ = json.Unmarshal(read("cinemeta-meta-series.json"), &series)
	videos := series.Meta["videos"].([]any)
	for i, released := range []string{"2011-04-17T00:00:00.000Z", "2011-04-24T00:00:00.000Z", "2011-05-01T00:00:00.000Z", "2999-01-01T00:00:00.000Z"} {
		n := string(rune('1' + i))
		videos = append(videos, map[string]any{
			"id": "tt0944947:1:" + n, "season": 1, "episode": i + 1, "number": i + 1,
			"name": []string{"Winter Is Coming", "The Kingsroad", "Lord Snow", "Not Yet"}[i], "released": released,
			"description": "Episode overview", "thumbnail": "https://episodes.example/" + n + ".jpg",
		})
	}
	series.Meta["videos"] = videos
	seriesMeta, _ := json.Marshal(series)
	seriesCat, _ := json.Marshal(map[string]any{"metas": []any{map[string]any{
		"id": "tt0944947", "type": "series", "name": "Game of Thrones", "releaseInfo": "2011–2019",
		"poster": series.Meta["poster"], "background": series.Meta["background"], "moviedb_id": 1399,
	}}})
	manifest := read("cinemeta-manifest.json")

	f := &fakeCinemeta{movies: cat.Metas}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		p := r.URL.EscapedPath()
		f.paths = append(f.paths, p)
		switch {
		case strings.HasSuffix(p, "/manifest.json"):
			_, _ = w.Write(manifest)
		case f.failAll:
			w.WriteHeader(500)
		case strings.HasSuffix(p, "/catalog/movie/top.json"): // page 1: two titles
			_ = json.NewEncoder(w).Encode(map[string]any{"metas": f.movies[:min(2, len(f.movies))]})
		case strings.HasSuffix(p, "/catalog/movie/top/skip=2.json"): // page 2: the rest
			_ = json.NewEncoder(w).Encode(map[string]any{"metas": f.movies[min(2, len(f.movies)):]})
		case strings.HasSuffix(p, "/catalog/series/top.json"):
			_, _ = w.Write(seriesCat)
		case strings.HasSuffix(p, "/catalog/series/top/skip=1.json"):
			_, _ = w.Write([]byte(`{"metas":[]}`))
		case strings.HasSuffix(p, "/meta/series/tt0944947.json") && !f.noMeta:
			_, _ = w.Write(seriesMeta)
		default:
			w.WriteHeader(500)
		}
	}))
	t.Cleanup(srv.Close)
	return f, srv.URL + "/manifest.json"
}

type syncFixture struct {
	h    http.Handler
	sync *stremio.Syncer
	id   string // the addon's id
	f    *fakeCinemeta
}

// newSyncFixture builds the server with a fake Cinemeta added and both its
// catalogs enabled; opts adjust the dependencies first.
func newSyncFixture(t *testing.T, opts ...func(*Deps)) *syncFixture {
	t.Helper()
	_, d := newIntegrationServer(t)
	seedCaptureToken(t, d)
	f, manifestURL := newFakeCinemeta(t)
	reg := &stremio.Registry{Pool: testPool, Client: &stremio.Client{}, Key: bytes.Repeat([]byte{7}, secret.KeyLen)}
	s := &stremio.Syncer{Registry: reg, Log: testutil.Discard(), Pages: 2, MissingGrace: time.Hour}
	d.Addons, d.CatalogSync = reg, s
	for _, o := range opts {
		o(&d)
	}
	rt := jfapi.NewRouter(testutil.Discard(), jfapi.Options{LegacyAuth: true})
	Register(rt, d)
	sf := &syncFixture{h: rt, sync: s, f: f}
	ad, err := reg.Add(t.Context(), manifestURL, 0)
	if err != nil {
		t.Fatal(err)
	}
	sf.id = strings.ReplaceAll(ad.ID.String(), "-", "")
	for _, c := range []string{"movie/top", "series/top"} {
		if rec := call(t, rt, "POST", "/blockbustr/addons/"+sf.id+"/catalogs/"+c, `MediaBrowser Token="`+captureToken+`"`, `{"Enabled":true}`); rec.Code != 200 {
			t.Fatalf("enable %s: %d %s", c, rec.Code, rec.Body)
		}
	}
	return sf
}

func (sf *syncFixture) syncAll(t *testing.T) {
	t.Helper()
	if err := sf.sync.SyncAll(t.Context()); err != nil {
		t.Fatal(err)
	}
}

type viewItem struct {
	Id, Name, Type, CollectionType string
	ProviderIds                    map[string]string
	ImageTags                      map[string]string
	BackdropImageTags              []string
	ProductionYear                 int
	IndexNumber, ParentIndexNumber int
	Overview                       string
}

func (sf *syncFixture) views(t *testing.T) map[string]viewItem {
	t.Helper()
	var v struct{ Items []viewItem }
	getJSON(t, sf.h, "/UserViews", &v)
	out := map[string]viewItem{}
	for _, it := range v.Items {
		out[it.Name] = it
	}
	return out
}

func (sf *syncFixture) children(t *testing.T, parent string) []viewItem {
	t.Helper()
	var v struct{ Items []viewItem }
	getJSON(t, sf.h, "/Items?ParentId="+parent+"&SortBy=SortName&Fields=ProviderIds,Overview", &v)
	return v.Items
}

func itemNames(items []viewItem) string {
	var n []string
	for _, it := range items {
		n = append(n, it.Name)
	}
	return strings.Join(n, " | ")
}

func TestCatalogSyncBuildsLibraries(t *testing.T) {
	sf := newSyncFixture(t)
	sf.syncAll(t)

	views := sf.views(t)
	movies, shows := views["Popular Movies"], views["Popular Shows"]
	if movies.Id == "" || shows.Id == "" {
		t.Fatalf("views: %v", views)
	}
	if movies.CollectionType != "movies" || shows.CollectionType != "tvshows" {
		t.Errorf("collection types: %q %q", movies.CollectionType, shows.CollectionType)
	}

	// All pages of the movie catalog, with ids and artwork from Cinemeta.
	ms := sf.children(t, movies.Id)
	if itemNames(ms) != "The Matrix | The Matrix Reloaded | The Matrix Resurrections" {
		t.Errorf("movies: %s", itemNames(ms))
	}
	if m := ms[0]; m.Type != "Movie" || m.ProductionYear != 1999 || m.ProviderIds["Imdb"] != "tt0133093" || m.ImageTags["Primary"] == "" || len(m.BackdropImageTags) != 1 {
		t.Errorf("The Matrix: %+v", m)
	}
	// Opening one works before it has any source (streams come with P3.7).
	// Its placeholder source is remote and never shows the internal path.
	var detail struct {
		Name         string
		MediaSources []struct {
			Name, Path, Protocol string
			IsRemote             bool
		}
	}
	getJSON(t, sf.h, "/Items/"+ms[0].Id, &detail)
	if len(detail.MediaSources) != 1 {
		t.Fatalf("detail: %+v", detail)
	}
	if src := detail.MediaSources[0]; src.Name != "The Matrix" || src.Protocol != "Http" || !src.IsRemote ||
		strings.Contains(src.Path, "stremio:") || !strings.Contains(src.Path, "/Videos/"+ms[0].Id+"/stream") {
		t.Errorf("placeholder source: %+v", src)
	}
	rec := call(t, sf.h, "POST", "/Items/"+ms[0].Id+"/PlaybackInfo", `MediaBrowser Token="`+captureToken+`"`, `{}`)
	if rec.Code != 200 || strings.Contains(rec.Body.String(), "stremio:") {
		t.Errorf("PlaybackInfo = %d %.300s", rec.Code, rec.Body)
	}
	if !strings.Contains(strings.Join(sf.f.paths, " "), "/catalog/movie/top/skip=2.json") {
		t.Errorf("second page never asked: %v", sf.f.paths)
	}

	// The series, its seasons and its aired episodes.
	ss := sf.children(t, shows.Id)
	if len(ss) != 1 || ss[0].Type != "Series" || ss[0].ProviderIds["Tmdb"] != "1399" || ss[0].ProviderIds["Imdb"] != "tt0944947" {
		t.Fatalf("series: %+v", ss)
	}
	var seasons struct{ Items []viewItem }
	getJSON(t, sf.h, "/Shows/"+ss[0].Id+"/Seasons", &seasons)
	if itemNames(seasons.Items) != "Specials | Season 1" {
		t.Errorf("seasons: %s", itemNames(seasons.Items))
	}
	var eps struct{ Items []viewItem }
	if len(seasons.Items) != 2 {
		t.FailNow()
	}
	getJSON(t, sf.h, "/Shows/"+ss[0].Id+"/Episodes?seasonId="+seasons.Items[1].Id+"&Fields=Overview", &eps)
	if itemNames(eps.Items) != "Winter Is Coming | The Kingsroad | Lord Snow" {
		t.Errorf("season 1 (the unaired episode left out): %s", itemNames(eps.Items))
	}
	if len(eps.Items) == 3 {
		if e := eps.Items[1]; e.IndexNumber != 2 || e.ParentIndexNumber != 1 || e.Overview != "Episode overview" || e.ImageTags["Primary"] == "" {
			t.Errorf("S01E02: %+v", e)
		}
	}
	getJSON(t, sf.h, "/Shows/"+ss[0].Id+"/Episodes?seasonId="+seasons.Items[0].Id, &eps)
	if len(eps.Items) != 12 {
		t.Errorf("specials: %d", len(eps.Items))
	}

	// A restart's config sync doesn't switch catalog libraries off.
	sc := library.NewScanner(testPool, nil, nil, config.Defaults().Scan, testutil.Discard())
	if _, err := sc.SyncLibraries(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	if v := sf.views(t); v["Popular Movies"].Id == "" || v["Popular Shows"].Id == "" {
		t.Errorf("config sync disabled catalog libraries: %v", v)
	}

	// Syncing again changes nothing (same items, same ids).
	sf.syncAll(t)
	if again := sf.children(t, movies.Id); itemNames(again) != itemNames(ms) || again[0].Id != ms[0].Id {
		t.Errorf("second sync: %s", itemNames(again))
	}
}

func TestCatalogSyncChanges(t *testing.T) {
	sf := newSyncFixture(t)
	sf.syncAll(t)
	movies := sf.views(t)["Popular Movies"]

	// A title leaves the catalog: hidden now, deleted after the grace.
	sf.f.mu.Lock()
	sf.f.movies = sf.f.movies[:2]
	sf.f.mu.Unlock()
	sf.syncAll(t)
	if got := itemNames(sf.children(t, movies.Id)); got != "The Matrix | The Matrix Reloaded" {
		t.Errorf("after a title left: %s", got)
	}

	// A series' meta failing keeps its episodes; the addon failing keeps
	// the whole library as it was.
	shows := sf.views(t)["Popular Shows"]
	series := sf.children(t, shows.Id)[0].Id
	var before struct{ Items []viewItem }
	getJSON(t, sf.h, "/Shows/"+series+"/Episodes", &before)
	sf.f.mu.Lock()
	sf.f.noMeta = true
	sf.f.mu.Unlock()
	sf.syncAll(t)
	var after struct{ Items []viewItem }
	getJSON(t, sf.h, "/Shows/"+series+"/Episodes", &after)
	if len(after.Items) != len(before.Items) || len(after.Items) != 15 {
		t.Errorf("episodes after a meta failure: %d, before %d", len(after.Items), len(before.Items))
	}
	sf.f.mu.Lock()
	sf.f.failAll = true
	sf.f.mu.Unlock()
	if err := sf.sync.SyncAll(t.Context()); err == nil {
		t.Error("catalog failures not reported")
	}
	if got := itemNames(sf.children(t, movies.Id)); got != "The Matrix | The Matrix Reloaded" {
		t.Errorf("after the addon failed: %s", got)
	}
	sf.f.mu.Lock()
	sf.f.failAll, sf.f.noMeta = false, false
	sf.f.mu.Unlock()

	// Disabling the catalog hides its library; enabling brings the same one back.
	authz := `MediaBrowser Token="` + captureToken + `"`
	call(t, sf.h, "POST", "/blockbustr/addons/"+sf.id+"/catalogs/movie/top", authz, `{"Enabled":false}`)
	sf.syncAll(t)
	if _, ok := sf.views(t)["Popular Movies"]; ok {
		t.Error("disabled catalog still listed")
	}
	call(t, sf.h, "POST", "/blockbustr/addons/"+sf.id+"/catalogs/movie/top", authz, `{"Enabled":true}`)
	sf.syncAll(t)
	if v := sf.views(t)["Popular Movies"]; v.Id != movies.Id {
		t.Errorf("re-enabled library: %q, want %q", v.Id, movies.Id)
	}

	// Disabling the whole addon hides all its libraries.
	call(t, sf.h, "POST", "/blockbustr/addons/"+sf.id, authz, `{"Enabled":false}`)
	sf.syncAll(t)
	if v := sf.views(t); len(v) != 0 {
		t.Errorf("disabled addon still has views: %v", v)
	}
}

// TestCatalogSyncLive syncs the real Cinemeta's popular catalogs into the
// test database. Only with BLOCKBUSTR_LIVE_STREMIO=1.
func TestCatalogSyncLive(t *testing.T) {
	if os.Getenv("BLOCKBUSTR_LIVE_STREMIO") == "" {
		t.Skip("BLOCKBUSTR_LIVE_STREMIO not set")
	}
	_, d := newIntegrationServer(t)
	seedCaptureToken(t, d)
	reg := &stremio.Registry{Pool: testPool, Client: &stremio.Client{}, Key: bytes.Repeat([]byte{7}, secret.KeyLen)}
	s := &stremio.Syncer{Registry: reg, Log: testutil.Discard(), Pages: 2}
	d.Addons = reg
	rt := jfapi.NewRouter(testutil.Discard(), jfapi.Options{LegacyAuth: true})
	Register(rt, d)
	sf := &syncFixture{h: rt, sync: s}
	ad, err := reg.Add(t.Context(), "https://v3-cinemeta.strem.io/manifest.json", 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []string{"movie", "series"} {
		if _, err := reg.SetCatalogEnabled(t.Context(), ad.ID, c, "top", true); err != nil {
			t.Fatal(err)
		}
	}
	start := time.Now()
	sf.syncAll(t)
	views := sf.views(t)
	movies, shows := sf.children(t, views["Popular Movies"].Id), sf.children(t, views["Popular Shows"].Id)
	var eps int
	_ = testPool.QueryRow(t.Context(), `SELECT count(*) FROM items WHERE type = 'Episode' AND missing_since IS NULL`).Scan(&eps)
	t.Logf("synced in %v: %d movies, %d series, %d episodes; first: %s, %s", time.Since(start).Round(time.Millisecond), len(movies), len(shows), eps, movies[0].Name, shows[0].Name)
	if len(movies) < 60 || len(shows) < 60 || eps < 1000 {
		t.Errorf("too little synced")
	}
}

// An admin renames a catalog library; clients see the new name, and the
// next sync keeps it. Config libraries are named in config.yaml (404), a
// taken name is refused (409), and only admins may rename.
func TestRenameCatalogLibrary(t *testing.T) {
	sf := newSyncFixture(t)
	sf.syncAll(t)
	admin := `MediaBrowser Token="` + captureToken + `"`
	movies := sf.views(t)["Popular Movies"].Id
	rename := func(folder, body, authz string) int {
		t.Helper()
		return call(t, sf.h, "POST", "/blockbustr/libraries/"+folder, authz, body).Code
	}
	if code := rename(movies, `{"Name":"  Films  "}`, admin); code != 204 {
		t.Fatalf("rename = %d", code)
	}
	sf.syncAll(t)
	v := sf.views(t)
	if _, ok := v["Films"]; !ok || len(sf.children(t, v["Films"].Id)) == 0 {
		t.Errorf("after rename and sync: %v", v)
	}
	if code := rename(movies, `{"Name":"Popular Shows"}`, admin); code != 409 {
		t.Errorf("taken name = %d, want 409", code)
	}
	if code := rename(movies, `{"Name":" "}`, admin); code != 400 {
		t.Errorf("blank name = %d, want 400", code)
	}
	if code := rename(movies, `{"Name":"x"}`, ""); code != 401 {
		t.Errorf("anonymous = %d, want 401", code)
	}
	// A file library from config.yaml isn't renamed here.
	execSQL(t, `INSERT INTO libraries (name, kind) VALUES ('Home Movies', 'movies')`)
	var folder string
	if err := testPool.QueryRow(t.Context(), `INSERT INTO items (library_id, type, name, sort_name)
		SELECT id, 'CollectionFolder', name, name FROM libraries WHERE name = 'Home Movies' RETURNING replace(id::text, '-', '')`).Scan(&folder); err != nil {
		t.Fatal(err)
	}
	if code := rename(folder, `{"Name":"Mine"}`, admin); code != 404 {
		t.Errorf("config library = %d, want 404", code)
	}
}
