package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sysadmin/blockbustr/internal/provider"
	"github.com/sysadmin/blockbustr/internal/resolve"
	"github.com/sysadmin/blockbustr/internal/settings"
	"github.com/sysadmin/blockbustr/internal/stremio"
	"github.com/sysadmin/blockbustr/internal/testutil"
)

// Settings changed on the admin UI apply at once: here the stream proxy
// hosts, which take an addon stream from proxied to redirected.
func TestSettingsEndpoints(t *testing.T) {
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("matroska bytes"))
	}))
	t.Cleanup(proxy.Close)
	fs := &fakeStreams{offers: map[string][]stremio.Offer{"movie/tt0133093": {
		{Addon: "AIOStreams", Stream: stremio.Stream{Name: "1080p", Title: "The Matrix 1080p x264 💾 1.5 GB", URL: proxy.URL + "/stream/m.mkv"}},
	}}}
	sf := newSyncFixture(t, func(d *Deps) {
		d.Streams = fs
		d.Resolver = &resolve.Resolver{Cache: d.Cache, Log: testutil.Discard(),
			Providers: map[provider.Name]provider.Provider{provider.RealDebrid: &fakeAccount{cdn: proxy.URL}}}
		d.Settings = settings.New(d.Config, d.Queries, make([]byte, 32), testutil.Discard())
	})
	sf.syncAll(t)
	admin := `MediaBrowser Token="` + captureToken + `"`
	matrix := sf.children(t, sf.views(t)["Popular Movies"].Id)[0]
	if rec := call(t, sf.h, "POST", "/Items/"+matrix.Id+"/PlaybackInfo", admin, `{}`); rec.Code != 200 {
		t.Fatalf("PlaybackInfo: %d", rec.Code)
	}
	stream := func() *httptest.ResponseRecorder {
		return call(t, sf.h, "GET", "/Videos/"+matrix.Id+"/stream?static=true", "", "")
	}
	if rec := stream(); rec.Code != 200 || rec.Header().Get("Location") != "" {
		t.Fatalf("before: %d %q, want proxied", rec.Code, rec.Header().Get("Location"))
	}

	if rec := call(t, sf.h, "POST", "/blockbustr/settings", admin, `{"stremio.redirect_hosts":["127.0.0.1"],"metadata.tmdb_api_key":"k"}`); rec.Code != 204 {
		t.Fatalf("set: %d %s", rec.Code, rec.Body)
	}
	if rec := stream(); rec.Code != 302 {
		t.Errorf("after: %d, want a redirect to the proxy", rec.Code)
	}
	var views []settings.View
	if err := json.Unmarshal(call(t, sf.h, "GET", "/blockbustr/settings", admin, "").Body.Bytes(), &views); err != nil || len(views) != len(settings.Fields) {
		t.Fatalf("list: %v %d", err, len(views))
	}
	for _, v := range views {
		if v.Key == "metadata.tmdb_api_key" && (!v.IsSet || v.Value != nil) {
			t.Errorf("secret listed: %+v", v)
		}
	}
	for body, want := range map[string]int{
		`{"stremio.streams.uhd_slots":99}`: 400,
		`{"nope":1}`:                       400,
		`{}`:                               400,
	} {
		if rec := call(t, sf.h, "POST", "/blockbustr/settings", admin, body); rec.Code != want {
			t.Errorf("%s: %d, want %d", body, rec.Code, want)
		}
	}
	if rec := call(t, sf.h, "GET", "/blockbustr/settings", "", ""); rec.Code != 401 {
		t.Errorf("anonymous: %d", rec.Code)
	}
}
