package resolve

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sysadmin/blockbustr/internal/provider"
	"github.com/sysadmin/blockbustr/internal/testutil"
)

// fakeCloud is a debrid account: added torrents get ids t1, t2, …; cached
// hashes are ready at once, others downloading until finish().
type fakeCloud struct {
	provider.Provider
	name     provider.Name
	cached   map[string]bool // hashes the account has cached
	instant  bool            // InstantCheck reports cached hashes
	refuse   bool            // AddMagnet fails (a revoked key)
	files    []provider.File
	mu       sync.Mutex
	torrents map[string]*provider.Torrent
	adds     int
	deleted  []string
}

func newCloud(name provider.Name, cached ...string) *fakeCloud {
	c := &fakeCloud{name: name, cached: map[string]bool{}, torrents: map[string]*provider.Torrent{}, files: []provider.File{
		{ID: "1", Path: "Show/S01E01.mkv", SizeBytes: 900},
		{ID: "2", Path: "Show/S01E02 - The Kingsroad.mkv", SizeBytes: 1000},
		{ID: "3", Path: "Show/sample.mkv", SizeBytes: 10},
		{ID: "4", Path: "Show/readme.nfo", SizeBytes: 1},
	}}
	for _, h := range cached {
		c.cached[h] = true
	}
	return c
}

func (c *fakeCloud) Name() provider.Name { return c.name }

func (c *fakeCloud) AddMagnet(_ context.Context, magnet string) (string, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.refuse {
		return "", false, errors.New("HTTP 401 (code 8): bad_token")
	}
	c.adds++
	hash := strings.TrimPrefix(magnet, "magnet:?xt=urn:btih:")
	id := fmt.Sprintf("t%d", c.adds)
	t := &provider.Torrent{ID: id, Hash: hash, Status: provider.StatusDownloading}
	if c.cached[hash] {
		t.Status, t.Files = provider.StatusReady, c.files
	}
	if hash == "dead" {
		t.Status = provider.StatusError
	}
	c.torrents[id] = t
	return id, c.cached[hash], nil
}

func (c *fakeCloud) Torrent(_ context.Context, id string) (provider.Torrent, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	t, ok := c.torrents[id]
	if !ok {
		return provider.Torrent{}, errors.New("unknown torrent")
	}
	return *t, nil
}

func (c *fakeCloud) InstantCheck(_ context.Context, hashes []string) ([]provider.InstantResult, error) {
	var out []provider.InstantResult
	for _, h := range hashes {
		if c.instant && c.cached[h] {
			out = append(out, provider.InstantResult{Hash: h, Cached: true})
		}
	}
	return out, nil
}

func (c *fakeCloud) FileLink(_ context.Context, torrentID, fileID string) (string, time.Time, error) {
	return "https://cdn." + string(c.name) + "/" + torrentID + "/" + fileID, time.Now().Add(time.Hour), nil
}

func (c *fakeCloud) Delete(_ context.Context, id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.torrents, id)
	c.deleted = append(c.deleted, id)
	return nil
}

func (c *fakeCloud) addCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.adds
}

// finish completes every download.
func (c *fakeCloud) finish() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, t := range c.torrents {
		if t.Status == provider.StatusDownloading {
			t.Status, t.Files = provider.StatusReady, c.files
		}
	}
}

type memTorrents struct {
	mu sync.Mutex
	m  map[string]KnownTorrent // provider/hash
}

func (s *memTorrents) Find(_ context.Context, hash string) ([]KnownTorrent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []KnownTorrent
	for _, k := range s.m {
		if k.Hash == hash {
			out = append(out, k)
		}
	}
	return out, nil
}

func (s *memTorrents) Save(_ context.Context, t KnownTorrent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[string(t.Provider)+"/"+t.Hash] = t
	return nil
}

func (s *memTorrents) Forget(_ context.Context, p provider.Name, hash string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, string(p)+"/"+hash)
	return nil
}

func (s *memTorrents) get(p provider.Name, hash string) (KnownTorrent, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k, ok := s.m[string(p)+"/"+hash]
	return k, ok
}

func torrentResolver(clouds ...*fakeCloud) (*Resolver, *memTorrents) {
	store := &memTorrents{m: map[string]KnownTorrent{}}
	r := &Resolver{Providers: map[provider.Name]provider.Provider{}, Torrents: store, Log: testutil.Discard()}
	for _, c := range clouds {
		r.Providers[c.name] = c
		r.Order = append(r.Order, c.name)
	}
	return r, store
}

func TestTorrentCached(t *testing.T) {
	rd := newCloud(provider.RealDebrid, "aaaa")
	r, store := torrentResolver(rd)
	src := FromURL(Magnet("aaaa", 1, "S01E02 - The Kingsroad.mkv"))
	l, err := r.Resolve(t.Context(), src)
	if err != nil || l.URL != "https://cdn.realdebrid/t1/2" || !l.Private {
		t.Fatalf("Resolve = %+v, %v", l, err)
	}
	if k, ok := store.get(provider.RealDebrid, "aaaa"); !ok || k.TorrentID != "t1" || k.Status != provider.StatusReady {
		t.Errorf("remembered: %+v %v", k, ok)
	}
	// Another file of the same torrent (the link cache misses): found, not
	// added again.
	if l, err := r.Resolve(t.Context(), FromURL(Magnet("aaaa", 0, ""))); err != nil || l.URL != "https://cdn.realdebrid/t1/1" {
		t.Errorf("second file = %+v, %v", l, err)
	}
	if n := rd.addCount(); n != 1 {
		t.Errorf("added %d times, want once", n)
	}
}

func TestTorrentDownloading(t *testing.T) {
	rd := newCloud(provider.RealDebrid)
	r, store := torrentResolver(rd)
	src := FromURL(Magnet("bbbb", -1, ""))
	if _, err := r.Resolve(t.Context(), src); !errors.Is(err, ErrDownloading) {
		t.Fatalf("uncached: %v, want ErrDownloading", err)
	}
	if k, _ := store.get(provider.RealDebrid, "bbbb"); k.Status != provider.StatusDownloading {
		t.Errorf("status = %q", k.Status)
	}
	// Asking again while it downloads doesn't add it again.
	if _, err := r.Resolve(t.Context(), src); !errors.Is(err, ErrDownloading) || rd.addCount() != 1 {
		t.Errorf("again: %v, %d adds", err, rd.addCount())
	}
	// Done: plays, the largest video (no name or index given).
	rd.finish()
	if l, err := r.Resolve(t.Context(), src); err != nil || l.URL != "https://cdn.realdebrid/t1/2" {
		t.Errorf("finished: %+v, %v", l, err)
	}
	if k, _ := store.get(provider.RealDebrid, "bbbb"); k.Status != provider.StatusReady {
		t.Errorf("status after finishing = %q", k.Status)
	}
}

func TestTorrentRemovedFromCloud(t *testing.T) {
	rd := newCloud(provider.RealDebrid, "cccc")
	r, store := torrentResolver(rd)
	_ = store.Save(t.Context(), KnownTorrent{Provider: provider.RealDebrid, Hash: "cccc", TorrentID: "gone", Status: provider.StatusReady})
	l, err := r.Resolve(t.Context(), FromURL(Magnet("cccc", -1, "")))
	if err != nil || !strings.Contains(l.URL, "/t1/") {
		t.Fatalf("Resolve = %+v, %v", l, err)
	}
	if k, _ := store.get(provider.RealDebrid, "cccc"); k.TorrentID != "t1" {
		t.Errorf("remembered %q, want the re-added t1", k.TorrentID)
	}
}

func TestTorrentFailed(t *testing.T) {
	rd := newCloud(provider.RealDebrid)
	r, store := torrentResolver(rd)
	_, err := r.Resolve(t.Context(), FromURL(Magnet("dead", -1, "")))
	if err == nil || errors.Is(err, ErrDownloading) {
		t.Fatalf("dead torrent: %v", err)
	}
	if _, ok := store.get(provider.RealDebrid, "dead"); ok || len(rd.deleted) != 1 {
		t.Errorf("dead torrent kept: remembered %v, deleted %v", ok, rd.deleted)
	}
}

func TestTorrentPicksAccount(t *testing.T) {
	// RD first by priority, but TorBox reports the hash cached.
	rd := newCloud(provider.RealDebrid)
	tb := newCloud(provider.TorBox, "dddd")
	tb.instant = true
	r, _ := torrentResolver(rd, tb)
	l, err := r.Resolve(t.Context(), FromURL(Magnet("dddd", -1, "")))
	if err != nil || !strings.HasPrefix(l.URL, "https://cdn.torbox/") || rd.addCount() != 0 {
		t.Errorf("Resolve = %+v, %v; rd adds %d", l, err, rd.addCount())
	}
	// Nobody has it: the first by priority gets the download.
	if _, err := r.Resolve(t.Context(), FromURL(Magnet("eeee", -1, ""))); !errors.Is(err, ErrDownloading) || rd.addCount() != 1 {
		t.Errorf("uncached: %v; rd adds %d", err, rd.addCount())
	}
}

func TestTorrentConcurrentAddsOnce(t *testing.T) {
	rd := newCloud(provider.RealDebrid, "ffff")
	r, _ := torrentResolver(rd)
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Different files, so the link-level singleflight doesn't merge them.
			_, _ = r.Resolve(context.Background(), FromURL(Magnet("ffff", i%2, "")))
		}()
	}
	wg.Wait()
	if n := rd.addCount(); n != 1 {
		t.Errorf("added %d times", n)
	}
}

func TestTorrentWithoutAccount(t *testing.T) {
	r := &Resolver{}
	if _, err := r.Resolve(t.Context(), FromURL(Magnet("aaaa", -1, ""))); !errors.Is(err, ErrNotResolvable) {
		t.Errorf("no account: %v", err)
	}
}

func TestPickFile(t *testing.T) {
	files := []provider.File{
		{ID: "a", Path: "Pack/info.txt", SizeBytes: 1},
		{ID: "b", Path: "Pack/Movie.2160p.mkv", SizeBytes: 900},
		{ID: "c", Path: "Pack/Extras/Featurette.mp4", SizeBytes: 50},
		{ID: "d", Path: "Pack/Movie.1080p.mkv", SizeBytes: 400},
	}
	cases := []struct {
		idx  int
		name string
		want string
	}{
		{-1, "movie.1080p.MKV", "d"}, // by name, case-insensitive, any folder
		{2, "", "c"},                 // by index
		{2, "missing.mkv", "c"},      // a name not found falls back to the index
		{0, "", "b"},                 // the index points at a non-video: largest video
		{9, "", "b"},                 // out of range
		{-1, "", "b"},
	}
	for _, c := range cases {
		if f, ok := pickFile(files, c.idx, c.name); !ok || f.ID != c.want {
			t.Errorf("pickFile(%d, %q) = %q, want %q", c.idx, c.name, f.ID, c.want)
		}
	}
	if _, ok := pickFile([]provider.File{{ID: "x", Path: "a.nfo"}}, -1, ""); ok {
		t.Error("picked a non-video")
	}
}

func TestPGTorrents(t *testing.T) {
	q, pool := testutil.Queries(t)
	if _, err := pool.Exec(t.Context(), `INSERT INTO debrid_accounts (provider, api_key_enc) VALUES ('realdebrid', '\x00'), ('torbox', '\x00')`); err != nil {
		t.Fatal(err)
	}
	s := PGTorrents{Q: q}
	ctx := t.Context()
	for _, k := range []KnownTorrent{
		{Provider: provider.RealDebrid, Hash: "aaaa", TorrentID: "R1", Status: provider.StatusDownloading},
		{Provider: provider.TorBox, Hash: "aaaa", TorrentID: "9", Status: provider.StatusReady},
		{Provider: provider.RealDebrid, Hash: "bbbb", TorrentID: "R2", Status: provider.StatusDownloading},
		{Provider: provider.RealDebrid, Hash: "aaaa", TorrentID: "R1", Status: provider.StatusReady}, // update
	} {
		if err := s.Save(ctx, k); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.Find(ctx, "aaaa")
	if err != nil || len(got) != 2 {
		t.Fatalf("Find = %+v, %v", got, err)
	}
	for _, k := range got {
		if k.Status != provider.StatusReady {
			t.Errorf("%+v not updated", k)
		}
	}
	if ready := s.Ready(ctx, []string{"aaaa", "bbbb", "cccc"}); !ready["aaaa"] || ready["bbbb"] || len(ready) != 1 {
		t.Errorf("Ready = %v", ready)
	}
	if err := s.Forget(ctx, provider.TorBox, "aaaa"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Find(ctx, "aaaa"); len(got) != 1 || got[0].Provider != provider.RealDebrid {
		t.Errorf("after Forget: %+v", got)
	}
	// Removing an account drops its torrents.
	if _, err := pool.Exec(ctx, `DELETE FROM debrid_accounts WHERE provider = 'realdebrid'`); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Find(ctx, "aaaa"); len(got) != 0 {
		t.Errorf("after account removal: %+v", got)
	}
}

func TestTorrentRefusedTriesNextAccount(t *testing.T) {
	rd := newCloud(provider.RealDebrid)
	rd.refuse = true
	tb := newCloud(provider.TorBox, "abab")
	r, store := torrentResolver(rd, tb)
	l, err := r.Resolve(t.Context(), FromURL(Magnet("abab", -1, "")))
	if err != nil || !strings.HasPrefix(l.URL, "https://cdn.torbox/") {
		t.Fatalf("Resolve = %+v, %v", l, err)
	}
	if _, ok := store.get(provider.TorBox, "abab"); !ok {
		t.Error("not remembered on the account that took it")
	}
	tb.refuse = true
	if _, err := r.Resolve(t.Context(), FromURL(Magnet("cdcd", -1, ""))); err == nil || !strings.Contains(err.Error(), "realdebrid") || !strings.Contains(err.Error(), "torbox") {
		t.Errorf("every account refusing: %v", err)
	}
}
