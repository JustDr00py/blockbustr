package handlers

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

// userClient logs name in on its own device and returns its auth header.
func userClient(t *testing.T, sf *syncFixture, name, pw string) string {
	t.Helper()
	dev := `MediaBrowser Client="C", DeviceId="` + name + `-dev", Device="P", Version="1"`
	rec := call(t, sf.h, "POST", "/Users/AuthenticateByName", dev, `{"Username":"`+name+`","Pw":"`+pw+`"}`)
	if rec.Code != 200 {
		t.Fatalf("login %s = %d %s", name, rec.Code, rec.Body)
	}
	var res struct{ AccessToken string }
	_ = json.Unmarshal(rec.Body.Bytes(), &res)
	return dev + `, Token="` + res.AccessToken + `"`
}

// itemNamesAs lists a query's item names, sorted, as authz sees them.
func itemNamesAs(t *testing.T, sf *syncFixture, authz, path string) []string {
	t.Helper()
	rec := call(t, sf.h, "GET", path, authz, "")
	if rec.Code != 200 {
		t.Fatalf("%s = %d", path, rec.Code)
	}
	var res struct{ Items []struct{ Name string } }
	_ = json.Unmarshal(rec.Body.Bytes(), &res)
	var out []string
	for _, it := range res.Items {
		out = append(out, it.Name)
	}
	slices.Sort(out)
	return out
}

func TestUserPolicy(t *testing.T) {
	sf := newSyncFixture(t)
	sf.syncAll(t)
	admin := `MediaBrowser Token="` + captureToken + `"`
	views := sf.views(t)
	movies, shows := views["Cinemeta Popular"].Id, views["Cinemeta Popular (2)"].Id
	// Ratings as TMDB would set them: The Matrix R, Reloaded PG, Resurrections
	// unrated; Game of Thrones TV-MA (its episodes inherit it).
	for name, rating := range map[string]any{"The Matrix": "R", "The Matrix Reloaded": "PG", "The Matrix Resurrections": nil, "Game of Thrones": "TV-MA"} {
		if _, err := testPool.Exec(t.Context(), `UPDATE items SET official_rating = $2 WHERE name = $1`, name, rating); err != nil {
			t.Fatal(err)
		}
	}
	idOf := func(name string) string {
		for _, it := range sf.children(t, movies) {
			if it.Name == name {
				return it.Id
			}
		}
		t.Fatalf("no %s", name)
		return ""
	}
	matrix, reloaded := idOf("The Matrix"), idOf("The Matrix Reloaded")

	// An admin creates a user, who signs in.
	rec := call(t, sf.h, "POST", "/Users/New", admin, `{"Name":"kid","Password":"pw1"}`)
	if rec.Code != 200 {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	var created struct{ Id string }
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	if rec := call(t, sf.h, "POST", "/Users/New", admin, `{"Name":"KID"}`); rec.Code != 400 {
		t.Errorf("duplicate name (case-insensitive): %d", rec.Code)
	}
	kid := userClient(t, sf, "kid", "pw1")
	if rec := call(t, sf.h, "POST", "/Users/New", kid, `{"Name":"x"}`); rec.Code != 403 {
		t.Errorf("non-admin creating a user: %d", rec.Code)
	}
	if got := itemNamesAs(t, sf, kid, "/UserViews"); len(got) != 2 {
		t.Fatalf("unrestricted views: %v", got)
	}

	// Restrict: the movie library only, PG at most, unrated movies hidden.
	// It applies to the kid's current session at once.
	policy := `{"EnableAllFolders":false,"EnabledFolders":["` + movies + `"],"MaxParentalRating":10,"BlockUnratedItems":["Movie"],"EnableMediaPlayback":true}`
	if rec := call(t, sf.h, "POST", "/Users/"+created.Id+"/Policy", admin, policy); rec.Code != 204 {
		t.Fatalf("policy: %d %s", rec.Code, rec.Body)
	}
	if got := itemNamesAs(t, sf, kid, "/UserViews"); strings.Join(got, ",") != "Cinemeta Popular" {
		t.Errorf("restricted views: %v", got)
	}
	if got := itemNamesAs(t, sf, kid, "/Items?ParentId="+movies); strings.Join(got, ",") != "The Matrix Reloaded" {
		t.Errorf("restricted movies: %v (R and unrated hidden)", got)
	}
	if got := itemNamesAs(t, sf, kid, "/Items?recursive=true&searchTerm=matrix"); strings.Join(got, ",") != "The Matrix Reloaded" {
		t.Errorf("restricted search: %v", got)
	}
	for _, path := range []string{"/Items/" + matrix, "/Items/" + shows} {
		if rec := call(t, sf.h, "GET", path, kid, ""); rec.Code != 404 {
			t.Errorf("%s for the kid: %d, want 404", path, rec.Code)
		}
	}
	if rec := call(t, sf.h, "POST", "/Items/"+matrix+"/PlaybackInfo", kid, `{}`); rec.Code != 404 {
		t.Errorf("PlaybackInfo of an R movie: %d", rec.Code)
	}
	// The admin isn't affected, and the stored policy reads back.
	if got := itemNamesAs(t, sf, admin, "/Items?ParentId="+movies); len(got) != 3 {
		t.Errorf("admin's movies: %v", got)
	}
	var u struct {
		Policy struct {
			EnableAllFolders  bool
			EnabledFolders    []string
			MaxParentalRating int
			IsAdministrator   bool
		}
	}
	getJSON(t, sf.h, "/Users/"+created.Id, &u)
	if u.Policy.EnableAllFolders || len(u.Policy.EnabledFolders) != 1 || u.Policy.MaxParentalRating != 10 || u.Policy.IsAdministrator {
		t.Errorf("policy read back: %+v", u.Policy)
	}

	// Playback off: PlaybackInfo is refused.
	if rec := call(t, sf.h, "POST", "/Users/"+created.Id+"/Policy", admin, `{"EnableMediaPlayback":false}`); rec.Code != 204 {
		t.Fatal(rec.Code)
	}
	if rec := call(t, sf.h, "POST", "/Items/"+reloaded+"/PlaybackInfo", kid, `{}`); rec.Code != 403 {
		t.Errorf("PlaybackInfo with playback off: %d", rec.Code)
	}

	// Episodes carry their series' rating: with every library but TV-14 at
	// most, Game of Thrones (TV-MA) and its episodes are gone.
	if rec := call(t, sf.h, "POST", "/Users/"+created.Id+"/Policy", admin, `{"MaxParentalRating":14}`); rec.Code != 204 {
		t.Fatal(rec.Code)
	}
	if got := itemNamesAs(t, sf, kid, "/Items?recursive=true&includeItemTypes=Series,Episode&limit=10"); len(got) != 0 {
		t.Errorf("TV-MA series and episodes visible under TV-14: %v", got)
	}
	if got := itemNamesAs(t, sf, admin, "/Items?recursive=true&includeItemTypes=Episode&limit=10"); len(got) == 0 {
		t.Error("the admin lost the episodes too")
	}

	// Passwords: the user needs the current one; an admin resets it.
	if rec := call(t, sf.h, "POST", "/Users/"+created.Id+"/Password", kid, `{"CurrentPw":"wrong","NewPw":"pw2"}`); rec.Code != 403 {
		t.Errorf("wrong current password: %d", rec.Code)
	}
	if rec := call(t, sf.h, "POST", "/Users/"+created.Id+"/Password", kid, `{"CurrentPw":"pw1","NewPw":"pw2"}`); rec.Code != 204 {
		t.Errorf("change password: %d", rec.Code)
	}
	kid = userClient(t, sf, "kid", "pw2") // a new login replaces the device's session
	if rec := call(t, sf.h, "POST", "/Users/"+created.Id+"/Password", kid, `{"ResetPassword":true}`); rec.Code != 403 {
		t.Errorf("non-admin reset: %d", rec.Code)
	}
	if rec := call(t, sf.h, "POST", "/Users/"+created.Id+"/Password", admin, `{"ResetPassword":true}`); rec.Code != 204 {
		t.Errorf("admin reset: %d", rec.Code)
	}
	kid = userClient(t, sf, "kid", "")

	// Rename (by the user).
	if rec := call(t, sf.h, "POST", "/Users/"+created.Id, kid, `{"Name":"junior"}`); rec.Code != 204 {
		t.Errorf("rename: %d", rec.Code)
	}
	var me struct{ Name string }
	_ = json.Unmarshal(call(t, sf.h, "GET", "/Users/Me", kid, "").Body.Bytes(), &me)
	if me.Name != "junior" {
		t.Errorf("name after rename: %q", me.Name)
	}

	// Disabling signs the user out at once and blocks logins.
	if rec := call(t, sf.h, "POST", "/Users/"+created.Id+"/Policy", admin, `{"IsDisabled":true}`); rec.Code != 204 {
		t.Fatal(rec.Code)
	}
	if rec := call(t, sf.h, "GET", "/Users/Me", kid, ""); rec.Code != 401 {
		t.Errorf("disabled user's token: %d", rec.Code)
	}
	dev := `MediaBrowser Client="C", DeviceId="kid2", Device="P", Version="1"`
	if rec := call(t, sf.h, "POST", "/Users/AuthenticateByName", dev, `{"Username":"junior","Pw":""}`); rec.Code != 401 {
		t.Errorf("disabled user's login: %d", rec.Code)
	}

	// Guards: an admin can't demote, disable or delete themselves.
	var adminMe struct{ Id string }
	getJSON(t, sf.h, "/Users/Me", &adminMe)
	for _, body := range []string{`{"IsAdministrator":false}`, `{"IsDisabled":true}`} {
		if rec := call(t, sf.h, "POST", "/Users/"+adminMe.Id+"/Policy", admin, body); rec.Code != 400 {
			t.Errorf("self policy %s: %d", body, rec.Code)
		}
	}
	if rec := call(t, sf.h, "DELETE", "/Users/"+adminMe.Id, admin, ""); rec.Code != 400 {
		t.Errorf("delete self: %d", rec.Code)
	}

	// Deleting removes the user.
	if rec := call(t, sf.h, "DELETE", "/Users/"+created.Id, admin, ""); rec.Code != 204 {
		t.Errorf("delete: %d", rec.Code)
	}
	if rec := call(t, sf.h, "GET", "/Users/"+created.Id, admin, ""); rec.Code != 404 {
		t.Errorf("deleted user: %d", rec.Code)
	}
}
