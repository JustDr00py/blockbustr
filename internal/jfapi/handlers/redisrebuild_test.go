package handlers

import (
	"net/http"
	"strings"
	"testing"
)

// Redis only holds caches and coordination (DESIGN §5), so losing all of
// it (a restart: it has no persistence) must not sign anyone out or change
// what they see (TASKS P4.6). The writes before the loss (a favourite, a
// progress report) must survive it, and answers come back identical.
func TestRedisRebuildFromPostgres(t *testing.T) {
	h, d := newIntegrationServer(t)
	seedRecon(t, d)
	seedCaptureToken(t, d)
	authz := `MediaBrowser Client="test", Device="test", DeviceId="rebuild-1", Version="1", Token="` + captureToken + `"`

	for _, w := range []struct{ method, path, body string }{
		{"POST", "/UserFavoriteItems/" + mfGhost, ""},
		{"POST", "/Sessions/Playing", `{"ItemId":"` + fockers + `","PositionTicks":0}`},
		{"POST", "/Sessions/Playing/Progress", `{"ItemId":"` + fockers + `","PositionTicks":6000000000}`},
		{"POST", "/Sessions/Playing/Stopped", `{"ItemId":"` + fockers + `","PositionTicks":6000000000}`},
	} {
		if rec := call(t, h, w.method, w.path, authz, w.body); rec.Code >= 300 {
			t.Fatalf("%s %s = %d %s", w.method, w.path, rec.Code, rec.Body)
		}
	}

	reads := []string{
		"/Users/Me",
		"/UserViews",
		"/Items?recursive=true&includeItemTypes=Movie,Series&sortBy=SortName&fields=PrimaryImageAspectRatio&limit=50",
		"/Items?recursive=true&filters=IsFavorite&includeItemTypes=Movie,Series",
		"/Items/Latest?limit=16",
		"/UserItems/Resume?limit=12",
		"/Shows/NextUp?limit=24",
		"/Items/" + fockers,
		"/Items/" + mfGhost,
	}
	answers := func() map[string]string {
		out := map[string]string{}
		for _, p := range reads {
			rec := call(t, h, "GET", p, authz, "")
			if rec.Code != http.StatusOK {
				t.Fatalf("GET %s = %d %s", p, rec.Code, rec.Body)
			}
			out[p] = rec.Body.String()
		}
		return out
	}
	before := answers()
	for p, id := range map[string]string{reads[3]: mfGhost, reads[5]: fockers} {
		if !strings.Contains(before[p], id) {
			t.Fatalf("GET %s lacks %s (the write before it): %s", p, id, before[p])
		}
	}
	answers() // twice, so cached answers (q:*, tok:*) are the ones replaced

	if err := d.Cache.DeleteByPrefix(t.Context()); err != nil {
		t.Fatal(err)
	}
	after := answers()
	for _, p := range reads {
		if before[p] != after[p] {
			t.Errorf("GET %s changed after Redis was emptied:\nbefore %s\nafter  %s", p, before[p], after[p])
		}
	}
}
