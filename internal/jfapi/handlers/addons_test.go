package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"

	"github.com/sysadmin/blockbustr/internal/jfapi"
	"github.com/sysadmin/blockbustr/internal/secret"
	"github.com/sysadmin/blockbustr/internal/store/pg/db"
	"github.com/sysadmin/blockbustr/internal/stremio"
	"github.com/sysadmin/blockbustr/internal/testutil"
)

// addonConfig stands in for a configured addon's settings (often a debrid
// key); it must never come back from the API or sit in the database.
const addonConfig = "providers=yts|realdebrid=SECRETKEY123"

type addonFixture struct {
	h        http.Handler
	reg      *stremio.Registry
	manifest atomic.Pointer[[]byte] // what the fake addon serves
	url      string                 // its manifest URL, config included
}

func newAddonFixture(t *testing.T, key []byte) *addonFixture {
	t.Helper()
	_, d := newIntegrationServer(t)
	seedCaptureToken(t, d)
	f := &addonFixture{}
	b, err := os.ReadFile("../../stremio/testdata/cinemeta-manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	f.manifest.Store(&b)
	addon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/manifest.json") {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(*f.manifest.Load())
	}))
	t.Cleanup(addon.Close)
	f.url = addon.URL + "/" + addonConfig + "/manifest.json"

	f.reg = &stremio.Registry{Pool: testPool, Client: &stremio.Client{}, Key: key}
	d.Addons = f.reg
	rt := jfapi.NewRouter(testutil.Discard(), jfapi.Options{LegacyAuth: true})
	Register(rt, d)
	f.h = rt
	return f
}

func (f *addonFixture) call(t *testing.T, method, path, body string) (int, map[string]any) {
	t.Helper()
	rec := call(t, f.h, method, path, `MediaBrowser Token="`+captureToken+`"`, body)
	if strings.Contains(rec.Body.String(), "SECRETKEY") || strings.Contains(rec.Body.String(), "realdebrid=") {
		t.Errorf("%s %s leaks the addon URL: %s", method, path, rec.Body)
	}
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func catalogState(t *testing.T, ad map[string]any) map[string]map[string]any {
	t.Helper()
	out := map[string]map[string]any{}
	for _, c := range ad["Catalogs"].([]any) {
		c := c.(map[string]any)
		out[c["Type"].(string)+"/"+c["Id"].(string)] = c
	}
	return out
}

func TestAddonsLifecycle(t *testing.T) {
	key := bytes.Repeat([]byte{7}, secret.KeyLen)
	f := newAddonFixture(t, key)

	code, ad := f.call(t, "POST", "/blockbustr/addons", `{"Url":"`+f.url+`","Priority":5}`)
	if code != 200 || ad["ManifestId"] != "com.linvo.cinemeta" || ad["Name"] != "Cinemeta" || ad["Priority"] != float64(5) ||
		ad["Enabled"] != true || !strings.HasPrefix(ad["Host"].(string), "http://127.0.0.1:") {
		t.Fatalf("add: %d %v", code, ad)
	}
	id := ad["Id"].(string)
	cats := catalogState(t, ad)
	if len(cats) != 8 {
		t.Errorf("catalogs: %d", len(cats))
	}
	for k, c := range cats {
		if c["Enabled"] != false {
			t.Errorf("%s enabled on add", k)
		}
	}
	if req := cats["movie/year"]["Requires"].([]any); len(req) != 1 || req[0] != "genre" {
		t.Errorf("movie/year requires %v", req)
	}

	// Sealed at rest: the database never holds the URL.
	var enc []byte
	if err := testPool.QueryRow(t.Context(), `SELECT url_enc FROM stremio_addons`).Scan(&enc); err != nil {
		t.Fatal(err)
	}
	var dump string
	_ = testPool.QueryRow(t.Context(), `SELECT row_to_json(a)::text FROM stremio_addons a`).Scan(&dump)
	if bytes.Contains(enc, []byte("SECRETKEY")) || strings.Contains(dump, "SECRETKEY") || strings.Contains(dump, "realdebrid=") {
		t.Error("addon URL stored in clear")
	}
	row, _ := db.New(testPool).GetStremioAddon(t.Context(), uuid.MustParse(id))
	want, _ := stremio.BaseURL(f.url)
	if base, err := f.reg.Base(row); err != nil || base != want {
		t.Errorf("Base: %q %v, want %q", base, err, want)
	}

	// The same addon again, in either spelling of its config, is a duplicate.
	for _, u := range []string{f.url, strings.ReplaceAll(f.url, "|", "%7C")} {
		if code, _ := f.call(t, "POST", "/blockbustr/addons", `{"Url":"`+u+`"}`); code != 409 {
			t.Errorf("duplicate %s = %d", u, code)
		}
	}
	var list []map[string]any
	rec := call(t, f.h, "GET", "/blockbustr/addons", `MediaBrowser Token="`+captureToken+`"`, "")
	_ = json.Unmarshal(rec.Body.Bytes(), &list)
	if rec.Code != 200 || len(list) != 1 || list[0]["Id"] != id {
		t.Errorf("list: %d %v", rec.Code, list)
	}

	if code, ad = f.call(t, "POST", "/blockbustr/addons/"+id, `{"Enabled":false}`); code != 200 || ad["Enabled"] != false || ad["Priority"] != float64(5) {
		t.Errorf("disable keeps priority: %d %v", code, ad)
	}
	if code, ad = f.call(t, "POST", "/blockbustr/addons/"+id, `{"priority":9}`); code != 200 || ad["Enabled"] != false || ad["Priority"] != float64(9) {
		t.Errorf("priority keeps enabled: %d %v", code, ad)
	}

	if code, ad = f.call(t, "POST", "/blockbustr/addons/"+id+"/catalogs/movie/top", `{"Enabled":true}`); code != 200 || catalogState(t, ad)["movie/top"]["Enabled"] != true {
		t.Errorf("enable movie/top: %d %v", code, ad)
	}
	if code, body := f.call(t, "POST", "/blockbustr/addons/"+id+"/catalogs/movie/year", `{"Enabled":true}`); code != 400 || !strings.Contains(body["Error"].(string), "extra") {
		t.Errorf("enable a genre-required catalog = %d %v", code, body)
	}
	if code, _ := f.call(t, "POST", "/blockbustr/addons/"+id+"/catalogs/movie/nope", `{"Enabled":true}`); code != 404 {
		t.Errorf("unknown catalog = %d", code)
	}
	if code, _ := f.call(t, "POST", "/blockbustr/addons/"+id+"/catalogs/movie/top", `{}`); code != 400 {
		t.Errorf("no Enabled = %d", code)
	}

	// Refresh: a renamed catalog keeps its state, a new one is disabled, a
	// dropped one goes.
	var m map[string]any
	_ = json.Unmarshal(*f.manifest.Load(), &m)
	var kept []any
	for _, c := range m["catalogs"].([]any) {
		c := c.(map[string]any)
		if c["id"] == "imdbRating" {
			continue
		}
		if c["type"] == "movie" && c["id"] == "top" {
			c["name"] = "Popular movies"
		}
		kept = append(kept, c)
	}
	m["catalogs"] = append(kept, map[string]any{"type": "movie", "id": "new", "name": "New"})
	nb, _ := json.Marshal(m)
	f.manifest.Store(&nb)
	if code, ad = f.call(t, "POST", "/blockbustr/addons/"+id+"/Refresh", ""); code != 200 {
		t.Fatalf("refresh: %d %v", code, ad)
	}
	cats = catalogState(t, ad)
	if top := cats["movie/top"]; top["Enabled"] != true || top["Name"] != "Popular movies" {
		t.Errorf("renamed catalog: %v", top)
	}
	if cats["movie/new"]["Enabled"] != false || cats["movie/imdbRating"] != nil || cats["series/imdbRating"] != nil || len(cats) != 7 {
		t.Errorf("after refresh: %v", cats)
	}

	if code, _ := f.call(t, "DELETE", "/blockbustr/addons/"+id, ""); code != 204 {
		t.Errorf("delete = %d", code)
	}
	if code, _ := f.call(t, "GET", "/blockbustr/addons/"+id, ""); code != 404 {
		t.Errorf("get after delete = %d", code)
	}
	var n int
	_ = testPool.QueryRow(t.Context(), `SELECT count(*) FROM stremio_catalogs`).Scan(&n)
	if n != 0 {
		t.Errorf("catalogs left after delete: %d", n)
	}
	if code, _ := f.call(t, "DELETE", "/blockbustr/addons/"+id, ""); code != 404 {
		t.Errorf("second delete = %d", code)
	}
}

func TestAddonsErrorsAndAccess(t *testing.T) {
	f := newAddonFixture(t, bytes.Repeat([]byte{7}, secret.KeyLen))
	for body, want := range map[string]int{
		`{}`:                              400,
		`not json`:                        400,
		`{"Url":"ftp://x/manifest.json"}`: 400,
		`{"Url":"http://127.0.0.1:1/` + addonConfig + `/manifest.json"}`: 502, // refused; message names the host only
	} {
		if code, out := f.call(t, "POST", "/blockbustr/addons", body); code != want {
			t.Errorf("%s = %d %v, want %d", body, code, out, want)
		}
	}
	// The addon answers something that isn't a manifest.
	bad := []byte(`{"name":"no id"}`)
	f.manifest.Store(&bad)
	if code, out := f.call(t, "POST", "/blockbustr/addons", `{"Url":"`+f.url+`"}`); code != 502 || !strings.Contains(out["Error"].(string), "no id") {
		t.Errorf("not a manifest = %d %v", code, out)
	}
	if code, _ := f.call(t, "GET", "/blockbustr/addons/zzz", ""); code != 404 {
		t.Errorf("bad id = %d", code)
	}

	// Admins only.
	if rec := call(t, f.h, "GET", "/blockbustr/addons", "", ""); rec.Code != 401 {
		t.Errorf("anonymous = %d", rec.Code)
	}
	if _, err := db.New(testPool).CreateUser(t.Context(), db.CreateUserParams{Name: "kid"}); err != nil {
		t.Fatal(err)
	}
	rec := call(t, f.h, "POST", "/Users/AuthenticateByName", `MediaBrowser Client="C", DeviceId="kid-1", Device="P", Version="1"`, `{"Username":"kid","Pw":""}`)
	var login struct{ AccessToken string }
	_ = json.Unmarshal(rec.Body.Bytes(), &login)
	if rec := call(t, f.h, "POST", "/blockbustr/addons", `MediaBrowser Token="`+login.AccessToken+`"`, `{"Url":"`+f.url+`"}`); rec.Code != 403 {
		t.Errorf("non-admin add = %d", rec.Code)
	}
}

func TestAddonsNeedSecretKey(t *testing.T) {
	f := newAddonFixture(t, nil)
	code, out := f.call(t, "POST", "/blockbustr/addons", `{"Url":"`+f.url+`"}`)
	if code != 503 || !strings.Contains(out["Error"].(string), "BLOCKBUSTR_SECRET_KEY") {
		t.Errorf("no secret_key = %d %v", code, out)
	}
	if rec := call(t, f.h, "GET", "/blockbustr/addons", `MediaBrowser Token="`+captureToken+`"`, ""); rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != "[]" {
		t.Errorf("list without key = %d %s", rec.Code, rec.Body)
	}
}

// An addon's URL can change to another configuration of the same addon
// (a new debrid key): it keeps its id, priority and catalog choices, and
// the URL is still never shown.
func TestAddonURLChange(t *testing.T) {
	f := newAddonFixture(t, bytes.Repeat([]byte{3}, 32))
	server := strings.TrimSuffix(f.url, "/"+addonConfig+"/manifest.json")
	code, ad := f.call(t, "POST", "/blockbustr/addons", `{"Url":"`+f.url+`","Priority":7}`)
	if code != 200 {
		t.Fatalf("add: %d %v", code, ad)
	}
	id := ad["Id"].(string)
	if code, _ := f.call(t, "POST", "/blockbustr/addons/"+id+"/catalogs/movie/top", `{"Enabled":true}`); code != 200 {
		t.Fatalf("enable catalog: %d", code)
	}

	newURL := server + "/providers=yts|realdebrid=SECRETKEY456/manifest.json"
	code, ad = f.call(t, "POST", "/blockbustr/addons/"+id, `{"Url":"`+newURL+`"}`)
	if code != 200 || ad["Id"] != id || ad["Priority"].(float64) != 7 || catalogState(t, ad)["movie/top"]["Enabled"] != true {
		t.Fatalf("change URL: %d %v", code, ad)
	}
	uid, err := uuid.Parse(id)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := testutilBase(t, f, uid)
	if err != nil || !strings.Contains(stored, "SECRETKEY456") {
		t.Errorf("stored URL not the new one: %v", err)
	}

	// Another addon's manifest at the new URL: refused, nothing changes.
	orig := *f.manifest.Load()
	other, err := os.ReadFile("../../stremio/testdata/torrentio-manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	f.manifest.Store(&other)
	if code, out := f.call(t, "POST", "/blockbustr/addons/"+id, `{"Url":"`+server+`/other/manifest.json"}`); code != 400 {
		t.Errorf("different addon: %d %v", code, out)
	}
	f.manifest.Store(&orig)

	// A URL another addon already has: 409. An unreachable one: 502.
	third := server + "/providers=yts|realdebrid=SECRETKEY789/manifest.json"
	if code, _ := f.call(t, "POST", "/blockbustr/addons", `{"Url":"`+third+`"}`); code != 200 {
		t.Fatalf("second addon: %d", code)
	}
	if code, _ := f.call(t, "POST", "/blockbustr/addons/"+id, `{"Url":"`+third+`"}`); code != 409 {
		t.Errorf("taken URL: %d", code)
	}
	if code, _ := f.call(t, "POST", "/blockbustr/addons/"+id, `{"Url":"http://127.0.0.1:1/x/manifest.json"}`); code != 502 {
		t.Errorf("unreachable URL: %d", code)
	}
	if stored, _ := testutilBase(t, f, uid); !strings.Contains(stored, "SECRETKEY456") {
		t.Error("a failed change altered the stored URL")
	}
}

// testutilBase opens an addon's stored (sealed) URL.
func testutilBase(t *testing.T, f *addonFixture, id uuid.UUID) (string, error) {
	t.Helper()
	row, err := db.New(testPool).GetStremioAddon(t.Context(), id)
	if err != nil {
		return "", err
	}
	return f.reg.Base(row)
}
