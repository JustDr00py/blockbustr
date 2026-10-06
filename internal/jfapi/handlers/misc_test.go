package handlers

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/sysadmin/blockbustr/internal/store/pg/db"
)

func TestMiscContract(t *testing.T) {
	h, d := newIntegrationServer(t)
	seedCaptureToken(t, d)
	for _, ep := range []string{
		"GET /Items/{id}/Collections", "GET /Items/{id}/ThemeMedia", "GET /LiveTv/Programs/Recommended",
		"GET /MediaSegments/{id}", "GET /SyncPlay/List", "GET /Users/{id}/Items/{id}/Intros",
	} {
		checkContractOn(t, h, capturesFor(t, ep))
	}
	// Plugin probes stay 404 ("plugin not installed", DESIGN §3.5).
	checkContractOn(t, h, capturesFor(t, "GET /Streamyfin/config"))
}

// What jellyfin-web saved comes back exactly as Jellyfin returned it,
// Id included.
func TestDisplayPreferencesContract(t *testing.T) {
	h, d := newIntegrationServer(t)
	execSQL(t, `UPDATE users SET id = $1 WHERE name = 'user1'`, captureUserID) // captures name the user by id
	seedCaptureToken(t, d)
	authz := `MediaBrowser Token="` + captureToken + `"`
	for _, c := range capturesFor(t, "GET /DisplayPreferences/usersettings") {
		path := c.Request.Path + "?client=" + c.Request.Query["client"][0]
		if rec := call(t, h, "POST", path, authz, string(c.Response.Body)); rec.Code != 204 {
			t.Fatalf("save: %d %s", rec.Code, rec.Body)
		}
		var want, got any
		_ = json.Unmarshal(c.Response.Body, &want)
		_ = json.Unmarshal(replay(t, h, c).Body.Bytes(), &got)
		if !reflect.DeepEqual(want, got) {
			t.Errorf("%s:\n  jellyfin:   %v\n  blockbustr: %v", c.Name, want, got)
		}
	}
}

func TestDisplayPreferences(t *testing.T) {
	h, d := newIntegrationServer(t)
	seedCaptureToken(t, d)
	authz := `MediaBrowser Token="` + captureToken + `"`
	type prefs struct {
		Id, Client, SortBy, SortOrder, ScrollDirection string
		ShowBackdrop                                   bool
		PrimaryImageHeight                             int
		CustomPrefs                                    map[string]*string
	}
	get := func(path string) prefs {
		var p prefs
		getJSON(t, h, path, &p)
		return p
	}
	if p := get("/DisplayPreferences/usersettings?client=findroid"); p.Id != "3ce5b65d-e116-d731-65d1-efc4a30ec35c" ||
		p.Client != "findroid" || p.SortBy != "SortName" || p.ScrollDirection != "Horizontal" || !p.ShowBackdrop ||
		p.PrimaryImageHeight != 250 || p.CustomPrefs == nil || len(p.CustomPrefs) != 0 {
		t.Errorf("defaults: %+v", p)
	}
	// camelCase body, dictionary keys kept as sent, unset fields keep defaults.
	if rec := call(t, h, "POST", "/DisplayPreferences/home?client=streamyfin", authz,
		`{"sortOrder":"Descending","customPrefs":{"homeSection0":"resume","tvHome":null}}`); rec.Code != 204 {
		t.Fatalf("save: %d", rec.Code)
	}
	p := get("/DisplayPreferences/home?client=streamyfin")
	if p.SortOrder != "Descending" || p.SortBy != "SortName" || p.CustomPrefs["homeSection0"] == nil ||
		*p.CustomPrefs["homeSection0"] != "resume" || p.CustomPrefs["tvHome"] != nil || len(p.CustomPrefs) != 2 {
		t.Errorf("saved: %+v", p)
	}
	if other := get("/DisplayPreferences/home?client=findroid"); other.SortOrder != "Ascending" {
		t.Errorf("preferences are per client: %+v", other)
	}
	if rec := call(t, h, "POST", "/DisplayPreferences/home", authz, `{"SortBy":`); rec.Code != 400 {
		t.Errorf("bad body = %d", rec.Code)
	}
}

func TestStubsAccess(t *testing.T) {
	h, d := newIntegrationServer(t)
	seedCaptureToken(t, d)
	admin := `MediaBrowser Token="` + captureToken + `"`
	if _, err := d.Queries.CreateUser(t.Context(), db.CreateUserParams{Name: "kid"}); err != nil {
		t.Fatal(err)
	}
	rec := call(t, h, "POST", "/Users/AuthenticateByName", `MediaBrowser Client="C", DeviceId="kid-1", Device="P", Version="1"`, `{"Username":"kid","Pw":""}`)
	var login struct{ AccessToken string }
	_ = json.Unmarshal(rec.Body.Bytes(), &login)
	kid := `MediaBrowser Token="` + login.AccessToken + `"`

	for path, want := range map[string]string{
		"/Plugins": "[]", "/Packages": "[]", "/ScheduledTasks": "[]", "/web/ConfigurationPages": "[]",
		"/Localization/Cultures": "[]", "/Localization/ParentalRatings": "[]",
		"/Items/" + mfGhost + "/SpecialFeatures": "[]", "/Users/" + strings.ReplaceAll(captureUserID, "-", "") + "/Items/" + mfGhost + "/LocalTrailers": "[]",
		"/Items/" + mfGhost + "/ThemeSongs": `{"Items":[],"OwnerId":"` + mfGhost + `","StartIndex":0,"TotalRecordCount":0}`,
	} {
		if rec := call(t, h, "GET", path, admin, ""); rec.Code != 200 || canonical(t, rec.Body.String()) != canonical(t, want) {
			t.Errorf("%s = %d %s", path, rec.Code, rec.Body)
		}
	}
	for _, p := range []string{"/Plugins", "/Packages", "/ScheduledTasks"} {
		if rec := call(t, h, "GET", p, kid, ""); rec.Code != 403 {
			t.Errorf("%s as a non-admin = %d", p, rec.Code)
		}
	}
	if rec := call(t, h, "GET", "/Localization/Options", "", ""); rec.Code != 401 {
		t.Errorf("anonymous = %d", rec.Code)
	}
}

// canonical re-encodes JSON so key order doesn't matter.
func canonical(t *testing.T, s string) string {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatalf("%q: %v", s, err)
	}
	b, _ := json.Marshal(v)
	return string(b)
}
