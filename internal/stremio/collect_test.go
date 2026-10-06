package stremio

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sysadmin/blockbustr/internal/provider"
	"github.com/sysadmin/blockbustr/internal/store/pg/db"
)

// streamAddon serves streams for every id; delay slows it down.
func streamAddon(t *testing.T, streams []Stream, delay time.Duration, hits *atomic.Int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits != nil {
			hits.Add(1)
		}
		if !strings.HasPrefix(r.URL.Path, "/stream/") {
			http.NotFound(w, r)
			return
		}
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"streams": streams})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func testAddon(name, base string, priority int, m Manifest) addonInfo {
	m.Name = name
	return addonInfo{row: db.StremioAddon{ID: uuid.New(), Host: name, Priority: int32(priority)}, base: base, manifest: m}
}

var streamManifest = Manifest{Types: []string{"movie", "series"}, IDPrefixes: []string{"tt"}, Resources: []Resource{{Name: "stream"}}}

type fakeDebrid struct {
	provider.Provider
	cached []string
	err    error
	asked  atomic.Int32
}

func (f *fakeDebrid) Name() provider.Name { return "fake" }

func (f *fakeDebrid) InstantCheck(_ context.Context, hashes []string) ([]provider.InstantResult, error) {
	f.asked.Add(1)
	var out []provider.InstantResult
	for _, h := range hashes {
		for _, c := range f.cached {
			if c == h {
				out = append(out, provider.InstantResult{Hash: h, Cached: true})
			}
		}
	}
	return out, f.err
}

func testCollector(addons ...addonInfo) *Collector {
	return &Collector{
		Registry: &Registry{Client: &Client{}},
		Timeout:  300 * time.Millisecond,
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		addons:   func(context.Context) ([]addonInfo, error) { return addons, nil },
	}
}

func TestCollect(t *testing.T) {
	var otherHits atomic.Int32
	first := streamAddon(t, []Stream{{Name: "first 1", InfoHash: "aaa"}, {Name: "first 2", InfoHash: "bbb"}}, 50*time.Millisecond, nil)
	second := streamAddon(t, []Stream{{Name: "second", URL: "https://cdn.example/x.mkv"}}, 0, nil)
	slow := streamAddon(t, []Stream{{Name: "slow", InfoHash: "ccc"}}, 5*time.Second, nil)
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) }))
	t.Cleanup(broken.Close)
	other := streamAddon(t, []Stream{{Name: "anime", InfoHash: "ddd"}}, 0, &otherHits)

	animeOnly := Manifest{Types: []string{"movie"}, IDPrefixes: []string{"kitsu:"}, Resources: []Resource{{Name: "stream"}}}
	catalogOnly := Manifest{Types: []string{"movie"}, Resources: []Resource{{Name: "catalog"}}}
	c := testCollector(
		testAddon("First", first.URL, 10, streamManifest),
		testAddon("Slow", slow.URL, 9, streamManifest),
		testAddon("Second", second.URL, 5, streamManifest),
		testAddon("Broken", broken.URL, 4, streamManifest),
		testAddon("Anime", other.URL, 3, animeOnly),
		testAddon("Catalog", other.URL, 2, catalogOnly),
	)
	debrid := &fakeDebrid{cached: []string{"bbb"}}
	c.Debrid = []provider.Provider{debrid}

	start := time.Now()
	got, err := c.Collect(context.Background(), "movie", "tt0133093")
	if err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("Collect took %s; the slow addon should have timed out", took)
	}
	var names []string
	for _, o := range got {
		names = append(names, o.Addon+":"+o.Stream.Name)
	}
	want := "First:first 1,First:first 2,Second:second"
	if strings.Join(names, ",") != want {
		t.Errorf("offers = %v, want %s (priority order, failing addons left out)", names, want)
	}
	if otherHits.Load() != 0 {
		t.Error("addons that don't serve this type/id prefix were asked")
	}
	if len(got) == 3 {
		if got[0].Cached || !got[1].Cached || got[2].Cached {
			t.Errorf("cached = %v %v %v, want only bbb", got[0].Cached, got[1].Cached, got[2].Cached)
		}
		if got[0].Priority != 10 || got[0].AddonID == uuid.Nil {
			t.Errorf("offer addon fields = %+v", got[0])
		}
	}
	if debrid.asked.Load() != 1 {
		t.Errorf("debrid asked %d times, want once", debrid.asked.Load())
	}
}

func TestCollectEpisodeID(t *testing.T) {
	var path atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path.Store(r.URL.Path)
		_, _ = io.WriteString(w, `{"streams":[]}`)
	}))
	t.Cleanup(srv.Close)
	c := testCollector(testAddon("A", srv.URL, 0, streamManifest))
	got, err := c.Collect(context.Background(), "series", "tt0944947:1:2")
	if err != nil || len(got) != 0 {
		t.Fatalf("Collect = %v, %v", got, err)
	}
	if p, _ := path.Load().(string); p != "/stream/series/tt0944947:1:2.json" {
		t.Errorf("path = %q", p)
	}
}

func TestCollectDebridFailureIgnored(t *testing.T) {
	srv := streamAddon(t, []Stream{{Name: "x", InfoHash: "aaa"}, {Name: "y", URL: "https://cdn.example/y"}}, 0, nil)
	c := testCollector(testAddon("A", srv.URL, 0, streamManifest))
	failing := &fakeDebrid{cached: []string{"aaa"}, err: context.DeadlineExceeded}
	c.Debrid = []provider.Provider{failing}
	got, err := c.Collect(context.Background(), "movie", "tt1")
	if err != nil || len(got) != 2 {
		t.Fatalf("Collect = %v, %v", got, err)
	}
	if got[0].Cached {
		t.Error("a failed check marked an offer cached")
	}
}

func TestCollectNoTorrentsSkipsDebrid(t *testing.T) {
	srv := streamAddon(t, []Stream{{Name: "y", URL: "https://cdn.example/y"}}, 0, nil)
	c := testCollector(testAddon("A", srv.URL, 0, streamManifest))
	d := &fakeDebrid{}
	c.Debrid = []provider.Provider{d}
	if _, err := c.Collect(context.Background(), "movie", "tt1"); err != nil {
		t.Fatal(err)
	}
	if d.asked.Load() != 0 {
		t.Error("debrid asked about direct URLs")
	}
}

func TestCollectKnownTorrents(t *testing.T) {
	srv := streamAddon(t, []Stream{{Name: "x", InfoHash: "aaa"}, {Name: "y", InfoHash: "bbb"}}, 0, nil)
	c := testCollector(testAddon("A", srv.URL, 0, streamManifest))
	var asked []string
	c.Known = func(_ context.Context, hashes []string) map[string]bool {
		asked = hashes
		return map[string]bool{"bbb": true}
	}
	got, err := c.Collect(context.Background(), "movie", "tt1")
	if err != nil || len(got) != 2 {
		t.Fatalf("Collect = %v, %v", got, err)
	}
	if got[0].Cached || !got[1].Cached || len(asked) != 2 {
		t.Errorf("cached = %v %v, asked %v", got[0].Cached, got[1].Cached, asked)
	}
}

func TestCollectSubtitles(t *testing.T) {
	subs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/subtitles/series/tt0944947:1:2.json" {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, `{"subtitles":[{"id":"1","url":"https://subs.example/1","lang":"eng"},{"id":"2","url":"","lang":"eng"},{"id":"3","url":"https://subs.example/3","lang":"pob"}]}`)
	}))
	t.Cleanup(subs.Close)
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(500) }))
	t.Cleanup(broken.Close)
	var streamHits atomic.Int32
	streamsOnly := streamAddon(t, nil, 0, &streamHits)
	subManifest := Manifest{Types: []string{"movie", "series"}, IDPrefixes: []string{"tt"}, Resources: []Resource{{Name: "subtitles"}}}
	c := testCollector(
		testAddon("OpenSubtitles", subs.URL, 5, subManifest),
		testAddon("Broken", broken.URL, 4, subManifest),
		testAddon("Torrentio", streamsOnly.URL, 3, streamManifest),
	)
	got, err := c.Subtitles(context.Background(), "series", "tt0944947:1:2")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Subtitle.ID != "1" || got[1].Subtitle.Lang != "pob" || got[0].Addon != "OpenSubtitles" {
		t.Errorf("subtitles = %+v (entries without a url dropped)", got)
	}
	if streamHits.Load() != 0 {
		t.Error("a stream-only addon was asked for subtitles")
	}
}
