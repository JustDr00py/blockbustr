package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/sysadmin/blockbustr/internal/jfapi"
	"github.com/sysadmin/blockbustr/internal/store/pg"
	"github.com/sysadmin/blockbustr/internal/store/pg/db"
	"github.com/sysadmin/blockbustr/internal/testutil"
)

// Expected differences from Jellyfin, not bugs:
//   - People[*].ImageBlurHashes: person blurhashes aren't computed (P1.17b).
//   - MediaSources/MediaStreams: Jellyfin adds them to episodes in big
//     field sets; they come with P1.20.
//   - UserData.LastPlayedDate/PlayedPercentage: playback state at the moment
//     of each capture (Little Fockers was 20% watched in one, 45% in a
//     later one); the seed holds one state per item. TestItemsUserData
//     checks the rules instead.
var itemIgnores = []string{
	"$.Items[*].People[*].ImageBlurHashes",
	"$.Items[*].MediaSources", "$.Items[*].MediaStreams",
	"$.Items[*].UserData.LastPlayedDate", "$.Items[*].UserData.PlayedPercentage",
}

// fockers and mfGhost are recon ids (stable across captures).
const (
	fockers = "d48b8adc50a77956fcee8210661176fb"
	mfGhost = "73ecf1e6db425eb1214bb7c9cb74dfe6"
)

func getJSON(t *testing.T, h http.Handler, path string, out any) {
	t.Helper()
	rec := call(t, h, "GET", path, `MediaBrowser Token="`+captureToken+`"`, "")
	if rec.Code != 200 {
		t.Fatalf("%s = %d %s", path, rec.Code, rec.Body)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
		t.Fatal(err)
	}
}

type userDataView struct {
	PlayedPercentage  *float64
	UnplayedItemCount *int
	Played            bool
	LastPlayedDate    *string
}

func TestItemsUserData(t *testing.T) {
	h, d := newIntegrationServer(t)
	seedRecon(t, d)
	seedCaptureToken(t, d)
	execSQL(t, `UPDATE user_data SET playback_position_ticks = 26364670000, last_played_at = now() WHERE item_id = $1`, fockers)
	execSQL(t, `UPDATE user_data SET played = false WHERE item_id IN (SELECT id FROM items WHERE type = 'Episode')`)
	execSQL(t, `UPDATE user_data SET played = true WHERE item_id = (SELECT id FROM items WHERE type = 'Episode' ORDER BY index_number LIMIT 1)`)
	var res struct {
		Items []struct {
			Name     string
			UserData userDataView
		}
	}
	getJSON(t, h, "/Items?ids="+fockers+","+mfGhost, &res)
	got := map[string]userDataView{}
	for _, it := range res.Items {
		got[it.Name] = it.UserData
	}
	if f := got["Little Fockers"]; f.PlayedPercentage == nil || *f.PlayedPercentage < 44.99 || *f.PlayedPercentage > 45 || f.LastPlayedDate == nil {
		t.Errorf("resumable movie: %+v", f)
	}
	if s := got["MF GHOST"]; s.UnplayedItemCount == nil || *s.UnplayedItemCount != 4 || s.Played || s.PlayedPercentage == nil || *s.PlayedPercentage != 20 {
		t.Errorf("series with 1 of 5 played: %+v", s)
	}
}

func TestItemsContract(t *testing.T) {
	h, d := newIntegrationServer(t)
	seedRecon(t, d)
	seedCaptureToken(t, d)
	var caps []capture
	for _, c := range capturesFor(t, "GET /Items") {
		if !isRandom(c) { // Items[0] differs run to run; TestItemsSameResultsAsJellyfin compares the set
			caps = append(caps, c)
		}
	}
	checkContractOn(t, h, caps, itemIgnores...)
}

func isRandom(c capture) bool {
	return strings.Contains(strings.ToLower(strings.Join(c.Request.Query["sortBy"], ",")), "random")
}

func TestItemsSameResultsAsJellyfin(t *testing.T) {
	h, d := newIntegrationServer(t)
	seedRecon(t, d)
	seedCaptureToken(t, d)
	for _, c := range capturesFor(t, "GET /Items") {
		if c.Response.Status != 200 {
			continue
		}
		var want, got struct {
			Items []struct{ Id, Name string }
		}
		_ = json.Unmarshal(c.Response.Body, &want)
		_ = json.Unmarshal(replay(t, h, c).Body.Bytes(), &got)
		ids := func(v []struct{ Id, Name string }) string {
			var s []string
			for _, it := range v {
				s = append(s, it.Name)
			}
			if isRandom(c) {
				sort.Strings(s)
			}
			return strings.Join(s, " | ")
		}
		if ids(want.Items) != ids(got.Items) {
			t.Errorf("%s ?%s\n  jellyfin:   %s\n  blockbustr: %s", c.Name, url.Values(c.Request.Query).Encode(), ids(want.Items), ids(got.Items))
		}
	}
}

// fakeRemote stands in for the P3 search providers.
type fakeRemote struct {
	items []db.Item
	err   error
	calls []string
}

func (f *fakeRemote) EnsureEpisodes(context.Context, db.Item) error { return nil }

func (f *fakeRemote) SearchItems(_ context.Context, term string, types []string, limit int) ([]db.Item, error) {
	f.calls = append(f.calls, fmt.Sprintf("%s %v %d", term, types, limit))
	return f.items, f.err
}

func TestItemsRemoteSearch(t *testing.T) {
	_, d := newIntegrationServer(t)
	seedRecon(t, d)
	seedCaptureToken(t, d)
	luca, err := d.Queries.GetItemsByIDs(t.Context(), []uuid.UUID{uuid.MustParse("84088e11eb6351255c08507602d79a0f")})
	if err != nil || len(luca) != 1 {
		t.Fatal(luca, err)
	}
	matrix := db.Item{ID: uuid.New(), Type: "Movie", Name: "Luca's Matrix", SortName: "matrix"}
	remote := &fakeRemote{items: []db.Item{luca[0], matrix}} // Luca is also a library match: listed once
	d.RemoteSearch = remote
	rt := jfapi.NewRouter(testutil.Discard(), jfapi.Options{})
	Register(rt, d)

	var res struct {
		Items            []struct{ Name string }
		TotalRecordCount int
	}
	getJSON(t, rt, "/Items?searchTerm=luca&recursive=true&includeItemTypes=Movie,Series,Episode&limit=10", &res)
	if len(res.Items) != 2 || res.Items[0].Name != "Luca" || res.Items[1].Name != "Luca's Matrix" || res.TotalRecordCount != 2 {
		t.Errorf("library first, then remote, deduplicated: %+v", res)
	}
	if len(remote.calls) != 1 || remote.calls[0] != "luca [Movie Series] 9" {
		t.Errorf("remote asked for %q", remote.calls)
	}

	remote.calls = nil
	for _, q := range []string{
		"searchTerm=luca&recursive=true&includeItemTypes=Episode",       // remote is movies/series only
		"searchTerm=luca&recursive=true&includeItemTypes=LiveTvProgram", // Jellyfin Android's TV search
		"searchTerm=luca&recursive=true&startIndex=20",                  // later pages: library only
		"recursive=true&includeItemTypes=Movie",                         // not a search
	} {
		getJSON(t, rt, "/Items?"+q, &res)
	}
	if len(remote.calls) != 0 {
		t.Errorf("remote searched for %q", remote.calls)
	}

	remote.err = errors.New("addon down")
	getJSON(t, rt, "/Items?searchTerm=luca&recursive=true", &res)
	if len(res.Items) != 1 || res.Items[0].Name != "Luca" {
		t.Errorf("a failing source must not fail the search: %+v", res)
	}
}

func TestRemoteTypes(t *testing.T) {
	for _, c := range []struct {
		q    pg.ItemQuery
		want string
	}{
		{pg.ItemQuery{}, "[Movie Series]"},
		{pg.ItemQuery{IncludeTypes: []string{"movie"}}, "[Movie]"},
		{pg.ItemQuery{ExcludeTypes: []string{"Movie", "Episode", "TvChannel"}}, "[Series]"},
		{pg.ItemQuery{MediaTypes: []string{"Video"}}, "[Movie]"},
		{pg.ItemQuery{MediaTypes: []string{"Audio"}}, "[]"},
		{pg.ItemQuery{IncludeTypes: []string{"Movie", "Series", "MusicArtist"}}, "[Movie Series]"},
	} {
		if got := fmt.Sprint(remoteTypes(c.q)); got != c.want {
			t.Errorf("%+v: %s, want %s", c.q, got, c.want)
		}
	}
}

func TestItemsAccessAndBadInput(t *testing.T) {
	h, d := newIntegrationServer(t)
	seedRecon(t, d)
	seedCaptureToken(t, d)
	if rec := call(t, h, "GET", "/Items", "", ""); rec.Code != 401 {
		t.Errorf("anonymous = %d", rec.Code)
	}
	if _, err := d.Queries.CreateUser(t.Context(), db.CreateUserParams{Name: "kid"}); err != nil {
		t.Fatal(err)
	}
	rec := call(t, h, "POST", "/Users/AuthenticateByName", `MediaBrowser Client="C", DeviceId="kid-1", Device="P", Version="1"`, `{"Username":"kid","Pw":""}`)
	var login struct{ AccessToken string }
	_ = json.Unmarshal(rec.Body.Bytes(), &login)
	kid := `MediaBrowser Token="` + login.AccessToken + `"`
	if rec := call(t, h, "GET", "/Items?userId="+strings.ReplaceAll(captureUserID, "-", ""), kid, ""); rec.Code != 403 {
		t.Errorf("non-admin reading another user's items = %d", rec.Code)
	}
	if rec := call(t, h, "GET", "/Users/"+strings.ReplaceAll(captureUserID, "-", "")+"/Items?recursive=true&includeItemTypes=Movie", `MediaBrowser Token="`+captureToken+`"`, ""); rec.Code != 200 {
		t.Errorf("legacy route = %d", rec.Code)
	}
	var res struct {
		Items            []any
		TotalRecordCount int
	}
	for _, q := range []string{"parentId=not-a-guid", "isMissing=true&recursive=true", "parentId=00000000000000000000000000000001"} {
		getJSON(t, h, "/Items?"+q, &res)
		if len(res.Items) != 0 || res.TotalRecordCount != 0 {
			t.Errorf("%s: %+v", q, res)
		}
	}
	getJSON(t, h, "/Items?parentId="+rootFolderID(d.ServerID).String(), &res)
	if len(res.Items) != 2 {
		t.Errorf("the root folder's children are the libraries: %d", len(res.Items))
	}
}

// A .strm item isn't probed until first play: its media source has no
// streams yet (found live: the width scan failed on NULL).
func TestItemsUnprobedStrm(t *testing.T) {
	h, d := newIntegrationServer(t)
	seedRecon(t, d)
	seedCaptureToken(t, d)
	execSQL(t, `DELETE FROM media_streams WHERE media_source_id IN (SELECT id FROM media_sources WHERE item_id = $1)`, fockers)
	var res struct {
		Items []struct {
			Name          string
			Width, Height *int
			IsHD          *bool
			Container     string
		}
	}
	getJSON(t, h, "/Items?ids="+fockers+"&fields=Width,Height,IsHD", &res)
	if len(res.Items) != 1 || res.Items[0].Width != nil || res.Items[0].IsHD != nil || res.Items[0].Container == "" {
		t.Errorf("unprobed item: %+v", res.Items)
	}
}
