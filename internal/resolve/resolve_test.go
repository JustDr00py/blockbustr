package resolve

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sysadmin/blockbustr/internal/provider"
	"github.com/sysadmin/blockbustr/internal/testutil"
)

// fakeDebrid is a provider whose FileLink is counted and scripted.
type fakeDebrid struct {
	provider.Provider // other methods unused
	calls             atomic.Int32
	link              func(torrentID, fileID string) (string, time.Time, error)
}

func (f *fakeDebrid) Name() provider.Name { return provider.RealDebrid }

func (f *fakeDebrid) FileLink(_ context.Context, torrentID, fileID string) (string, time.Time, error) {
	f.calls.Add(1)
	time.Sleep(20 * time.Millisecond) // long enough for concurrent callers to pile up
	return f.link(torrentID, fileID)
}

// origin plays a jellybird or CDN: it redirects to /cdn, which answers a
// 1-byte range of a 1000-byte file.
func origin(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.URL.Path != "/cdn/movie.mkv" {
			http.Redirect(w, r, "/cdn/movie.mkv", http.StatusFound)
			return
		}
		w.Header().Set("Content-Range", "bytes 0-0/1000")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write([]byte{0})
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func TestJellybirdTargets(t *testing.T) {
	for target, want := range map[string]*Source{
		"http://jb:8080/stream/realdebrid/ABC123/7?sig=xyz": {Kind: Debrid, Provider: provider.RealDebrid, TorrentID: "ABC123", FileID: "7"},
		"https://h.example/jellybird/stream/torbox/42/3":    {Kind: Debrid, Provider: provider.TorBox, TorrentID: "42", FileID: "3"},
		"http://jb/stream/premiumize/1/2":                   nil, // unknown provider
		"http://jb/stream/realdebrid/1":                     nil, // too short
		"http://jb/stream/realdebrid/1/2/extra":             nil, // "stream" not 4th from the end
		"http://cdn.example/d/ABCDEF/movie.mkv":             nil,
		"http://jb/streams/realdebrid/1/2":                  nil,
		"http://jb/stream/realdebrid//2":                    nil,
		"%zz":                                               nil,
	} {
		got, ok := jellybird(target)
		if (want == nil) == ok || (want != nil && got != *want) {
			t.Errorf("%s: %+v %v, want %+v", target, got, ok, want)
		}
	}
}

func TestDebridSourceCachedAndShared(t *testing.T) {
	rd := &fakeDebrid{link: func(tid, fid string) (string, time.Time, error) {
		return "https://cdn.example/d/" + tid + "/" + fid, time.Now().Add(2 * time.Hour), nil
	}}
	r := &Resolver{Cache: testutil.Cache(t), Providers: map[provider.Name]provider.Provider{provider.RealDebrid: rd}}
	src := Source{Kind: Debrid, Provider: provider.RealDebrid, TorrentID: "T1", FileID: "2"}

	// A burst of resolves (an HLS start plus the client's own range
	// requests) unrestricts once.
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			l, err := r.Resolve(t.Context(), src)
			if err != nil || l.URL != "https://cdn.example/d/T1/2" {
				t.Errorf("%+v %v", l, err)
			}
		})
	}
	wg.Wait()
	if n := rd.calls.Load(); n != 1 {
		t.Errorf("FileLink calls in a burst = %d", n)
	}
	if _, err := r.Resolve(t.Context(), src); err != nil || rd.calls.Load() != 1 {
		t.Errorf("cached: calls = %d, %v", rd.calls.Load(), err)
	}
	r.Forget(t.Context(), src)
	if _, err := r.Resolve(t.Context(), src); err != nil || rd.calls.Load() != 2 {
		t.Errorf("after Forget: calls = %d, %v", rd.calls.Load(), err)
	}

	if _, err := r.Resolve(t.Context(), Source{Kind: Debrid, Provider: provider.TorBox, TorrentID: "1", FileID: "1"}); !errors.Is(err, ErrNotResolvable) {
		t.Errorf("unconfigured provider: %v", err)
	}
}

func TestJellybirdTargetResolvedDirectly(t *testing.T) {
	jb, hits := origin(t)
	rd := &fakeDebrid{link: func(tid, fid string) (string, time.Time, error) {
		if tid != "ABC" || fid != "1" {
			t.Errorf("FileLink(%s, %s)", tid, fid)
		}
		return "https://cdn.example/direct.mkv", time.Time{}, nil
	}}
	r := &Resolver{Providers: map[provider.Name]provider.Provider{provider.RealDebrid: rd}, Log: testutil.Discard()}
	l, err := r.Resolve(t.Context(), FromURL(jb.URL+"/stream/realdebrid/ABC/1?sig=s"))
	if err != nil || l.URL != "https://cdn.example/direct.mkv" {
		t.Fatalf("%+v %v", l, err)
	}
	if hits.Load() != 0 {
		t.Errorf("jellybird was called %d times", hits.Load())
	}

	// Not on this account (jellybird uses another): jellybird is followed.
	rd.link = func(string, string) (string, time.Time, error) {
		return "", time.Time{}, errors.New("realdebrid: HTTP 404 (code 7): unknown_ressource")
	}
	l, err = r.Resolve(t.Context(), FromURL(jb.URL+"/stream/realdebrid/OTHER/1?sig=s"))
	if err != nil || l.URL != jb.URL+"/cdn/movie.mkv" || l.Size != 1000 {
		t.Errorf("fallback: %+v %v", l, err)
	}
}

func TestURLWithoutProviderIsFollowed(t *testing.T) {
	jb, hits := origin(t)
	r := &Resolver{} // no debrid account
	l, err := r.Resolve(t.Context(), FromURL(jb.URL+"/stream/realdebrid/ABC/1?sig=s"))
	if err != nil || l.URL != jb.URL+"/cdn/movie.mkv" || l.Size != 1000 || hits.Load() != 2 {
		t.Errorf("%+v %v, hits %d", l, err, hits.Load())
	}
}

func TestFileAndTorrentSources(t *testing.T) {
	p := filepath.Join(t.TempDir(), "movie.mkv")
	if err := os.WriteFile(p, make([]byte, 1234), 0o600); err != nil {
		t.Fatal(err)
	}
	r := &Resolver{}
	if l, err := r.Resolve(t.Context(), Source{Kind: File, Path: p}); err != nil || l.URL != p || l.Size != 1234 {
		t.Errorf("file: %+v %v", l, err)
	}
	if _, err := r.Resolve(t.Context(), Source{Kind: File, Path: p + ".gone"}); err == nil {
		t.Error("missing file resolved")
	}
	if _, err := r.Resolve(t.Context(), Source{Kind: Torrent, InfoHash: "abc", FileIdx: -1}); !errors.Is(err, ErrNotResolvable) {
		t.Errorf("infoHash: %v", err)
	}
	if _, err := r.Resolve(t.Context(), Source{}); !errors.Is(err, ErrNotResolvable) {
		t.Errorf("zero source: %v", err)
	}
}

func TestLinkTTL(t *testing.T) {
	now := time.Now()
	for _, c := range []struct {
		expiry time.Time
		want   time.Duration
	}{
		{time.Time{}, 4 * time.Hour}, // provider didn't say: cache.LinkTTL
		{now.Add(2 * time.Hour), 2 * time.Hour},
		{now.Add(time.Minute), 5 * time.Minute},   // too short to be useful
		{now.Add(-time.Hour), 5 * time.Minute},    // already past
		{now.Add(72 * time.Hour), 24 * time.Hour}, // capped
	} {
		if got := linkTTL(c.expiry, now); got != c.want {
			t.Errorf("expiry %v: %v, want %v", c.expiry.Sub(now), got, c.want)
		}
	}
}

func TestMagnetTargets(t *testing.T) {
	cases := map[string]Source{
		Magnet("ABCDEF", 2):                    {Kind: Torrent, InfoHash: "abcdef", FileIdx: 2},
		Magnet("abcdef", -1):                   {Kind: Torrent, InfoHash: "abcdef", FileIdx: -1},
		"magnet:?xt=urn:btih:abc&dn=Some+Name": {Kind: Torrent, InfoHash: "abc", FileIdx: -1},
		"magnet:?dn=no-hash":                   {Kind: URL, URL: "magnet:?dn=no-hash"},
		"https://cdn.example/magnet:?xt=urn:x": {Kind: URL, URL: "https://cdn.example/magnet:?xt=urn:x"},
	}
	for in, want := range cases {
		if got := FromURL(in); got != want {
			t.Errorf("FromURL(%q) = %+v, want %+v", in, got, want)
		}
	}
}
