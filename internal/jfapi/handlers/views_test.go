package handlers

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/sysadmin/blockbustr/internal/store/pg/db"
)

// Differences from the captures that are expected, not bugs: Jellyfin
// generates a collage as the Movies library image; blockbustr has no
// library artwork yet, so ImageTags/ImageBlurHashes are empty and there's no
// PrimaryImageAspectRatio (Jellyfin sent Shows the same way).
var viewIgnores = []string{"$.Items[*].ImageTags", "$.Items[*].ImageBlurHashes", "$.Items[*].PrimaryImageAspectRatio"}

type viewsEnv struct {
	h             http.Handler
	d             Deps
	movies, shows uuid.UUID
	adminAuth     string
}

func newViewsEnv(t *testing.T) viewsEnv {
	t.Helper()
	h, d := newIntegrationServer(t)
	seedCaptureToken(t, d)
	ctx := t.Context()
	lib := func(name, kind, path string) (db.Library, uuid.UUID) {
		l, err := d.Queries.UpsertLibrary(ctx, db.UpsertLibraryParams{Name: name, Kind: kind, Paths: []string{path}})
		if err != nil {
			t.Fatal(err)
		}
		f, err := d.Queries.EnsureCollectionFolder(ctx, db.EnsureCollectionFolderParams{LibraryID: l.ID, Name: name, SortName: strings.ToLower(name)})
		if err != nil {
			t.Fatal(err)
		}
		return l, f
	}
	mLib, movies := lib("Movies", "movies", "/media/Movies")
	sLib, shows := lib("Shows", "tvshows", "/media/Series")
	lib("Old Stuff", "movies", "/media/old") // removed from config: disabled below
	if _, err := d.Queries.DisableLibrariesExcept(ctx, []uuid.UUID{mLib.ID, sLib.ID}); err != nil {
		t.Fatal(err)
	}
	child := func(libID, parent uuid.UUID, typ, name string) uuid.UUID {
		it, err := d.Queries.CreateItem(ctx, db.CreateItemParams{LibraryID: libID, ParentID: &parent, TopParentID: &parent, Type: typ, Name: name, SortName: strings.ToLower(name), SourceKind: "virtual"})
		if err != nil {
			t.Fatal(err)
		}
		return it.ID
	}
	child(mLib.ID, movies, "Movie", "Luca")
	child(mLib.ID, movies, "Movie", "Little Fockers")
	child(mLib.ID, movies, "Movie", "Mortal Kombat II")
	gone := child(mLib.ID, movies, "Movie", "Deleted Film")
	child(sLib.ID, shows, "Series", "MF GHOST")
	execSQL(t, `UPDATE items SET missing_since = now(), is_missing = true WHERE id = $1`, gone)
	return viewsEnv{h: h, d: d, movies: movies, shows: shows, adminAuth: `MediaBrowser Token="` + captureToken + `"`}
}

func TestUserViewsContract(t *testing.T) {
	e := newViewsEnv(t)
	for _, b := range checkContractOn(t, e.h, capturesFor(t, "GET /UserViews"), viewIgnores...) {
		res := b.(map[string]any)
		items := res["Items"].([]any)
		if res["TotalRecordCount"] != float64(2) || len(items) != 2 {
			t.Fatalf("views: %v (disabled library must not be listed)", res)
		}
		m, s := items[0].(map[string]any), items[1].(map[string]any)
		if m["Name"] != "Movies" || m["CollectionType"] != "movies" || m["ChildCount"] != float64(3) ||
			s["Name"] != "Shows" || s["CollectionType"] != "tvshows" || s["ChildCount"] != float64(1) {
			t.Errorf("views: %v / %v (missing items aren't counted)", m, s)
		}
		ud := m["UserData"].(map[string]any)
		if ud["Key"] != e.movies.String() || ud["ItemId"] != strings.ReplaceAll(e.movies.String(), "-", "") {
			t.Errorf("UserData key/id: %v", ud)
		}
		if m["ParentId"] != rootFolderID(e.d.ServerID).String() || m["DisplayPreferencesId"] != m["Id"] || m["Path"] != "/media/Movies" {
			t.Errorf("folder fields: %v", m)
		}
	}
}

func TestViewsGoldenShapes(t *testing.T) {
	e := newViewsEnv(t)
	for path, name := range map[string]string{
		"/Users/bbbb0000000000000000000000000001/Views": "users-views",
		"/Library/MediaFolders":                         "mediafolders",
		"/Library/VirtualFolders":                       "virtualfolders",
		"/UserViews/GroupingOptions":                    "groupingoptions",
	} {
		rec := call(t, e.h, "GET", path, e.adminAuth, "")
		if rec.Code != 200 {
			t.Fatalf("%s = %d", path, rec.Code)
		}
		ignore := map[string]bool{}
		for _, p := range viewIgnores {
			ignore[p] = true
		}
		if err := sameShapeIgnoring("$", golden(t, name), decode(t, rec), ignore); err != nil {
			t.Errorf("%s vs Jellyfin: %v", path, err)
		}
	}
	var vf []struct {
		Name, ItemId, CollectionType string
		Locations                    []string
		LibraryOptions               struct {
			PathInfos   []struct{ Path string }
			TypeOptions []struct{ Type string }
		}
	}
	_ = json.Unmarshal(call(t, e.h, "GET", "/Library/VirtualFolders", e.adminAuth, "").Body.Bytes(), &vf)
	if len(vf) != 2 || vf[1].Name != "Shows" || vf[1].Locations[0] != "/media/Series" || vf[1].LibraryOptions.PathInfos[0].Path != "/media/Series" ||
		len(vf[1].LibraryOptions.TypeOptions) != 3 || vf[0].LibraryOptions.TypeOptions[0].Type != "Movie" {
		t.Errorf("virtual folders: %+v", vf)
	}
}

func TestViewsAccess(t *testing.T) {
	e := newViewsEnv(t)
	for _, p := range []string{"/UserViews", "/Library/MediaFolders", "/Library/VirtualFolders", "/UserViews/GroupingOptions"} {
		if rec := call(t, e.h, "GET", p, "", ""); rec.Code != 401 {
			t.Errorf("%s anonymous = %d", p, rec.Code)
		}
	}
	if _, err := e.d.Queries.CreateUser(t.Context(), db.CreateUserParams{Name: "kid"}); err != nil {
		t.Fatal(err)
	}
	rec := call(t, e.h, "POST", "/Users/AuthenticateByName", `MediaBrowser Client="C", DeviceId="kid-1", Device="P", Version="1"`, `{"Username":"kid","Pw":""}`)
	var res struct{ AccessToken string }
	_ = json.Unmarshal(rec.Body.Bytes(), &res)
	kid := `MediaBrowser Token="` + res.AccessToken + `"`
	if rec := call(t, e.h, "GET", "/UserViews", kid, ""); rec.Code != 200 {
		t.Errorf("user views for non-admin = %d", rec.Code)
	}
	for _, p := range []string{"/Library/MediaFolders", "/Library/VirtualFolders"} {
		if rec := call(t, e.h, "GET", p, kid, ""); rec.Code != 403 {
			t.Errorf("%s non-admin = %d", p, rec.Code)
		}
	}
}

func TestViewsCarryUserData(t *testing.T) {
	e := newViewsEnv(t)
	u, _ := e.d.Queries.GetUserByName(t.Context(), "user1")
	execSQL(t, `INSERT INTO user_data (user_id, item_id, is_favorite) VALUES ($1, $2, true)`, u.ID, e.movies)
	var res struct {
		Items []struct {
			Name     string
			UserData struct{ IsFavorite bool }
		}
	}
	_ = json.Unmarshal(call(t, e.h, "GET", "/UserViews", e.adminAuth, "").Body.Bytes(), &res)
	if !res.Items[0].UserData.IsFavorite || res.Items[1].UserData.IsFavorite {
		t.Errorf("favourite flag: %+v", res.Items)
	}
}
