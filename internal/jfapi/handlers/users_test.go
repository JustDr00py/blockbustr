package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/sysadmin/blockbustr/internal/jfapi"
	"github.com/sysadmin/blockbustr/internal/store/pg/db"
	"github.com/sysadmin/blockbustr/internal/testutil"
)

const findroidAuth = `MediaBrowser Client="Findroid", Version="1.1.0", DeviceId="device-0001", Device="Device 1"`

func call(t *testing.T, h http.Handler, method, path, authz, body string) *httptest.ResponseRecorder {
	t.Helper()
	var rd *strings.Reader
	if body != "" {
		rd = strings.NewReader(body)
	} else {
		rd = strings.NewReader("")
	}
	req := httptest.NewRequestWithContext(context.Background(), method, path, rd)
	if authz != "" {
		req.Header.Set("Authorization", authz)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// login signs in as the bootstrapped admin and returns the access token.
func login(t *testing.T, h http.Handler) string {
	t.Helper()
	rec := call(t, h, "POST", "/Users/AuthenticateByName", findroidAuth, `{"Username":"user1","Pw":"REDACTED"}`)
	if rec.Code != 200 {
		t.Fatalf("login = %d %s", rec.Code, rec.Body)
	}
	var res struct{ AccessToken string }
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil || len(res.AccessToken) != 32 {
		t.Fatalf("token: %q %v", res.AccessToken, err)
	}
	return res.AccessToken
}

func withToken(tok string) string { return findroidAuth + `, Token="` + tok + `"` }

func golden(t *testing.T, name string) any {
	t.Helper()
	raw, err := os.ReadFile("testdata/" + name + ".json")
	if err != nil {
		t.Fatal(err)
	}
	var v any
	_ = json.Unmarshal(raw, &v)
	return v
}

func decode(t *testing.T, rec *httptest.ResponseRecorder) any {
	t.Helper()
	var v any
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("not JSON: %v %q", err, rec.Body)
	}
	return v
}

// Every captured successful login replays against blockbustr with the same
// shape: User (with full Configuration/Policy), SessionInfo, AccessToken, ServerId.
func TestAuthenticateByNameContract(t *testing.T) {
	h, d := newIntegrationServer(t)
	var ok []capture
	for _, c := range capturesFor(t, "POST /Users/AuthenticateByName") {
		if c.Response.Status == 200 {
			ok = append(ok, c)
		}
	}
	for _, b := range checkContractOn(t, h, ok) {
		res := b.(map[string]any)
		user := res["User"].(map[string]any)
		if user["Name"] != "user1" || res["ServerId"] != d.ServerID.String() || user["Policy"].(map[string]any)["IsAdministrator"] != true {
			t.Errorf("login result: %v", res)
		}
		if s := res["SessionInfo"].(map[string]any); s["Client"] != "Findroid" && s["Client"] != "Jellyfin for Android" && s["Client"] != "Streamyfin" {
			t.Errorf("SessionInfo.Client = %v", s["Client"])
		}
	}
}

func TestAuthenticateByNameErrors(t *testing.T) {
	h, _ := newIntegrationServer(t)
	for _, c := range []struct {
		name, authz, body string
		status            int
	}{
		{"wrong password", findroidAuth, `{"Username":"user1","Pw":"nope"}`, 401},
		{"unknown user", findroidAuth, `{"Username":"ghost","Pw":"x"}`, 401},
		{"no client fields", "", `{"Username":"user1","Pw":"REDACTED"}`, 400},
		{"token-only header", `MediaBrowser Token="x"`, `{"Username":"user1","Pw":"REDACTED"}`, 400},
		{"malformed body", findroidAuth, `{"Username":`, 400},
	} {
		rec := call(t, h, "POST", "/Users/AuthenticateByName", c.authz, c.body)
		if rec.Code != c.status || rec.Body.String() != "Error processing request." || rec.Header().Get("Content-Type") != "text/plain" {
			t.Errorf("%s: %d %q %q", c.name, rec.Code, rec.Body, rec.Header().Get("Content-Type"))
		}
	}
	// Usernames are case-insensitive and camelCase bodies work (Go's decoder).
	if rec := call(t, h, "POST", "/users/authenticatebyname", findroidAuth, `{"username":"USER1","pw":"REDACTED"}`); rec.Code != 200 {
		t.Errorf("case-insensitive login = %d", rec.Code)
	}
}

func TestProtectedEndpoints(t *testing.T) {
	h, _ := newIntegrationServer(t)
	tok := login(t, h)
	for _, p := range []string{"/System/Info", "/Users/Me", "/System/Endpoint", "/Users"} {
		for name, authz := range map[string]string{"no token": "", "bad token": withToken("0123456789abcdef0123456789abcdef")} {
			rec := call(t, h, "GET", p, authz, "")
			if rec.Code != 401 || rec.Body.Len() != 0 || rec.Header().Get("Content-Type") != "" {
				t.Errorf("%s %s: %d %q %q (want bare 401)", name, p, rec.Code, rec.Body, rec.Header().Get("Content-Type"))
			}
		}
	}
	// A token-only header is enough once signed in, as in 12.1.0, and so is ?ApiKey=.
	if rec := call(t, h, "GET", "/Users/Me", `MediaBrowser Token="`+tok+`"`, ""); rec.Code != 200 {
		t.Errorf("token-only header = %d", rec.Code)
	}
	if rec := call(t, h, "GET", "/Users/Me?ApiKey="+tok, "", ""); rec.Code != 200 {
		t.Errorf("ApiKey query = %d", rec.Code)
	}
}

func TestUsersMeAndSystemInfoShapes(t *testing.T) {
	h, _ := newIntegrationServer(t)
	authz := withToken(login(t, h))
	for path, want := range map[string]string{"/Users/Me": "users-me", "/System/Info": "system-info"} {
		rec := call(t, h, "GET", path, authz, "")
		if rec.Code != 200 {
			t.Fatalf("%s = %d", path, rec.Code)
		}
		if err := sameShape("$", golden(t, want), decode(t, rec)); err != nil {
			t.Errorf("%s vs Jellyfin: %v", path, err)
		}
	}
	// httptest requests come from 192.0.2.1 (TEST-NET), which isn't a local
	// network; TestIsLocalNetwork covers the true cases.
	if rec := call(t, h, "GET", "/System/Endpoint", authz, ""); rec.Body.String() != `{"IsInNetwork":false,"IsLocal":false}` {
		t.Errorf("/System/Endpoint = %s", rec.Body)
	}
}

func TestUserByIDAndList(t *testing.T) {
	h, d := newIntegrationServer(t)
	admin := withToken(login(t, h))
	me := decode(t, call(t, h, "GET", "/Users/Me", admin, "")).(map[string]any)
	id := me["Id"].(string)

	if rec := call(t, h, "GET", "/Users/"+id, admin, ""); rec.Code != 200 {
		t.Errorf("self by id = %d", rec.Code)
	}
	if rec := call(t, h, "GET", "/Users/not-a-guid", admin, ""); rec.Code != 400 {
		t.Errorf("bad id = %d", rec.Code)
	}
	if rec := call(t, h, "GET", "/Users/00000000000000000000000000000001", admin, ""); rec.Code != 404 {
		t.Errorf("unknown user = %d", rec.Code)
	}

	// A non-admin can read only themselves and can't list users.
	if _, err := d.Queries.CreateUser(t.Context(), db.CreateUserParams{Name: "kid"}); err != nil {
		t.Fatal(err)
	}
	rec := call(t, h, "POST", "/Users/AuthenticateByName", `MediaBrowser Client="Streamyfin", DeviceId="kid-phone", Device="P", Version="0.55.0"`, `{"Username":"kid","Pw":""}`)
	if rec.Code != 200 {
		t.Fatalf("passwordless login = %d", rec.Code)
	}
	var res struct {
		AccessToken string
		User        struct {
			HasPassword bool
			Policy      struct{ IsAdministrator bool }
		}
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &res)
	if res.User.HasPassword || res.User.Policy.IsAdministrator {
		t.Errorf("kid user: %+v", res.User)
	}
	kid := `MediaBrowser Token="` + res.AccessToken + `"`
	if rec := call(t, h, "GET", "/Users/"+id, kid, ""); rec.Code != 403 {
		t.Errorf("non-admin reading admin = %d", rec.Code)
	}
	if rec := call(t, h, "GET", "/Users", kid, ""); rec.Code != 403 {
		t.Errorf("non-admin listing users = %d", rec.Code)
	}
	if list := decode(t, call(t, h, "GET", "/Users", admin, "")).([]any); len(list) != 2 {
		t.Errorf("admin list = %d users", len(list))
	}
}

func TestPublicUsersAndQuickConnect(t *testing.T) {
	h, _ := newIntegrationServer(t)
	for _, b := range checkContractOn(t, h, capturesFor(t, "GET /Users/Public")) {
		if len(b.([]any)) != 0 {
			t.Errorf("/Users/Public = %v (default policy hides users)", b)
		}
	}
	for _, b := range checkContractOn(t, h, capturesFor(t, "GET /QuickConnect/Enabled")) {
		if b != false {
			t.Errorf("QuickConnect/Enabled = %v until P2.12", b)
		}
	}
}

func TestLogout(t *testing.T) {
	h, _ := newIntegrationServer(t)
	authz := withToken(login(t, h))
	if rec := call(t, h, "POST", "/Sessions/Logout", authz, ""); rec.Code != 204 {
		t.Fatalf("logout = %d", rec.Code)
	}
	if rec := call(t, h, "GET", "/Users/Me", authz, ""); rec.Code != 401 {
		t.Errorf("after logout = %d", rec.Code)
	}
}

func TestIsLocalNetwork(t *testing.T) {
	for ip, want := range map[string]bool{
		"127.0.0.1": true, "10.89.1.3": true, "192.168.254.182": true, "100.64.12.34": true,
		"fd7a:115c:a1e0::1": true, "::1": true, "8.8.8.8": false, "192.0.2.1": false, "garbage": false,
	} {
		if got := isLocalNetwork(ip); got != want {
			t.Errorf("isLocalNetwork(%s) = %v", ip, got)
		}
	}
}

type countingTrigger struct{ n int }

func (c *countingTrigger) Trigger() { c.n++ }

func TestLibraryRefresh(t *testing.T) {
	_, d := newIntegrationServer(t)
	trig := &countingTrigger{}
	d.Library = trig
	rt := jfapi.NewRouter(testutil.Discard(), jfapi.Options{LegacyAuth: true})
	Register(rt, d)
	admin := withToken(login(t, rt))
	if rec := call(t, rt, "POST", "/library/refresh", admin, ""); rec.Code != 204 || trig.n != 1 {
		t.Errorf("admin refresh = %d, triggers %d", rec.Code, trig.n)
	}
	if rec := call(t, rt, "POST", "/Library/Refresh", "", ""); rec.Code != 401 || trig.n != 1 {
		t.Errorf("anonymous refresh = %d", rec.Code)
	}
	if _, err := d.Queries.CreateUser(t.Context(), db.CreateUserParams{Name: "kid"}); err != nil {
		t.Fatal(err)
	}
	rec := call(t, rt, "POST", "/Users/AuthenticateByName", `MediaBrowser Client="C", DeviceId="kid-1", Device="P", Version="1"`, `{"Username":"kid","Pw":""}`)
	var res struct{ AccessToken string }
	_ = json.Unmarshal(rec.Body.Bytes(), &res)
	if rec := call(t, rt, "POST", "/Library/Refresh", `MediaBrowser Token="`+res.AccessToken+`"`, ""); rec.Code != 403 || trig.n != 1 {
		t.Errorf("non-admin refresh = %d", rec.Code)
	}
}
