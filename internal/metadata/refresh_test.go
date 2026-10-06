package metadata

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sysadmin/blockbustr/internal/config"
	"github.com/sysadmin/blockbustr/internal/library"
	"github.com/sysadmin/blockbustr/internal/media"
	"github.com/sysadmin/blockbustr/internal/metadata/tmdb"
	"github.com/sysadmin/blockbustr/internal/store/pg/db"
	"github.com/sysadmin/blockbustr/internal/testutil"
)

type noProbe struct{}

func (noProbe) Probe(context.Context, string, media.Options) (*media.Info, error) {
	return &media.Info{Container: "matroska,webm"}, nil
}

// fakeTMDB serves just enough of the API for the test library.
func fakeTMDB(t *testing.T, calls *atomic.Int32) *tmdb.Client {
	t.Helper()
	routes := map[string]string{
		"/search/movie?Luca":         `{"results":[{"id":9,"title":"Luca's Legacy","release_date":"2021-01-01"},{"id":508943,"title":"Luca","release_date":"2021-06-17"}]}`,
		"/search/movie?Unknown Film": `{"results":[]}`,
		"/movie/508943": `{"id":508943,"imdb_id":"tt12801262","title":"Luca","original_title":"Luca","overview":"Two sea monsters.","tagline":"Hello summer.",
			"release_date":"2021-06-17","runtime":95,"vote_average":7.8,"genres":[{"name":"Animation"},{"name":"Comedy"}],"production_companies":[{"name":"Pixar"}],
			"credits":{"cast":[{"id":1,"name":"Jacob Tremblay","character":"Luca Paguro (voice)","order":0,"profile_path":"/jt.jpg"}],
			"crew":[{"id":2,"name":"Enrico Casarosa","job":"Director"},{"id":3,"name":"Jesse Andrews","job":"Screenplay"},{"id":3,"name":"Jesse Andrews","job":"Writer"},{"id":4,"name":"Grip Person","job":"Grip"}]},
			"images":{"posters":[{"file_path":"/p.jpg","iso_639_1":"en","vote_average":5}],"backdrops":[{"file_path":"/b.jpg","iso_639_1":null}],"logos":[{"file_path":"/l.png","iso_639_1":"en"}]},
			"release_dates":{"results":[{"iso_3166_1":"US","release_dates":[{"certification":"PG","type":3}]}]}}`,
		"/find/tt0133093":     `{"movie_results":[{"id":603}],"tv_results":[]}`,
		"/movie/603":          `{"id":603,"imdb_id":"tt0133093","title":"The Matrix","overview":"TMDB plot","tagline":"Welcome to the Real World.","release_date":"1999-03-30","genres":[{"name":"Action"}]}`,
		"/search/tv?MF Ghost": `{"results":[{"id":235493,"name":"MF Ghost","first_air_date":"2023-10-02"}]}`,
		"/tv/235493": `{"id":235493,"name":"MF Ghost","overview":"Racing.","first_air_date":"2023-10-02","status":"Returning Series","vote_average":7.1,
			"genres":[{"name":"Animation"}],"networks":[{"name":"Tokyo MX"}],"external_ids":{"imdb_id":"tt27526575","tvdb_id":425707},
			"content_ratings":{"results":[{"iso_3166_1":"US","rating":"TV-14"}]},"credits":{"cast":[{"id":7,"name":"Daisuke Hirose","character":"Kanata Rivington"}]},
			"images":{"posters":[{"file_path":"/mfp.jpg","iso_639_1":"en"}]}}`,
		"/tv/235493/season/1": `{"season_number":1,"name":"MF GHOST","overview":"First season.","air_date":"2023-10-02","poster_path":"/s1.jpg",
			"episodes":[{"id":4615001,"episode_number":1,"name":"The Challenger from England","overview":"Ep one.","air_date":"2023-10-02","still_path":"/e1.jpg","vote_average":7.5,
				"guest_stars":[{"id":8,"name":"Guest Voice","character":"Commentator"}],"crew":[{"id":9,"name":"Ep Director","job":"Director"}]},
			{"episode_number":2,"name":"The Shocking New MFG Generation","air_date":"2023-10-09"}]}`,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		key := r.URL.Path
		if q := r.URL.Query().Get("query"); q != "" {
			key += "?" + q
		}
		body, ok := routes[key]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	c := tmdb.New("key", "en-US")
	c.SetBaseURL(srv.URL)
	return c
}

type env struct {
	t          *testing.T
	root       string
	q          *db.Queries
	pool       *pgxpool.Pool
	scanner    *library.Scanner
	movies, tv db.Library
}

func newEnv(t *testing.T) *env {
	t.Helper()
	q, pool := testutil.Queries(t)
	e := &env{t: t, root: t.TempDir(), q: q, pool: pool}
	e.scanner = library.NewScanner(pool, testutil.Cache(t), noProbe{}, config.Scan{ProbeWorkers: 1, ProbeTimeout: time.Minute, MissingGrace: time.Hour}, testutil.Discard())
	write := func(rel, content string) {
		p := filepath.Join(e.root, rel)
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("Movies/Luca (2021)/Luca (2021).strm", "http://jellybird/x\n")
	write("Movies/Unknown Film (1999)/Unknown Film (1999).mkv", "x")
	write("Movies/The Matrix (1999)/The Matrix (1999).mkv", "x")
	write("Movies/The Matrix (1999)/The Matrix (1999).nfo", `<movie><title>The Matrix (Director's Cut)</title><plot>nfo plot</plot><uniqueid type="imdb">tt0133093</uniqueid></movie>`)
	write("Series/MF Ghost/Season 01/MF Ghost S01E01.mkv", "x")
	write("Series/MF Ghost/Season 01/MF Ghost S01E02.mkv", "x")
	write("Series/MF Ghost/Season 01/MF Ghost S01E02.nfo", `<episodedetails><title>Custom Title</title><season>1</season><episode>2</episode></episodedetails>`)
	libs, err := e.scanner.SyncLibraries(t.Context(), []config.Library{
		{Name: "Movies", Kind: "movies", Paths: []string{filepath.Join(e.root, "Movies")}},
		{Name: "Shows", Kind: "tvshows", Paths: []string{filepath.Join(e.root, "Series")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	e.movies, e.tv = libs[0], libs[1]
	e.scan()
	return e
}

func (e *env) scan() {
	e.t.Helper()
	for _, lib := range []db.Library{e.movies, e.tv} {
		if _, err := e.scanner.Scan(e.t.Context(), lib); err != nil {
			e.t.Fatal(err)
		}
	}
}

func (e *env) refresh(r *Refresher) {
	e.t.Helper()
	for _, lib := range []db.Library{e.movies, e.tv} {
		if err := r.Refresh(e.t.Context(), lib); err != nil {
			e.t.Fatal(err)
		}
	}
}

// byType returns a library's items of one type, keyed by name.
func (e *env) byType(lib db.Library, typ string) map[string]db.Item {
	e.t.Helper()
	rows, err := e.q.ListLibraryItems(e.t.Context(), lib.ID)
	if err != nil {
		e.t.Fatal(err)
	}
	out := map[string]db.Item{}
	for _, r := range rows {
		if r.Type == typ {
			out[r.Name] = r
		}
	}
	return out
}

func keys[V any](m map[string]V) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func TestRefreshWithTMDBAndNFO(t *testing.T) {
	e := newEnv(t)
	var calls atomic.Int32
	r := NewRefresher(e.pool, fakeTMDB(t, &calls), testutil.Discard())
	e.refresh(r)
	ctx := t.Context()

	movies := e.byType(e.movies, "Movie")
	if got := strings.Join(keys(movies), "|"); got != "Luca|The Matrix (Director's Cut)|Unknown Film" {
		t.Fatalf("movie names: %s", got)
	}

	luca := movies["Luca"]
	if deref(luca.Overview) != "Two sea monsters." || deref(luca.Tagline) != "Hello summer." || deref(luca.OfficialRating) != "PG" ||
		deref(luca.CommunityRating) != 7.8 || luca.PremiereDate == nil || luca.PremiereDate.Format("2006-01-02") != "2021-06-17" ||
		deref(luca.MetadataSource) != "tmdb" || luca.OriginalTitle != nil || deref(luca.RuntimeTicks) != 95*60*10_000_000 {
		t.Errorf("Luca: %+v", luca)
	}
	var ids map[string]string
	_ = json.Unmarshal(luca.ProviderIds, &ids)
	if ids["Tmdb"] != "508943" || ids["Imdb"] != "tt12801262" {
		t.Errorf("Luca provider ids: %v (the exact title must beat the first search hit)", ids)
	}
	if g, _ := e.q.ListItemGenres(ctx, luca.ID); strings.Join(g, ",") != "Animation,Comedy" {
		t.Errorf("genres %v", g)
	}
	if s, _ := e.q.ListItemStudios(ctx, luca.ID); strings.Join(s, ",") != "Pixar" {
		t.Errorf("studios %v", s)
	}
	people, _ := e.q.ListItemPeople(ctx, luca.ID)
	var credits []string
	for _, p := range people {
		credits = append(credits, p.Kind+":"+p.Name+":"+deref(p.Role))
	}
	if strings.Join(credits, "|") != "Actor:Jacob Tremblay:Luca Paguro (voice)|Director:Enrico Casarosa:Director|Writer:Jesse Andrews:Screenplay" {
		t.Errorf("people: %v (writer once, grip dropped)", credits)
	}
	if deref(people[0].ImageUrl) != "https://image.tmdb.org/t/p/original/jt.jpg" {
		t.Errorf("actor image: %v", people[0].ImageUrl)
	}
	imgs, _ := e.q.ListItemImages(ctx, luca.ID)
	var art []string
	for _, im := range imgs {
		art = append(art, im.Type+"="+strings.TrimPrefix(deref(im.SourceUrl), tmdb.ImageBase))
		if len(im.Tag) != 16 {
			t.Errorf("image tag %q", im.Tag)
		}
	}
	if strings.Join(art, ",") != "Backdrop=/b.jpg,Logo=/l.png,Primary=/p.jpg" {
		t.Errorf("artwork: %v", art)
	}

	if u := movies["Unknown Film"]; deref(u.MetadataSource) != "none" || u.MetadataRefreshedAt == nil {
		t.Errorf("unmatched: %+v", u)
	}

	matrix := movies["The Matrix (Director's Cut)"]
	if deref(matrix.Overview) != "nfo plot" || deref(matrix.Tagline) != "Welcome to the Real World." || deref(matrix.MetadataSource) != "nfo+tmdb" {
		t.Errorf("nfo over TMDB: %+v", matrix)
	}

	series := e.byType(e.tv, "Series")["MF Ghost"]
	_ = json.Unmarshal(series.ProviderIds, &ids)
	if deref(series.Overview) != "Racing." || deref(series.OfficialRating) != "TV-14" || ids["Tvdb"] != "425707" || series.EndDate != nil {
		t.Errorf("series: %+v %v", series, ids)
	}
	if s, _ := e.q.ListItemStudios(ctx, series.ID); strings.Join(s, ",") != "Tokyo MX" {
		t.Errorf("series studios (networks): %v", s)
	}
	// TMDB's season name repeats the show name, so "Season 1" is kept.
	if season, ok := e.byType(e.tv, "Season")["Season 1"]; !ok || deref(season.Overview) != "First season." {
		t.Errorf("season: %v %+v", keys(e.byType(e.tv, "Season")), season)
	}
	eps := e.byType(e.tv, "Episode")
	if got := strings.Join(keys(eps), "|"); got != "Custom Title|The Challenger from England" {
		t.Fatalf("episode names: %s (nfo title wins for E02)", got)
	}
	ep1 := eps["The Challenger from England"]
	if deref(ep1.Overview) != "Ep one." || ep1.SortName != "0001 episode 1" {
		t.Errorf("episode 1: overview %q sort %q (sort names stay number-based)", deref(ep1.Overview), ep1.SortName)
	}
	_ = json.Unmarshal(ep1.ProviderIds, &ids)
	if ids["Tmdb"] != "4615001" {
		t.Errorf("episode provider ids: %s", ep1.ProviderIds)
	}
	epPeople, _ := e.q.ListItemPeople(ctx, ep1.ID)
	if len(epPeople) != 2 || epPeople[0].Kind != "GuestStar" || epPeople[1].Kind != "Director" {
		t.Errorf("episode people: %+v", epPeople)
	}

	// Rescans keep metadata names; a second refresh has nothing to do.
	e.scan()
	if got := strings.Join(keys(e.byType(e.movies, "Movie")), "|"); got != "Luca|The Matrix (Director's Cut)|Unknown Film" {
		t.Errorf("rescan reset names: %s", got)
	}
	if got := strings.Join(keys(e.byType(e.tv, "Episode")), "|"); got != "Custom Title|The Challenger from England" {
		t.Errorf("rescan reset episode names: %s", got)
	}
	before := calls.Load()
	e.refresh(r)
	if calls.Load() != before {
		t.Errorf("second refresh called TMDB %d more times", calls.Load()-before)
	}
}

func TestRefreshWithoutTMDBUsesNFOOnly(t *testing.T) {
	e := newEnv(t)
	e.refresh(NewRefresher(e.pool, nil, testutil.Discard()))
	movies := e.byType(e.movies, "Movie")
	if m := movies["The Matrix (Director's Cut)"]; deref(m.MetadataSource) != "nfo" || deref(m.Overview) != "nfo plot" {
		t.Errorf("nfo-only movie: %+v", m)
	}
	if l := movies["Luca"]; deref(l.MetadataSource) != "none" {
		t.Errorf("Luca without TMDB: %v", deref(l.MetadataSource))
	}
	if eps := e.byType(e.tv, "Episode"); eps["Custom Title"].MetadataSource == nil {
		t.Error("episode nfo not applied")
	}
}

func TestBestMatch(t *testing.T) {
	res := []tmdb.Result{
		{ID: 1, Title: "Amélie 2", ReleaseDate: "2001-01-01"},
		{ID: 2, Title: "Amélie", ReleaseDate: "2001-04-25"},
		{ID: 3, Title: "Amélie", ReleaseDate: "2015-01-01"},
	}
	if got := bestMatch(res, "Amelie", 2001); got != 2 {
		t.Errorf("accent-folded exact title + year = %d", got)
	}
	if got := bestMatch(res, "Amélie", 2015); got != 3 {
		t.Errorf("year picks the remake: %d", got)
	}
	if got := bestMatch(res, "Amélie", 1990); got != 0 {
		t.Errorf("wrong year must not match: %d", got)
	}
	if got := bestMatch([]tmdb.Result{{ID: 5, Title: "Something Else", ReleaseDate: "1999-01-01"}}, "Some Thing", 1999); got != 5 {
		t.Errorf("single result with the right year: %d", got)
	}
	if got := bestMatch([]tmdb.Result{{ID: 5, Title: "Something Else"}}, "Some Thing", 0); got != 0 {
		t.Errorf("single result without a year must not match blindly: %d", got)
	}
	if normTitle("Tom & Jerry: The Movie!") != "tomandjerrythemovie" {
		t.Error("normTitle")
	}
}

// A Stremio catalog item has no files (no .nfo) but carries its IMDb id:
// that id is looked up directly, never the name.
func TestRefreshCatalogItemByIMDb(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	folder, err := e.q.EnsureCollectionFolder(ctx, db.EnsureCollectionFolderParams{LibraryID: e.movies.ID, Name: e.movies.Name, SortName: "movies"})
	if err != nil {
		t.Fatal(err)
	}
	path := "stremio:movie:tt0133093"
	if _, err := e.q.UpsertStremioItem(ctx, db.UpsertStremioItemParams{
		LibraryID: e.movies.ID, ParentID: &folder, TopParentID: &folder, Type: "Movie",
		Name: "No Search Would Find This", SortName: "no search would find this", Path: &path,
		StremioRef: []byte(`{"addonId":"x","type":"movie","id":"tt0133093"}`), ProviderIds: []byte(`{"Imdb":"tt0133093"}`),
	}); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	e.refresh(NewRefresher(e.pool, fakeTMDB(t, &calls), testutil.Discard()))
	var got db.Item
	for _, it := range e.byType(e.movies, "Movie") {
		if deref(it.Path) == path {
			got = it
		}
	}
	var ids map[string]string
	_ = json.Unmarshal(got.ProviderIds, &ids)
	if got.Name != "The Matrix" || deref(got.Overview) != "TMDB plot" || ids["Tmdb"] != "603" || deref(got.MetadataSource) != "tmdb" {
		t.Errorf("catalog item: %q %q %v %v", got.Name, deref(got.Overview), ids, deref(got.MetadataSource))
	}
}
