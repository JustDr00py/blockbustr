package handlers

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/sysadmin/blockbustr/internal/jfapi/dto"
	"github.com/sysadmin/blockbustr/internal/store/pg"
)

// fakeIMDb resolves TMDB ids as TMDB's external ids do.
type fakeIMDb map[int]string

func (f fakeIMDb) Enabled() bool { return true }

func (f fakeIMDb) IMDbID(_ context.Context, _ string, id int) (string, error) {
	if imdb, ok := f[id]; ok {
		return imdb, nil
	}
	return "", fmt.Errorf("no imdb id for %d", id)
}

// A title several catalogs list is one item, in each of their libraries,
// with one watch state: even when a catalog lists it by its TMDB id. A
// series' episodes are written once. A title one catalog drops leaves that
// library only.
func TestOneItemPerTitle(t *testing.T) {
	sf := newSyncFixture(t)
	sf.sync.IMDb = fakeIMDb{604: "tt0234215"}
	sf.f.extra = map[string][]map[string]any{
		"movie/imdbRating": {
			{"id": "tt0133093", "type": "movie", "name": "The Matrix", "releaseInfo": "1999"},
			{"id": "tmdb:604", "type": "movie", "name": "The Matrix Reloaded", "releaseInfo": "2003"},
		},
		"series/imdbRating": {
			{"id": "tt0944947", "type": "series", "name": "Game of Thrones", "releaseInfo": "2011–2019"},
		},
	}
	for _, c := range []string{"movie/imdbRating", "series/imdbRating"} {
		if rec := call(t, sf.h, "POST", "/blockbustr/addons/"+sf.id+"/catalogs/"+c, `MediaBrowser Token="`+captureToken+`"`, `{"Enabled":true}`); rec.Code != 200 {
			t.Fatalf("enable %s: %d %s", c, rec.Code, rec.Body)
		}
	}
	sf.syncAll(t)

	views := sf.views(t)
	var movieViews, seriesViews []viewItem
	for _, v := range views {
		switch v.CollectionType {
		case "movies":
			movieViews = append(movieViews, v)
		case "tvshows":
			seriesViews = append(seriesViews, v)
		}
	}
	if len(movieViews) != 2 || len(seriesViews) != 2 {
		t.Fatalf("views: %v", views)
	}
	idsIn := func(view viewItem) map[string]string {
		t.Helper()
		out := map[string]string{}
		for _, it := range sf.children(t, view.Id) {
			out[it.Name] = it.Id
		}
		return out
	}
	// movieViews[0]: the popular catalog (3 titles); [1]: the rated one (2).
	if len(idsIn(movieViews[0])) < len(idsIn(movieViews[1])) {
		movieViews[0], movieViews[1] = movieViews[1], movieViews[0]
	}
	popular, rated := idsIn(movieViews[0]), idsIn(movieViews[1])
	if len(popular) != 3 || len(rated) != 2 {
		t.Fatalf("movie libraries: %v and %v", popular, rated)
	}
	for _, name := range []string{"The Matrix", "The Matrix Reloaded"} {
		if popular[name] == "" || popular[name] != rated[name] {
			t.Errorf("%s: %q in one library, %q in the other; want the same item", name, popular[name], rated[name])
		}
	}
	got := map[string]int{}
	rows, err := testPool.Query(t.Context(), `SELECT canonical_key, count(*) FROM items WHERE canonical_key IS NOT NULL GROUP BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var k string
		var n int
		_ = rows.Scan(&k, &n)
		got[k] = n
	}
	rows.Close()
	if got["movie:tt0133093"] != 1 || got["movie:tt0234215"] != 1 || got["series:tt0944947"] != 1 || got["movie:tmdb:604"] != 0 {
		t.Errorf("items per title: %v", got)
	}
	// The series: one item in both libraries, its episodes written once.
	thrones := idsIn(seriesViews[0])["Game of Thrones"]
	if thrones == "" || thrones != idsIn(seriesViews[1])["Game of Thrones"] {
		t.Errorf("Game of Thrones: not the same item in both series libraries")
	}
	var dupes int
	if err := testPool.QueryRow(t.Context(), `SELECT count(*) FROM (
	      SELECT 1 FROM items e WHERE e.type = 'Episode' GROUP BY e.parent_index_number, e.index_number HAVING count(*) > 1) d`).Scan(&dupes); err != nil {
		t.Fatal(err)
	}
	if dupes != 0 {
		t.Errorf("%d episodes written twice", dupes)
	}

	// Search finds each title once.
	var found struct{ Items []viewItem }
	getJSON(t, sf.h, "/Items?searchTerm=matrix&recursive=true&includeItemTypes=Movie", &found)
	var names []string
	for _, it := range found.Items {
		names = append(names, it.Name)
	}
	slices.Sort(names)
	if strings.Join(names, ",") != "The Matrix,The Matrix Reloaded,The Matrix Resurrections" {
		t.Errorf("search: %q", names)
	}

	// A user who may browse only the rated library sees its titles there,
	// though they're at home in the other one.
	folder, err := dto.ParseID(movieViews[1].Id)
	if err != nil {
		t.Fatal(err)
	}
	res, err := pg.QueryItems(t.Context(), testPool, pg.ItemQuery{
		Recursive: true, IncludeTypes: []string{"Movie"}, Access: pg.Access{Folders: []uuid.UUID{folder.UUID()}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var visible []string
	for _, it := range res.Items {
		visible = append(visible, it.Name)
	}
	slices.Sort(visible)
	if strings.Join(visible, ",") != "The Matrix,The Matrix Reloaded" {
		t.Errorf("restricted user sees %q", visible)
	}

	// Played through one library: the same item in the other.
	matrix := popular["The Matrix"]
	if rec := call(t, sf.h, "POST", "/UserPlayedItems/"+matrix, `MediaBrowser Token="`+captureToken+`"`, ""); rec.Code != 200 {
		t.Fatalf("mark played: %d %s", rec.Code, rec.Body)
	}

	// The popular catalog drops The Matrix: it leaves that library only,
	// as the same item, still played.
	sf.f.mu.Lock()
	sf.f.movies = sf.f.movies[1:]
	sf.f.mu.Unlock()
	sf.syncAll(t)
	if _, ok := idsIn(movieViews[0])["The Matrix"]; ok {
		t.Error("The Matrix is still in the catalog that dropped it")
	}
	if id := idsIn(movieViews[1])["The Matrix"]; id != matrix {
		t.Errorf("The Matrix in the other library: %q, want %q", id, matrix)
	}
	var item struct{ UserData struct{ Played bool } }
	getJSON(t, sf.h, "/Items/"+matrix, &item)
	if !item.UserData.Played {
		t.Error("The Matrix lost its played state")
	}
}
