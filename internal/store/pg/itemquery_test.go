package pg_test

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sysadmin/blockbustr/internal/store/pg"
	"github.com/sysadmin/blockbustr/internal/store/pg/db"
	"github.com/sysadmin/blockbustr/internal/testutil"
)

func p[T any](v T) *T { return &v }

// queryFixture is a small library: three movies, one series with a season
// and two episodes, plus a missing movie and a disabled library.
type queryFixture struct {
	pool                     db.DBTX
	user                     uuid.UUID
	movies, shows            uuid.UUID
	luca, fockers, mk, gone  uuid.UUID
	series, season, ep1, ep2 uuid.UUID
	hidden                   uuid.UUID
}

func newQueryFixture(t *testing.T) queryFixture {
	t.Helper()
	q, pool := testutil.Queries(t)
	ctx := t.Context()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	u, err := q.CreateUser(ctx, db.CreateUserParams{Name: "u"})
	if err != nil {
		t.Fatal(err)
	}
	f := queryFixture{pool: pool, user: u.ID}
	lib := func(name, kind string) (uuid.UUID, uuid.UUID) {
		l, err := q.UpsertLibrary(ctx, db.UpsertLibraryParams{Name: name, Kind: kind, Paths: []string{"/" + name}})
		if err != nil {
			t.Fatal(err)
		}
		folder, err := q.EnsureCollectionFolder(ctx, db.EnsureCollectionFolderParams{LibraryID: l.ID, Name: name, SortName: strings.ToLower(name)})
		if err != nil {
			t.Fatal(err)
		}
		return l.ID, folder
	}
	item := func(libID, parent, top uuid.UUID, typ, name string, year int32, premiere string) uuid.UUID {
		it, err := q.CreateItem(ctx, db.CreateItemParams{LibraryID: libID, ParentID: &parent, TopParentID: &top, Type: typ, Name: name,
			SortName: strings.ToLower(name), SourceKind: "file", ProductionYear: &year})
		if err != nil {
			t.Fatal(err)
		}
		if premiere != "" {
			exec(`UPDATE items SET premiere_date = $2 WHERE id = $1`, it.ID, premiere)
		}
		return it.ID
	}
	mLib, movies := lib("Movies", "movies")
	sLib, shows := lib("Shows", "tvshows")
	xLib, xFolder := lib("Hidden", "movies")
	f.movies, f.shows = movies, shows
	f.luca = item(mLib, movies, movies, "Movie", "Luca", 2021, "2021-06-17")
	f.fockers = item(mLib, movies, movies, "Movie", "Little Fockers", 2010, "2010-12-22")
	f.mk = item(mLib, movies, movies, "Movie", "Mortal Kombat II", 2026, "")
	f.gone = item(mLib, movies, movies, "Movie", "Lucarne", 2000, "")
	exec(`UPDATE items SET missing_since = now(), is_missing = true WHERE id = $1`, f.gone)
	f.series = item(sLib, shows, shows, "Series", "MF GHOST", 2023, "2023-10-02")
	f.season = item(sLib, f.series, shows, "Season", "Season 1", 2023, "")
	f.ep1 = item(sLib, f.season, shows, "Episode", "The Challenger from England", 2023, "2023-10-02")
	f.ep2 = item(sLib, f.season, shows, "Episode", "Pégaso", 2023, "2023-10-09")
	f.hidden = item(xLib, xFolder, xFolder, "Movie", "Luca Hidden", 2021, "")
	if _, err := q.DisableLibrariesExcept(ctx, []uuid.UUID{mLib, sLib}); err != nil {
		t.Fatal(err)
	}
	exec(`UPDATE items SET provider_ids = '{"Tmdb":"508943","Imdb":"tt12801262"}' WHERE id = $1`, f.luca)
	exec(`INSERT INTO genres (name) VALUES ('Animation'), ('Comedy')`)
	exec(`INSERT INTO item_genres (item_id, genre_id) SELECT $1, id FROM genres`, f.luca)
	exec(`INSERT INTO item_genres (item_id, genre_id) SELECT $1, id FROM genres WHERE name = 'Comedy'`, f.fockers)
	exec(`INSERT INTO user_data (user_id, item_id, is_favorite, playback_position_ticks) VALUES ($1, $2, true, 0), ($1, $3, false, 600000000)`, u.ID, f.fockers, f.mk)
	exec(`INSERT INTO user_data (user_id, item_id, played, play_count, last_played_at) VALUES ($1, $2, true, 1, $3)`, u.ID, f.ep1, time.Now())
	return f
}

func names(t *testing.T, f queryFixture, q pg.ItemQuery) ([]string, int) {
	t.Helper()
	q.UserID = f.user
	res, err := pg.QueryItems(t.Context(), f.pool, q)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, len(res.Items))
	for i, it := range res.Items {
		out[i] = it.Name
	}
	return out, res.Total
}

func TestQueryItems(t *testing.T) {
	f := newQueryFixture(t)
	all := []string{"Little Fockers", "Luca", "Mortal Kombat II"}
	cases := []struct {
		name string
		q    pg.ItemQuery
		want []string
	}{
		{"library children by sort name", pg.ItemQuery{ParentID: &f.movies}, all},
		{"root without recursive lists libraries", pg.ItemQuery{}, []string{"Movies", "Shows"}},
		{"recursive movies+series", pg.ItemQuery{Recursive: true, IncludeTypes: []string{"movie", "Series"}}, []string{"Little Fockers", "Luca", "MF GHOST", "Mortal Kombat II"}},
		{"recursive under a series", pg.ItemQuery{ParentID: &f.series, Recursive: true, IncludeTypes: []string{"Episode"}, SortBy: []pg.SortKey{{By: "PremiereDate"}}}, []string{"The Challenger from England", "Pégaso"}},
		{"exclude types", pg.ItemQuery{Recursive: true, ExcludeTypes: []string{"Movie", "Episode"}}, []string{"MF GHOST", "Season 1"}},
		{"media type video", pg.ItemQuery{Recursive: true, MediaTypes: []string{"Video"}, SortBy: []pg.SortKey{{By: "Name"}}}, []string{"Little Fockers", "Luca", "Mortal Kombat II", "Pégaso", "The Challenger from England"}},
		{"premiere desc nulls last", pg.ItemQuery{ParentID: &f.movies, SortBy: []pg.SortKey{{By: "PremiereDate", Descending: true}}}, []string{"Luca", "Little Fockers", "Mortal Kombat II"}},
		{"ids keep missing and disabled out", pg.ItemQuery{IDs: []uuid.UUID{f.luca, f.gone, f.hidden}}, []string{"Luca"}},
		{"exclude ids", pg.ItemQuery{ParentID: &f.movies, ExcludeIDs: []uuid.UUID{f.luca}}, []string{"Little Fockers", "Mortal Kombat II"}},
		{"search prefix and accents", pg.ItemQuery{Recursive: true, SearchTerm: "pegas"}, []string{"Pégaso"}},
		{"search ranks exact title first", pg.ItemQuery{Recursive: true, SearchTerm: "luca"}, []string{"Luca"}},
		{"search substring", pg.ItemQuery{Recursive: true, SearchTerm: "ombat"}, []string{"Mortal Kombat II"}},
		{"search with sql-ish input", pg.ItemQuery{Recursive: true, SearchTerm: `%' OR 1=1 --`}, []string{}},
		{"genre by name", pg.ItemQuery{Recursive: true, Genres: []string{"comedy"}}, []string{"Little Fockers", "Luca"}},
		{"years", pg.ItemQuery{Recursive: true, Years: []int32{2010, 2026}}, []string{"Little Fockers", "Mortal Kombat II"}},
		{"provider id", pg.ItemQuery{Recursive: true, ProviderIDs: map[string][]string{"tmdb": {"508943"}}}, []string{"Luca"}},
		{"favorite", pg.ItemQuery{Recursive: true, IsFavorite: p(true)}, []string{"Little Fockers"}},
		{"resumable", pg.ItemQuery{Recursive: true, IsResumable: p(true)}, []string{"Mortal Kombat II"}},
		{"played episode", pg.ItemQuery{Recursive: true, IsPlayed: p(true)}, []string{"The Challenger from England"}},
		{"series unplayed until every episode is", pg.ItemQuery{Recursive: true, IncludeTypes: []string{"Series"}, IsPlayed: p(false)}, []string{"MF GHOST"}},
		{"folders only", pg.ItemQuery{Recursive: true, IsFolder: p(true)}, []string{"MF GHOST", "Season 1"}},
		{"name starts with", pg.ItemQuery{Recursive: true, NameStartsWith: "l"}, []string{"Little Fockers", "Luca"}},
		{"favourites first", pg.ItemQuery{ParentID: &f.movies, SortBy: []pg.SortKey{{By: "IsFavoriteOrLiked", Descending: true}, {By: "SortName"}}}, []string{"Little Fockers", "Luca", "Mortal Kombat II"}},
		{"unknown sort ignored", pg.ItemQuery{ParentID: &f.movies, SortBy: []pg.SortKey{{By: "Album"}}}, all},
	}
	for _, c := range cases {
		got, _ := names(t, f, c.q)
		if strings.Join(got, "|") != strings.Join(c.want, "|") {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestQueryItemsPaging(t *testing.T) {
	f := newQueryFixture(t)
	for _, c := range []struct {
		start, limit int
		count        bool
		want         string
		total        int
	}{
		{0, 2, true, "Little Fockers|Luca", 3},
		{2, 2, true, "Mortal Kombat II", 3},
		{5, 2, true, "", 3},
		{0, 0, true, "Little Fockers|Luca|Mortal Kombat II", 3},
		{0, 2, false, "Little Fockers|Luca", 2}, // like Jellyfin with enableTotalRecordCount=false
	} {
		got, total := names(t, f, pg.ItemQuery{ParentID: &f.movies, StartIndex: c.start, Limit: c.limit, Count: c.count})
		if strings.Join(got, "|") != c.want || total != c.total {
			t.Errorf("start %d limit %d count %v: %q total %d", c.start, c.limit, c.count, got, total)
		}
	}
	// A search page past the end needs the count query, which must not get
	// the ranking (ORDER BY-only) arguments.
	for _, c := range []struct {
		start, limit int
		want         string
		total        int
	}{{5, 0, "", 1}, {1, 1, "", 1}, {0, 1, "Luca", 1}} {
		got, total := names(t, f, pg.ItemQuery{Recursive: true, SearchTerm: "lu", StartIndex: c.start, Limit: c.limit, Count: true})
		if strings.Join(got, "|") != c.want || total != c.total {
			t.Errorf("search start %d limit %d: %q total %d", c.start, c.limit, got, total)
		}
	}
}
