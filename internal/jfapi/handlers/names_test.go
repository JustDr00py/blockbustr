package handlers

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// Expected differences from Jellyfin, not bugs:
//   - Studio Thumb artwork: Jellyfin fetched studio logos from its artwork
//     provider; blockbustr has no studio image source (only people have
//     artwork).
var nameIgnores = []string{"$.Items[*].ImageTags.Thumb", "$.Items[*].ImageBlurHashes.Thumb"}

// moviesFolder is the recon Movies library.
const moviesFolder = "f137a2dd21bbc1b99aa5c0f6bf02a805"

func TestNamesContract(t *testing.T) {
	h, d := newIntegrationServer(t)
	seedRecon(t, d)
	seedCaptureToken(t, d)
	for _, ep := range []string{"GET /Studios", "GET /Persons", "GET /Artists", "GET /Items/Filters", "GET /Items/Filters2"} {
		checkContractOn(t, h, capturesFor(t, ep), nameIgnores...)
	}
}

// The same names and filter values as Jellyfin (Tags aren't stored yet:
// DESIGN §3.6 "Not yet").
func TestNamesSameValuesAsJellyfin(t *testing.T) {
	h, d := newIntegrationServer(t)
	seedRecon(t, d)
	seedCaptureToken(t, d)
	for _, c := range capturesFor(t, "GET /Studios") {
		var want, got struct {
			Items []struct {
				Name                                string
				ChildCount, MovieCount, SeriesCount int
			}
			TotalRecordCount int
		}
		_ = json.Unmarshal(c.Response.Body, &want)
		_ = json.Unmarshal(replay(t, h, c).Body.Bytes(), &got)
		if !reflect.DeepEqual(want, got) {
			t.Errorf("%s:\n  jellyfin:   %+v\n  blockbustr: %+v", c.Name, want, got)
		}
	}
	for _, ep := range []string{"GET /Items/Filters", "GET /Items/Filters2"} {
		for _, c := range capturesFor(t, ep) {
			var want, got map[string]any
			_ = json.Unmarshal(c.Response.Body, &want)
			_ = json.Unmarshal(replay(t, h, c).Body.Bytes(), &got)
			delete(want, "Tags")
			delete(got, "Tags")
			if !reflect.DeepEqual(want, got) {
				t.Errorf("%s:\n  jellyfin:   %v\n  blockbustr: %v", c.Name, want, got)
			}
		}
	}
}

type nameList struct {
	Items []struct {
		Name, Type              string
		ChildCount              int
		MovieCount, SeriesCount int
		ImageTags               map[string]string
		UserData                struct{ Key string }
		PrimaryImageAspectRatio *float64
	}
	TotalRecordCount, StartIndex int
}

func (l nameList) names() string {
	var s []string
	for _, it := range l.Items {
		s = append(s, it.Name)
	}
	return strings.Join(s, ", ")
}

func TestNamesScopeAndPaging(t *testing.T) {
	h, d := newIntegrationServer(t)
	seedRecon(t, d)
	seedCaptureToken(t, d)

	var all, movies, shows nameList
	getJSON(t, h, "/Genres", &all)
	getJSON(t, h, "/Genres?parentId="+moviesFolder, &movies)
	getJSON(t, h, "/Genres?includeItemTypes=Series", &shows)
	if len(all.Items) == 0 || len(all.Items) != all.TotalRecordCount || len(movies.Items) >= len(all.Items) || len(shows.Items) == 0 {
		t.Fatalf("all %d (total %d), movies %d, shows %d", len(all.Items), all.TotalRecordCount, len(movies.Items), len(shows.Items))
	}
	for _, g := range shows.Items {
		if g.Type != "Genre" || g.SeriesCount == 0 || g.MovieCount != 0 || g.UserData.Key != "Genre-"+g.Name {
			t.Errorf("series genre: %+v", g)
		}
	}

	var page nameList
	getJSON(t, h, "/Genres?startIndex=1&limit=2&sortOrder=Descending", &page)
	if len(page.Items) != 2 || page.StartIndex != 1 || page.TotalRecordCount != len(all.Items) ||
		page.Items[0].Name != all.Items[len(all.Items)-2].Name {
		t.Errorf("descending page 2: %s (total %d), all: %s", page.names(), page.TotalRecordCount, all.names())
	}
	getJSON(t, h, fmt.Sprintf("/Genres?startIndex=%d&limit=5", len(all.Items)+3), &page)
	if len(page.Items) != 0 || page.TotalRecordCount != len(all.Items) {
		t.Errorf("past the end: %+v", page)
	}
	getJSON(t, h, "/Genres?nameStartsWith=a", &page)
	if len(page.Items) == 0 {
		t.Errorf("nameStartsWith=a: nothing")
	}
	for _, g := range page.Items {
		if !strings.HasPrefix(strings.ToLower(g.Name), "a") {
			t.Errorf("nameStartsWith=a: %s", page.names())
		}
	}
	for _, q := range []string{"isFavorite=true", "parentId=nope"} {
		getJSON(t, h, "/Studios?"+q, &page)
		if len(page.Items) != 0 {
			t.Errorf("%s: %s", q, page.names())
		}
	}
}

func TestPersons(t *testing.T) {
	h, d := newIntegrationServer(t)
	seedRecon(t, d)
	seedCaptureToken(t, d)
	var res nameList
	getJSON(t, h, "/Persons?appearsInItemId="+fockers+"&personTypes=Director&fields=PrimaryImageAspectRatio", &res)
	if len(res.Items) == 0 {
		t.Fatal("Little Fockers has no director")
	}
	for _, p := range res.Items {
		if p.Type != "Person" || p.MovieCount != 1 {
			t.Errorf("director: %+v", p)
		}
		if p.ImageTags["Primary"] != "" && p.PrimaryImageAspectRatio == nil {
			t.Errorf("person with a photo has no aspect ratio: %+v", p)
		}
	}
	name := res.Items[0].Name
	// Case-insensitive substring search.
	getJSON(t, h, "/Persons?searchTerm="+strings.ReplaceAll(strings.ToUpper(name[1:len(name)-1]), " ", "%20"), &res)
	if !strings.Contains(res.names(), name) {
		t.Errorf("search for %q: %s", name, res.names())
	}
	getJSON(t, h, "/Persons?appearsInItemId="+fockers+"&excludePersonTypes=Actor,GuestStar,Director,Writer,Producer,Composer", &res)
	if len(res.Items) != 0 {
		t.Errorf("every kind excluded: %s", res.names())
	}
}

func TestSearchHints(t *testing.T) {
	h, d := newIntegrationServer(t)
	seedRecon(t, d)
	seedCaptureToken(t, d)
	type hints struct {
		SearchHints []struct {
			Name, Type, Id, ItemId string
			Series                 string
			Artists                []string
		}
		TotalRecordCount int
	}
	var res hints
	getJSON(t, h, "/Search/Hints?searchTerm=luca", &res)
	if len(res.SearchHints) == 0 || res.SearchHints[0].Name != "Luca" || res.SearchHints[0].Type != "Movie" ||
		res.SearchHints[0].Id != res.SearchHints[0].ItemId || res.SearchHints[0].Artists == nil {
		t.Fatalf("luca: %+v", res)
	}
	getJSON(t, h, "/Search/Hints?searchTerm=ghost&includeItemTypes=Episode", &res)
	for _, s := range res.SearchHints {
		if s.Type != "Episode" || s.Series == "" {
			t.Errorf("episodes only, with their series: %+v", s)
		}
	}
	// A person by name, and only people when asked for.
	var people nameList
	getJSON(t, h, "/Persons?appearsInItemId="+fockers, &people)
	if len(people.Items) == 0 {
		t.Fatal("no people on Little Fockers")
	}
	who := people.Items[0].Name
	getJSON(t, h, "/Search/Hints?includeItemTypes=Person&searchTerm="+strings.ReplaceAll(who, " ", "%20"), &res)
	if len(res.SearchHints) == 0 || res.TotalRecordCount != len(res.SearchHints) {
		t.Fatalf("%s: %+v", who, res)
	}
	for _, s := range res.SearchHints {
		if s.Type != "Person" {
			t.Errorf("people only: %+v", s)
		}
	}
	getJSON(t, h, "/Search/Hints?searchTerm=a&limit=3&startIndex=2", &res)
	if len(res.SearchHints) != 3 || res.TotalRecordCount < 5 {
		t.Errorf("paged: %d hints of %d", len(res.SearchHints), res.TotalRecordCount)
	}
	getJSON(t, h, "/Search/Hints?searchTerm=", &res)
	if len(res.SearchHints) != 0 || res.SearchHints == nil {
		t.Errorf("empty term: %+v", res)
	}
}
