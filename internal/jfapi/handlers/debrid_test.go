package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sysadmin/blockbustr/internal/debrid"
	"github.com/sysadmin/blockbustr/internal/jfapi"
	"github.com/sysadmin/blockbustr/internal/provider"
	"github.com/sysadmin/blockbustr/internal/secret"
	"github.com/sysadmin/blockbustr/internal/testutil"
)

// fakeAccountInfo answers AccountInfo; the key "bad" is refused.
type fakeAccountInfo struct {
	provider.Provider
	name provider.Name
	key  string
}

func (f fakeAccountInfo) Name() provider.Name { return f.name }

func (f fakeAccountInfo) AccountInfo(context.Context) (provider.Account, error) {
	if f.key == "bad" {
		return provider.Account{}, errors.New("HTTP 401 bad_token")
	}
	return provider.Account{PremiumUntil: time.Now().Add(24 * time.Hour)}, nil
}

func TestDebridAdmin(t *testing.T) {
	_, d := newIntegrationServer(t)
	seedCaptureToken(t, d)
	key := bytes.Repeat([]byte{9}, secret.KeyLen)
	set := provider.NewSet(nil, nil)
	mgr := &debrid.Manager{
		Q: d.Queries, Key: key, Set: set, Log: testutil.Discard(),
		// Opens the enabled rows like main's loadProviders, as fakes.
		Load: func(ctx context.Context) (map[provider.Name]provider.Provider, []provider.Name, error) {
			rows, err := d.Queries.ListDebridAccounts(ctx)
			if err != nil {
				return nil, nil, err
			}
			out := map[provider.Name]provider.Provider{}
			var order []provider.Name
			for _, r := range rows {
				if !r.Enabled {
					continue
				}
				k, err := secret.Open(key, r.ApiKeyEnc)
				if err != nil {
					return nil, nil, err
				}
				n := provider.Name(r.Provider)
				out[n] = fakeAccountInfo{name: n, key: string(k)}
				order = append(order, n)
			}
			return out, order, nil
		},
	}
	d.Debrid = mgr
	rt := jfapi.NewRouter(testutil.Discard(), jfapi.Options{LegacyAuth: true})
	Register(rt, d)
	admin := `MediaBrowser Token="` + captureToken + `"`
	type listing struct {
		Accounts []debrid.Account
		CanStore bool
	}
	list := func() listing {
		t.Helper()
		var l listing
		rec := call(t, rt, "GET", "/blockbustr/debrid", admin, "")
		if rec.Code != 200 {
			t.Fatalf("list: %d %s", rec.Code, rec.Body)
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &l)
		return l
	}
	if l := list(); len(l.Accounts) != 0 || !l.CanStore {
		t.Fatalf("initial: %+v", l)
	}

	// Adding keys: stored sealed, active at once (no restart), never shown.
	for _, c := range []struct{ p, body string }{
		{"realdebrid", `{"ApiKey":"rd-secret-key","Priority":5}`},
		{"torbox", `{"ApiKey":"bad"}`},
	} {
		rec := call(t, rt, "POST", "/blockbustr/debrid/"+c.p, admin, c.body)
		if rec.Code != 200 || strings.Contains(rec.Body.String(), "rd-secret-key") {
			t.Fatalf("put %s: %d %s", c.p, rec.Code, rec.Body)
		}
	}
	l := list()
	if len(l.Accounts) != 2 || l.Accounts[0].Provider != provider.RealDebrid || l.Accounts[0].Priority != 5 || !l.Accounts[0].Active || !l.Accounts[1].Active {
		t.Errorf("after adding: %+v", l.Accounts)
	}
	if names := set.Names(); len(names) != 2 || names[0] != provider.RealDebrid {
		t.Errorf("live set: %v", names)
	}
	var stored []byte
	if err := testPool.QueryRow(t.Context(), `SELECT api_key_enc FROM debrid_accounts WHERE provider = 'realdebrid'`).Scan(&stored); err != nil || bytes.Contains(stored, []byte("rd-secret-key")) {
		t.Errorf("key stored in the clear (or missing): %v", err)
	}

	// Checks: a working key, and one the provider refuses.
	var st debrid.Status
	_ = json.Unmarshal(call(t, rt, "GET", "/blockbustr/debrid/realdebrid/status", admin, "").Body.Bytes(), &st)
	if !st.OK || st.PremiumUntil.IsZero() {
		t.Errorf("rd status: %+v", st)
	}
	st = debrid.Status{}
	_ = json.Unmarshal(call(t, rt, "GET", "/blockbustr/debrid/torbox/status", admin, "").Body.Bytes(), &st)
	if st.OK || !strings.Contains(st.Error, "bad_token") {
		t.Errorf("torbox status: %+v", st)
	}

	// Disabling takes it out of the live set; priority changes reorder.
	if rec := call(t, rt, "POST", "/blockbustr/debrid/torbox", admin, `{"Enabled":false}`); rec.Code != 200 {
		t.Fatal(rec.Code)
	}
	if set.Get(provider.TorBox) != nil || set.Len() != 1 {
		t.Errorf("disabled account still live: %v", set.Names())
	}
	if rec := call(t, rt, "POST", "/blockbustr/debrid/torbox", admin, `{"Enabled":true,"Priority":9}`); rec.Code != 200 {
		t.Fatal(rec.Code)
	}
	if names := set.Names(); len(names) != 2 || names[0] != provider.TorBox {
		t.Errorf("after reprioritising: %v", names)
	}

	// Bad input; deleting.
	if rec := call(t, rt, "POST", "/blockbustr/debrid/premiumize", admin, `{"ApiKey":"x"}`); rec.Code != 400 {
		t.Errorf("unknown provider: %d", rec.Code)
	}
	if rec := call(t, rt, "POST", "/blockbustr/debrid/realdebrid", admin, `{"ApiKey":""}`); rec.Code != 400 {
		t.Errorf("empty key: %d", rec.Code)
	}
	if rec := call(t, rt, "DELETE", "/blockbustr/debrid/torbox", admin, ""); rec.Code != 204 {
		t.Errorf("delete: %d", rec.Code)
	}
	if set.Get(provider.TorBox) != nil || len(list().Accounts) != 1 {
		t.Errorf("deleted account remains: %v", set.Names())
	}

	// Without a secret key nothing can be stored.
	mgr.Key = nil
	if rec := call(t, rt, "POST", "/blockbustr/debrid/torbox", admin, `{"ApiKey":"k"}`); rec.Code != 503 {
		t.Errorf("no secret key: %d", rec.Code)
	}
	// Admins only.
	if rec := call(t, rt, "GET", "/blockbustr/debrid", "", ""); rec.Code != 401 {
		t.Errorf("anonymous: %d", rec.Code)
	}
}

// The admin UI: "/blockbustr/ui" redirects to "/blockbustr/ui/" (routing
// trims the slash, which once made this a redirect loop), and the app and
// its client routes answer with a page.
func TestAdminUIRoutes(t *testing.T) {
	h, _ := newIntegrationServer(t)
	rec := call(t, h, "GET", "/blockbustr/ui", "", "")
	if rec.Code != 302 || rec.Header().Get("Location") != "/blockbustr/ui/" {
		t.Errorf("/blockbustr/ui: %d → %q", rec.Code, rec.Header().Get("Location"))
	}
	for _, p := range []string{"/blockbustr/ui/", "/blockbustr/ui/users", "/BlockBustr/UI/"} {
		rec := call(t, h, "GET", p, "", "")
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), "blockbustr admin") {
			t.Errorf("%s: %d", p, rec.Code)
		}
	}
}
