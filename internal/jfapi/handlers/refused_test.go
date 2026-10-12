package handlers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sysadmin/blockbustr/internal/cache"
	"github.com/sysadmin/blockbustr/internal/jfapi/dto"
	"github.com/sysadmin/blockbustr/internal/resolve"
	"github.com/sysadmin/blockbustr/internal/stremio"
	"github.com/sysadmin/blockbustr/internal/testutil"
)

// lockedBuffer is a log destination background goroutines can share.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// refusalFixture is a catalog movie whose versions resolve through cdn.
type refusalFixture struct {
	sf     *syncFixture
	cache  *cache.Cache
	item   string // the movie's id
	prober *garbageProber
	logs   *lockedBuffer
}

func newRefusalFixture(t *testing.T, cdn *httptest.Server, paths ...string) *refusalFixture {
	t.Helper()
	var offers []stremio.Offer
	for i, p := range paths {
		height := []string{"2160p", "1080p", "720p", "576p", "480p"}[min(i, 4)]
		offers = append(offers, stremio.Offer{Addon: "AIO", Stream: stremio.Stream{
			Name: "⚡ " + height, URL: cdn.URL + p, Hints: stremio.StreamHints{Filename: strings.TrimPrefix(p, "/")},
		}})
	}
	f := &refusalFixture{prober: &garbageProber{calls: map[string]int{}}, logs: &lockedBuffer{}}
	f.sf = newSyncFixture(t, func(d *Deps) {
		d.Streams = &fakeStreams{offers: map[string][]stremio.Offer{"movie/tt0133093": offers}}
		d.ChoiceProber = f.prober
		d.Resolver = &resolve.Resolver{Cache: d.Cache, Log: testutil.Discard()}
		d.Log = slog.New(slog.NewTextHandler(f.logs, nil))
		f.cache = d.Cache
	})
	f.sf.syncAll(t)
	f.item = f.sf.children(t, f.sf.views(t)["Popular Movies"].Id)[0].Id
	return f
}

// choice is the id of the version at path.
func (f *refusalFixture) choice(t *testing.T, cdn *httptest.Server, path string) uuid.UUID {
	t.Helper()
	id, err := dto.ParseID(f.item)
	if err != nil {
		t.Fatal(err)
	}
	return uuid.NewSHA1(id.UUID(), []byte("url:"+cdn.URL+path))
}

type offered struct {
	MediaSources  []struct{ Id, Name string }
	PlaySessionId string
}

func (f *refusalFixture) playbackInfo(t *testing.T, body string) offered {
	t.Helper()
	rec := call(t, f.sf.h, "POST", "/Items/"+f.item+"/PlaybackInfo", `MediaBrowser Token="`+captureToken+`"`, body)
	var out offered
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || rec.Code != 200 {
		t.Fatalf("PlaybackInfo: %d %s", rec.Code, rec.Body)
	}
	return out
}

func (f *refusalFixture) waitProbes(t *testing.T, n int) {
	t.Helper()
	for deadline := time.Now().Add(15 * time.Second); f.prober.total() < n && time.Now().Before(deadline); {
		time.Sleep(20 * time.Millisecond)
	}
}

func (f *refusalFixture) probeFailedMark(t *testing.T, id uuid.UUID) bool {
	t.Helper()
	var v bool
	ok, err := f.cache.GetJSON(t.Context(), choiceProbeFailKey(id), &v)
	return err == nil && ok
}

// Production: AIOStreams answered 6 of 7 resolves fired at once with 400,
// and the one version probed went ahead of the one the user picked. Probes
// now send an addon host at most probeHostParallel resolves at a time:
// against a host that refuses more, every version is probed.
func TestProbesSpareTheAddonHost(t *testing.T) {
	var inflight, most atomic.Int32
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := inflight.Add(1)
		defer inflight.Add(-1)
		for m := most.Load(); n > m && !most.CompareAndSwap(m, n); m = most.Load() {
		}
		time.Sleep(30 * time.Millisecond)
		if n > probeHostParallel {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		serveVideo(w, "x")
	}))
	t.Cleanup(cdn.Close)
	var paths []string
	for i := 1; i <= 7; i++ {
		paths = append(paths, fmt.Sprintf("/v%d.mkv", i))
	}
	f := newRefusalFixture(t, cdn, paths...)
	if got := len(f.playbackInfo(t, `{}`).MediaSources); got != 7 {
		t.Fatalf("%d versions offered, want 7", got)
	}
	f.waitProbes(t, 7)
	if got := f.prober.total(); got != 7 {
		t.Errorf("%d of 7 versions probed", got)
	}
	if most.Load() > probeHostParallel {
		t.Errorf("%d resolves at once on one host, want at most %d", most.Load(), probeHostParallel)
	}
	for _, p := range paths {
		if f.probeFailedMark(t, f.choice(t, cdn, p)) {
			t.Errorf("%s marked as a failed probe", p)
		}
	}
}

// A refusal that may pass is asked again once: one that passes leaves no
// mark; one that lasts marks the probe failed but the version stays
// offered (it isn't known bad).
func TestPassingRefusalRetried(t *testing.T) {
	var flakyHits atomic.Int32
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/flaky.mkv":
			if flakyHits.Add(1) == 1 {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
		case "/refused.mkv":
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		serveVideo(w, "x")
	}))
	t.Cleanup(cdn.Close)
	f := newRefusalFixture(t, cdn, "/flaky.mkv", "/refused.mkv", "/fine.mkv")
	f.playbackInfo(t, `{}`)
	f.waitProbes(t, 2)
	time.Sleep(3 * refusedRetry) // the refused one's retry
	f.prober.mu.Lock()
	flaky := f.prober.calls["/flaky.mkv"]
	f.prober.mu.Unlock()
	if flaky != 1 || f.probeFailedMark(t, f.choice(t, cdn, "/flaky.mkv")) {
		t.Errorf("flaky: probed %d times, failed mark %v; want probed after a retry, no mark", flaky, f.probeFailedMark(t, f.choice(t, cdn, "/flaky.mkv")))
	}
	refused := f.choice(t, cdn, "/refused.mkv")
	if !f.probeFailedMark(t, refused) {
		t.Error("lasting 400: no failed-probe mark")
	}
	itemID, _ := dto.ParseID(f.item)
	if bad := (&api{Deps: Deps{Cache: f.cache}}).badChoices(t.Context(), itemID.UUID()); bad[refused] {
		t.Error("a 400 marked the version bad")
	}
	if got := len(f.playbackInfo(t, `{}`).MediaSources); got != 3 {
		t.Errorf("%d versions offered after the refusals, want 3", got)
	}
}

// A version the user picks plays or fails: never swapped for another. Its
// failed-probe mark doesn't stop it being probed again, nor its rank
// putting it first by default; a default play falls back, and says so.
func TestPickedVersionHonoured(t *testing.T) {
	var aDown atomic.Bool
	var bHits atomic.Int32
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/a.mkv" && aDown.Load():
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		case r.URL.Path == "/b.mkv":
			bHits.Add(1)
		}
		serveVideo(w, "x")
	}))
	t.Cleanup(cdn.Close)
	f := newRefusalFixture(t, cdn, "/a.mkv", "/b.mkv", "/c.mkv")
	a, b := f.choice(t, cdn, "/a.mkv"), f.choice(t, cdn, "/b.mkv")
	ctx := t.Context()
	for _, id := range []uuid.UUID{a, b} { // both refused a moment ago
		if err := f.cache.SetJSON(ctx, choiceProbeFailKey(id), true, probeFailTTL); err != nil {
			t.Fatal(err)
		}
	}

	// By default the best ranked comes first though its probe failed, and
	// the session's order is the one returned.
	got := f.playbackInfo(t, `{}`)
	if len(got.MediaSources) != 3 || !strings.Contains(got.MediaSources[0].Name, "2160p") {
		t.Fatalf("default order: %+v", got.MediaSources)
	}
	var ps PlaySession
	if ok, err := f.cache.GetJSON(ctx, cache.PlaySessionKey(got.PlaySessionId), &ps); err != nil || !ok || len(ps.Order) != 3 {
		t.Fatalf("play session: %v %v %+v", ok, err, ps)
	}
	for i, ms := range got.MediaSources[1:] {
		if !sameID(ms.Id, dto.IDFromUUID(ps.Order[i+1]).String()) {
			t.Errorf("source %d: %s returned, %s in the session's order", i+1, ms.Id, ps.Order[i+1])
		}
	}
	if ps.Order[0] != a {
		t.Errorf("session's default %s, want a", ps.Order[0])
	}

	// Picked: only that version, probed again despite its mark.
	before := f.prober.total()
	picked := f.playbackInfo(t, `{"MediaSourceId":"`+dto.IDFromUUID(b).String()+`"}`)
	if len(picked.MediaSources) != 1 || !sameID(picked.MediaSources[0].Id, dto.IDFromUUID(b).String()) {
		t.Errorf("picked b, got %+v", picked.MediaSources)
	}
	f.waitProbes(t, before+1)
	f.prober.mu.Lock()
	bProbes := f.prober.calls["/b.mkv"]
	f.prober.mu.Unlock()
	if bProbes == 0 {
		t.Error("the picked version wasn't probed again")
	}

	// a fails: picked, it fails as itself (502), b isn't served.
	aDown.Store(true)
	hits := bHits.Load()
	stream := func(ms string) int {
		return call(t, f.sf.h, "GET", "/Videos/"+f.item+"/stream?static=true&MediaSourceId="+ms, "", "").Code
	}
	if code := stream(dto.IDFromUUID(a).String()); code != http.StatusBadGateway {
		t.Errorf("picked a, down: %d, want 502", code)
	}
	if bHits.Load() != hits {
		t.Error("another version was served for the picked one")
	}
	// The default falls back to the next, and says so at info.
	if code := stream(f.item); code != 200 {
		t.Errorf("default with a down: %d, want the next version", code)
	}
	if !strings.Contains(f.logs.String(), "stream fell back to another version") {
		t.Errorf("no fallback logged:\n%s", f.logs.String())
	}
	// A 503 passes: a stays offered.
	if got := len(f.playbackInfo(t, `{}`).MediaSources); got != 3 {
		t.Errorf("%d versions offered after a's 503, want 3", got)
	}
}
