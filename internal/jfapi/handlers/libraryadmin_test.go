package handlers

import (
	"encoding/json"
	"strings"
	"testing"
)

// The admin orders and hides libraries for everyone; a user's own order
// and exclusions (saved from their app's settings) apply on top.
func TestLibraryOrderAndHiding(t *testing.T) {
	sf := newSyncFixture(t)
	sf.syncAll(t)
	admin := `MediaBrowser Token="` + captureToken + `"`
	v := sf.views(t)
	movies, shows := v["Popular Movies"].Id, v["Popular Shows"].Id
	// viewNames lists /UserViews (with query) in the order it comes.
	viewNames := func(query string) string {
		t.Helper()
		rec := call(t, sf.h, "GET", "/UserViews"+query, admin, "")
		var res struct{ Items []struct{ Name string } }
		if err := json.Unmarshal(rec.Body.Bytes(), &res); rec.Code != 200 || err != nil {
			t.Fatalf("views: %d %v", rec.Code, err)
		}
		var n []string
		for _, it := range res.Items {
			n = append(n, it.Name)
		}
		return strings.Join(n, ",")
	}
	post := func(path, body string) {
		t.Helper()
		if rec := call(t, sf.h, "POST", path, admin, body); rec.Code != 204 {
			t.Fatalf("%s: %d %s", path, rec.Code, rec.Body)
		}
	}
	latestNames := func(query string) string {
		t.Helper()
		var res []struct{ Name string }
		rec := call(t, sf.h, "GET", "/Items/Latest"+query, admin, "")
		if err := json.Unmarshal(rec.Body.Bytes(), &res); rec.Code != 200 || err != nil {
			t.Fatalf("latest: %d %v", rec.Code, err)
		}
		var n []string
		for _, it := range res {
			n = append(n, it.Name)
		}
		return strings.Join(n, ",")
	}

	if got := viewNames(""); got != "Popular Movies,Popular Shows" {
		t.Fatalf("default order: %s", got)
	}
	allLatest := latestNames("?limit=50")

	// The admin's order.
	post("/blockbustr/libraries/order", `{"Ids":["`+shows+`","`+movies+`"]}`)
	if got := viewNames(""); got != "Popular Shows,Popular Movies" {
		t.Errorf("admin order: %s", got)
	}
	var libs []adminLibrary
	if err := json.Unmarshal(call(t, sf.h, "GET", "/blockbustr/libraries", admin, "").Body.Bytes(), &libs); err != nil || len(libs) != 2 || libs[0].Id != shows || !libs[0].Catalog {
		t.Errorf("admin list: %+v %v", libs, err)
	}

	// The user's own order wins; a library they excluded leaves their views
	// but is listed with includeHidden (their settings page); one excluded
	// from Latest leaves the all-libraries Latest.
	post("/Users/Configuration", `{"OrderedViews":["`+movies+`"],"MyMediaExcludes":["`+shows+`"],"LatestItemsExcludes":["`+movies+`"]}`)
	if got := viewNames(""); got != "Popular Movies" {
		t.Errorf("with the user's exclusion: %s", got)
	}
	if got := viewNames("?includeHidden=true"); got != "Popular Movies,Popular Shows" {
		t.Errorf("includeHidden in the user's order: %s", got)
	}
	var me struct {
		Configuration struct{ OrderedViews, MyMediaExcludes []string }
	}
	if err := json.Unmarshal(call(t, sf.h, "GET", "/Users/Me", admin, "").Body.Bytes(), &me); err != nil ||
		len(me.Configuration.OrderedViews) != 1 || len(me.Configuration.MyMediaExcludes) != 1 {
		t.Errorf("configuration read back: %+v %v", me.Configuration, err)
	}
	if got := latestNames("?limit=50"); got == allLatest || got == "" {
		t.Errorf("Latest with movies excluded: %q (all: %q)", got, allLatest)
	}
	if got := latestNames("?limit=50&parentId=" + movies); got == "" {
		t.Error("Latest of the excluded library asked for by id is empty")
	}
	post("/Users/Configuration", `{}`)

	// Hidden by the admin: gone for everyone, even with includeHidden, and
	// from Latest; still listed for the admin, and back when unhidden.
	post("/blockbustr/libraries/"+movies, `{"Hidden":true}`)
	if got := viewNames("?includeHidden=true"); got != "Popular Shows" {
		t.Errorf("admin-hidden library in views: %s", got)
	}
	if got, want := latestNames("?limit=50"), latestNames("?limit=50&parentId="+shows); got != want {
		t.Errorf("Latest with movies hidden: %q, want the shows' %q", got, want)
	}
	libs = nil
	_ = json.Unmarshal(call(t, sf.h, "GET", "/blockbustr/libraries", admin, "").Body.Bytes(), &libs)
	if len(libs) != 2 || libs[1].Id != movies || !libs[1].Hidden {
		t.Errorf("admin list with a hidden library: %+v", libs)
	}
	post("/blockbustr/libraries/"+movies, `{"Hidden":false}`)
	if got := viewNames(""); got != "Popular Shows,Popular Movies" {
		t.Errorf("unhidden: %s", got)
	}

	for path, body := range map[string]string{
		"/blockbustr/libraries/order":     `{"Ids":["nope"]}`,
		"/blockbustr/libraries/" + movies: `{}`,
	} {
		if rec := call(t, sf.h, "POST", path, admin, body); rec.Code != 400 {
			t.Errorf("%s %s = %d, want 400", path, body, rec.Code)
		}
	}
	if rec := call(t, sf.h, "POST", "/blockbustr/libraries/00000000000000000000000000000042", admin, `{"Hidden":true}`); rec.Code != 404 {
		t.Errorf("hiding an unknown library = %d", rec.Code)
	}
}
