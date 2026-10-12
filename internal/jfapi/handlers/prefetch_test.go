package handlers

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sysadmin/blockbustr/internal/cache"
	"github.com/sysadmin/blockbustr/internal/events"
	"github.com/sysadmin/blockbustr/internal/jfapi/dto"
	"github.com/sysadmin/blockbustr/internal/store/pg/db"
	"github.com/sysadmin/blockbustr/internal/stremio"
)

// coldStreams answers its first cold collections with nothing, as
// AIOStreams does a title it hasn't looked up yet, then like fakeStreams.
type coldStreams struct {
	*fakeStreams
	cold atomic.Int32
}

func (c *coldStreams) Collect(ctx context.Context, typ, id string) ([]stremio.Offer, error) {
	if c.cold.Add(-1) >= 0 {
		c.calls.Add(1)
		return nil, nil
	}
	return c.fakeStreams.Collect(ctx, typ, id)
}

// lateStreams offers nothing until offer is called.
type lateStreams struct {
	mu     sync.Mutex
	offers []stremio.Offer
	calls  atomic.Int32
}

func (l *lateStreams) offer(o []stremio.Offer) { l.mu.Lock(); l.offers = o; l.mu.Unlock() }

func (l *lateStreams) Collect(context.Context, string, string) ([]stremio.Offer, error) {
	l.calls.Add(1)
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.offers, nil
}

func (l *lateStreams) Subtitles(context.Context, string, string) ([]stremio.SubtitleOffer, error) {
	return nil, nil
}

// slowSubtitles answers subtitles after delay.
type slowSubtitles struct {
	*fakeStreams
	delay time.Duration
}

func (s slowSubtitles) Subtitles(ctx context.Context, typ, id string) ([]stremio.SubtitleOffer, error) {
	time.Sleep(s.delay)
	return s.fakeStreams.Subtitles(ctx, typ, id)
}

// countedStreams tracks how many collections run at once.
type countedStreams struct {
	*fakeStreams
	delay         time.Duration
	inFlight, max atomic.Int32
}

func (c *countedStreams) Collect(ctx context.Context, typ, id string) ([]stremio.Offer, error) {
	n := c.inFlight.Add(1)
	defer c.inFlight.Add(-1)
	for {
		m := c.max.Load()
		if n <= m || c.max.CompareAndSwap(m, n) {
			break
		}
	}
	time.Sleep(c.delay)
	return c.fakeStreams.Collect(ctx, typ, id)
}

type detailSources struct {
	MediaSources []struct {
		Name         string
		MediaStreams []struct{ Type string }
	}
}

// versions are the addon versions listed (not the placeholder).
func (d detailSources) versions() []string {
	var out []string
	for _, s := range d.MediaSources {
		if strings.HasPrefix(s.Name, "1080p") {
			out = append(out, s.Name)
		}
	}
	return out
}

func matrixOffer() []stremio.Offer {
	return []stremio.Offer{{Addon: "AIOStreams", Stream: stremio.Stream{Name: "1080p", URL: "https://cdn.example/m.mkv"}}}
}

func waitFor(t *testing.T, what string, within time.Duration, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// AIOStreams answers a title it hasn't looked up with nothing, then has
// streams a moment later (Grandma's Boy, 2026-10-12). The first details
// answer waits through the retry and lists them: Streamyfin never
// refetches on ItemsUpdated. The event still goes out for apps that do.
func TestDetailsRetryEmptyFirstCollection(t *testing.T) {
	fs := &coldStreams{fakeStreams: &fakeStreams{offers: map[string][]stremio.Offer{"movie/tt0133093": matrixOffer()}}}
	fs.cold.Store(1)
	var bus *events.Bus
	sf := newSyncFixture(t, func(d *Deps) {
		d.Streams = fs
		bus = &events.Bus{Cache: d.Cache}
		d.Events = bus
	})
	sf.syncAll(t)
	matrix := sf.children(t, sf.views(t)["Popular Movies"].Id)[0]
	id, err := dto.ParseID(matrix.Id)
	if err != nil {
		t.Fatal(err)
	}
	evs, err := bus.Subscribe(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	var detail detailSources
	getJSON(t, sf.h, "/Items/"+matrix.Id, &detail)
	took := time.Since(start)
	if v := detail.versions(); len(v) != 1 || took < prefetchRetries[0] || took > prefetchRetries[0]+time.Second {
		t.Fatalf("first view: versions %q after %s, want the 1080p after ~%s", v, took, prefetchRetries[0])
	}
	if n := fs.calls.Load(); n != 2 {
		t.Errorf("collections: %d, want 2", n)
	}
	deadline := time.After(3 * time.Second)
	for {
		select {
		case e := <-evs:
			if e.Kind == events.LibraryChanged && len(e.Updated) == 1 && e.Updated[0] == id.UUID() {
				return
			}
		case <-deadline:
			t.Fatal("no ItemsUpdated after the retry found versions")
		}
	}
}

// A title with no streams anywhere: the first details answer comes when
// the retries are over (within detailsCap), and the next opens don't wait,
// thanks to the marker. A later collection that finds versions clears it.
func TestDetailsNoStreamsRemembered(t *testing.T) {
	recheck := prefetchRecheck
	prefetchRecheck = 300 * time.Millisecond
	t.Cleanup(func() { prefetchRecheck = recheck })
	ls := &lateStreams{}
	var rc *cache.Cache
	sf := newSyncFixture(t, func(d *Deps) { d.Streams = ls; rc = d.Cache })
	sf.syncAll(t)
	matrix := sf.children(t, sf.views(t)["Popular Movies"].Id)[0]
	id, err := dto.ParseID(matrix.Id)
	if err != nil {
		t.Fatal(err)
	}
	marked := func() bool {
		var v bool
		ok, err := rc.GetJSON(t.Context(), cache.StreamNoneKey(id.UUID().String()), &v)
		return err == nil && ok
	}

	start := time.Now()
	var detail detailSources
	getJSON(t, sf.h, "/Items/"+matrix.Id, &detail)
	last := prefetchRetries[len(prefetchRetries)-1]
	if took := time.Since(start); len(detail.versions()) != 0 || took < last || took > detailsCap+500*time.Millisecond {
		t.Fatalf("first view: %+v after %s, want no versions after %s–%s", detail, took, last, detailsCap)
	}
	if !marked() {
		t.Fatal("no marker after the retries found nothing")
	}
	waitFor(t, "the background recheck", 3*time.Second, func() bool { return ls.calls.Load() == int32(2+len(prefetchRetries)) })

	// Streams turn up: the next open answers at once (the marker), but
	// rechecks in the background, which finds them and clears it.
	ls.offer(matrixOffer())
	time.Sleep(100 * time.Millisecond) // the first lookup lets go of its lock
	start = time.Now()
	getJSON(t, sf.h, "/Items/"+matrix.Id, &detail)
	if took := time.Since(start); len(detail.versions()) != 0 || took > 500*time.Millisecond {
		t.Errorf("marked view: %+v after %s, want an answer at once", detail, took)
	}
	waitFor(t, "the recheck to clear the marker", 3*time.Second, func() bool { return !marked() })
	getJSON(t, sf.h, "/Items/"+matrix.Id, &detail)
	if v := detail.versions(); len(v) != 1 {
		t.Errorf("after the recheck: versions %q", v)
	}
}

// Subtitles slower than the details' wait (AIOStreams' often hit the 6 s
// addon timeout) don't hold the versions up; they're added when they come.
func TestDetailsDontWaitForSubtitles(t *testing.T) {
	fs := &fakeStreams{
		offers: map[string][]stremio.Offer{"movie/tt0133093": matrixOffer()},
		subs: map[string][]stremio.SubtitleOffer{"movie/tt0133093": {
			{Addon: "OpenSubtitles v3", Subtitle: stremio.Subtitle{Lang: "eng", URL: "https://subs.example/en1"}},
		}},
	}
	sf := newSyncFixture(t, func(d *Deps) {
		d.Streams = slowSubtitles{fs, 3 * time.Second}
		d.Config.Stremio.Subtitles.Languages = []string{"en"}
	})
	sf.syncAll(t)
	matrix := sf.children(t, sf.views(t)["Popular Movies"].Id)[0]
	subtitles := func(d detailSources) int {
		n := 0
		for _, s := range d.MediaSources {
			for _, st := range s.MediaStreams {
				if st.Type == "Subtitle" {
					n++
				}
			}
		}
		return n
	}

	start := time.Now()
	var detail detailSources
	getJSON(t, sf.h, "/Items/"+matrix.Id, &detail)
	if took := time.Since(start); len(detail.versions()) != 1 || subtitles(detail) != 0 || took > time.Second {
		t.Fatalf("first view: %+v after %s, want the version, without subtitles, at once", detail, took)
	}
	waitFor(t, "the subtitles", 5*time.Second, func() bool {
		getJSON(t, sf.h, "/Items/"+matrix.Id, &detail)
		return subtitles(detail) == 1
	})
	if len(detail.versions()) != 1 {
		t.Errorf("versions after the subtitles: %+v", detail)
	}
}

// searchFixture is a sync fixture whose remote search finds its catalog
// titles: the movies, then the series.
func searchFixture(t *testing.T, streams StreamCollector) (*syncFixture, *fakeRemote) {
	t.Helper()
	remote := &fakeRemote{}
	var q *db.Queries
	sf := newSyncFixture(t, func(d *Deps) { d.Streams = streams; d.RemoteSearch = remote; q = d.Queries })
	sf.syncAll(t)
	var ids []uuid.UUID
	for _, view := range []string{"Popular Movies", "Popular Shows"} {
		for _, c := range sf.children(t, sf.views(t)[view].Id) {
			id, err := dto.ParseID(c.Id)
			if err != nil {
				t.Fatal(err)
			}
			ids = append(ids, id.UUID())
		}
	}
	items, err := q.GetItemsByIDs(t.Context(), ids)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[uuid.UUID]db.Item{}
	for _, it := range items {
		byID[it.ID] = it
	}
	for _, id := range ids {
		remote.items = append(remote.items, byID[id])
	}
	return sf, remote
}

// A search starts its titles' collections; opening one then joins the
// collection in flight instead of starting another.
func TestSearchPrefetchJoinedByDetails(t *testing.T) {
	fs := &fakeStreams{offers: map[string][]stremio.Offer{"movie/tt0133093": matrixOffer()}}
	sf, remote := searchFixture(t, slowStreams{fs, 2 * time.Second})
	remote.items = remote.items[:1] // The Matrix
	var res struct{ Items []struct{ Name string } }
	getJSON(t, sf.h, "/Items?searchTerm=matrix&recursive=true&includeItemTypes=Movie,Series&limit=10", &res)
	time.Sleep(500 * time.Millisecond) // the search's collection is under way

	// A collection of its own would take the addon's whole 2 s.
	start := time.Now()
	var detail detailSources
	getJSON(t, sf.h, "/Items/"+dto.IDFromUUID(remote.items[0].ID).String(), &detail)
	if took := time.Since(start); len(detail.versions()) != 1 || took > 1800*time.Millisecond {
		t.Errorf("details: %+v after %s, want the version from the search's collection", detail, took)
	}
	if n := fs.calls.Load(); n != 1 {
		t.Errorf("collections: %d, want 1", n)
	}
}

// A search collects only its first titles, and no more at once than the
// prefetch semaphore (2) allows.
func TestSearchPrefetchBounded(t *testing.T) {
	offers := map[string][]stremio.Offer{}
	for _, id := range []string{"tt0133093", "tt0234215", "tt10838180"} {
		offers["movie/"+id] = matrixOffer()
	}
	cs := &countedStreams{fakeStreams: &fakeStreams{offers: offers}, delay: 500 * time.Millisecond}
	sf, remote := searchFixture(t, cs)
	if len(remote.items) <= searchPrefetch {
		t.Fatalf("search results: %d, want more than %d", len(remote.items), searchPrefetch)
	}
	var res struct{ Items []struct{ Name string } }
	getJSON(t, sf.h, "/Items?searchTerm=the&recursive=true&includeItemTypes=Movie,Series&limit=20", &res)
	waitFor(t, "the search's collections", 5*time.Second, func() bool { return cs.calls.Load() == searchPrefetch })
	time.Sleep(time.Second)
	if n := cs.calls.Load(); n != searchPrefetch {
		t.Errorf("collections: %d, want %d", n, searchPrefetch)
	}
	if m := cs.max.Load(); m != 2 {
		t.Errorf("collections at once: %d, want the semaphore's 2", m)
	}
	cs.mu.Lock()
	defer cs.mu.Unlock()
	for _, a := range cs.asked {
		if strings.HasPrefix(a, "streams series/") {
			t.Errorf("collected past the first %d: %s", searchPrefetch, a)
		}
	}
}

// A series found by search gets the episode to be opened collected: S1E1
// for a viewer who hasn't started it, else their next one (Next Up's).
func TestSearchPrefetchSeriesEpisode(t *testing.T) {
	fs := &fakeStreams{offers: map[string][]stremio.Offer{
		"series/tt0944947:1:1": matrixOffer(), "series/tt0944947:0:1": matrixOffer(),
	}}
	sf, remote := searchFixture(t, fs)
	var series db.Item
	for _, it := range remote.items {
		if it.Type == "Series" {
			series = it
		}
	}
	if series.ID == uuid.Nil {
		t.Fatal("no series in the results")
	}
	remote.items = []db.Item{series}
	search := func() {
		var res struct{ Items []struct{ Name string } }
		getJSON(t, sf.h, "/Items?searchTerm=thrones&recursive=true&includeItemTypes=Movie,Series&limit=10", &res)
	}
	asked := func(want string) func() bool {
		return func() bool {
			fs.mu.Lock()
			defer fs.mu.Unlock()
			for _, a := range fs.asked {
				if a == want {
					return true
				}
			}
			return false
		}
	}

	search()
	waitFor(t, "S1E1's collection", 3*time.Second, asked("streams series/tt0944947:1:1"))

	var e1 string
	var eps struct{ Items []viewItem }
	getJSON(t, sf.h, "/Shows/"+dto.IDFromUUID(series.ID).String()+"/Episodes", &eps)
	for _, ep := range eps.Items {
		if ep.ParentIndexNumber == 1 && ep.IndexNumber == 1 {
			e1 = ep.Id
		}
	}
	if e1 == "" {
		t.Fatal("no S1E1")
	}
	if rec := call(t, sf.h, "POST", "/UserPlayedItems/"+e1, `MediaBrowser Token="`+captureToken+`"`, ""); rec.Code != 200 {
		t.Fatalf("mark played: %d %s", rec.Code, rec.Body)
	}
	search()
	// The next episode is the one Next Up shows (a special, here).
	var next struct{ Items []viewItem }
	getJSON(t, sf.h, "/Shows/NextUp?SeriesId="+dto.IDFromUUID(series.ID).String(), &next)
	if len(next.Items) != 1 || next.Items[0].Id == e1 {
		t.Fatalf("next up: %+v", next.Items)
	}
	want := fmt.Sprintf("streams series/tt0944947:%d:%d", next.Items[0].ParentIndexNumber, next.Items[0].IndexNumber)
	waitFor(t, "the next episode's collection", 3*time.Second, asked(want))
}

// A title with its versions remembered answers at once, collecting nothing.
func TestDetailsStoredSetUnchanged(t *testing.T) {
	fs := &fakeStreams{offers: map[string][]stremio.Offer{"movie/tt0133093": matrixOffer()}}
	sf := newSyncFixture(t, func(d *Deps) { d.Streams = fs })
	sf.syncAll(t)
	matrix := sf.children(t, sf.views(t)["Popular Movies"].Id)[0]
	var detail detailSources
	getJSON(t, sf.h, "/Items/"+matrix.Id, &detail)
	if len(detail.versions()) != 1 {
		t.Fatalf("first view: %+v", detail)
	}
	calls := fs.calls.Load()
	start := time.Now()
	getJSON(t, sf.h, "/Items/"+matrix.Id, &detail)
	if took := time.Since(start); len(detail.versions()) != 1 || took > 300*time.Millisecond || fs.calls.Load() != calls {
		t.Errorf("stored set: %+v after %s, %d collections", detail, took, fs.calls.Load()-calls)
	}
}
