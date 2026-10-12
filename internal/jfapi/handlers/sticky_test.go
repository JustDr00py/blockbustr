package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sysadmin/blockbustr/internal/resolve"
	"github.com/sysadmin/blockbustr/internal/stremio"
	"github.com/sysadmin/blockbustr/internal/testutil"
)

// A series keeps to the release an episode was watched in (5 minutes or
// more): the next episode's default is that release's version over a
// higher resolution from another group. A shorter play doesn't move it;
// watching another release's version as long does.
func TestSeriesKeepsRelease(t *testing.T) {
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { serveVideo(w, "x") }))
	t.Cleanup(cdn.Close)
	offers := map[string][]stremio.Offer{}
	for ep := 1; ep <= 3; ep++ {
		id := fmt.Sprintf("tt0944947:1:%d", ep)
		for _, v := range []struct{ height, group string }{{"2160p", "GRPY"}, {"1080p", "GRPX"}, {"720p", "GRPZ"}} {
			offers["series/"+id] = append(offers["series/"+id], stremio.Offer{Addon: "AIO", Cached: true, Stream: stremio.Stream{
				Name: "AIO " + v.height, URL: cdn.URL + "/" + id + "/" + v.group + ".mkv",
				Hints: stremio.StreamHints{
					Filename:   fmt.Sprintf("Game.of.Thrones.S01E%02d.%s.WEB-DL-%s.mkv", ep, v.height, v.group),
					BingeGroup: "aio|" + v.height + "|" + v.group,
				},
			}})
		}
	}
	sf := newSyncFixture(t, func(d *Deps) {
		d.Streams = &fakeStreams{offers: offers}
		d.Resolver = &resolve.Resolver{Cache: d.Cache, Log: testutil.Discard()}
	})
	sf.syncAll(t)
	auth := `MediaBrowser Token="` + captureToken + `"`
	var series string
	for _, v := range sf.views(t) {
		if v.CollectionType != "tvshows" {
			continue
		}
		for _, it := range sf.children(t, v.Id) {
			if it.Name == "Game of Thrones" {
				series = it.Id
			}
		}
	}
	var seasons struct{ Items []viewItem }
	getJSON(t, sf.h, "/Shows/"+series+"/Seasons", &seasons)
	var season1 string
	for _, s := range seasons.Items {
		if s.Name == "Season 1" {
			season1 = s.Id
		}
	}
	var eps struct{ Items []viewItem }
	getJSON(t, sf.h, "/Shows/"+series+"/Episodes?seasonId="+season1, &eps)
	if len(eps.Items) < 3 {
		t.Fatalf("episodes: %v", eps.Items)
	}
	type source struct{ Id, Name string }
	versions := func(ep int) []source {
		t.Helper()
		rec := call(t, sf.h, "POST", "/Items/"+eps.Items[ep-1].Id+"/PlaybackInfo", auth, `{}`)
		var out struct{ MediaSources []source }
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || rec.Code != 200 || len(out.MediaSources) != 3 {
			t.Fatalf("E%d PlaybackInfo: %d %s", ep, rec.Code, rec.Body)
		}
		return out.MediaSources
	}
	height := func(s source) string {
		for _, h := range []string{"2160p", "1080p", "720p"} {
			if strings.Contains(s.Name, h) {
				return h
			}
		}
		return s.Name
	}
	// watch plays the version of episode ep at height for minutes.
	watch := func(ep int, h string, minutes int64) {
		t.Helper()
		var pick string
		for _, s := range versions(ep) {
			if height(s) == h {
				pick = s.Id
			}
		}
		item := eps.Items[ep-1].Id
		ps := fmt.Sprintf("ps-%d-%s", ep, h)
		report := func(path string, pos int64) {
			t.Helper()
			body := fmt.Sprintf(`{"ItemId":%q,"MediaSourceId":%q,"PlaySessionId":%q,"PositionTicks":%d}`, item, pick, ps, pos)
			if rec := call(t, sf.h, "POST", path, auth, body); rec.Code != 204 {
				t.Fatalf("%s: %d", path, rec.Code)
			}
		}
		report("/Sessions/Playing", 0)
		report("/Sessions/Playing/Progress", minutes*60*10_000_000)
	}
	defaultOf := func(ep int) string { t.Helper(); return height(versions(ep)[0]) }

	if got := defaultOf(1); got != "2160p" {
		t.Fatalf("E1 default: %s, want the 2160p (nothing watched yet)", got)
	}
	watch(1, "1080p", 6) // GRPX, picked over the default
	if got := defaultOf(2); got != "1080p" {
		t.Errorf("E2 default after E1 in GRPX: %s, want GRPX's 1080p", got)
	}
	// The details' first version follows too (clients that play it).
	var details struct{ MediaSources []source }
	getJSON(t, sf.h, "/Items/"+eps.Items[1].Id, &details)
	if len(details.MediaSources) == 0 || height(details.MediaSources[0]) != "1080p" {
		t.Errorf("E2 details: %v, want GRPX's 1080p first", details.MediaSources)
	}
	watch(2, "2160p", 2) // too short to move the series
	if got := defaultOf(3); got != "1080p" {
		t.Errorf("E3 default after 2 minutes of E2 in GRPY: %s, want still GRPX's 1080p", got)
	}
	watch(2, "720p", 6) // E2 again, switched to GRPZ
	if got := defaultOf(3); got != "720p" {
		t.Errorf("E3 default after E2 in GRPZ: %s, want GRPZ's 720p", got)
	}
}

func TestStickyFirst(t *testing.T) {
	c := func(name, group, binge string, ready bool) StreamChoice {
		return StreamChoice{Name: name, Ready: ready, Release: stremio.Release{Group: group, BingeGroup: binge}}
	}
	names := func(cs []StreamChoice) string {
		var out []string
		for _, x := range cs {
			out = append(out, x.Name)
		}
		return strings.Join(out, ",")
	}
	y, x, x720 := c("y", "GRPY", "b|y", true), c("x", "GRPX", "b|x", true), c("x720", "grpx", "", true)
	for _, tc := range []struct {
		in   []StreamChoice
		rel  stremio.Release
		want string
	}{
		{[]StreamChoice{y, x}, stremio.Release{Group: "GRPX", BingeGroup: "b|x"}, "x,y"},
		{[]StreamChoice{y, x720, x}, stremio.Release{Group: "GRPX", BingeGroup: "b|x"}, "x,y,x720"}, // the binge match first
		{[]StreamChoice{y, x720}, stremio.Release{Group: "GRPX", BingeGroup: "b|x"}, "x720,y"},      // else the group
		{[]StreamChoice{y, x}, stremio.Release{}, "y,x"},
		{[]StreamChoice{y, x}, stremio.Release{Group: "GRPZ"}, "y,x"},
		{[]StreamChoice{y, c("x", "GRPX", "", false)}, stremio.Release{Group: "GRPX"}, "y,x"}, // not ahead of a cached one
	} {
		if got := names(stickyFirst(tc.in, tc.rel)); got != tc.want {
			t.Errorf("stickyFirst(%s, %+v) = %s, want %s", names(tc.in), tc.rel, got, tc.want)
		}
	}
}
