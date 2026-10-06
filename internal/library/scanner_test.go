package library

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sysadmin/blockbustr/internal/cache"
	"github.com/sysadmin/blockbustr/internal/config"
	"github.com/sysadmin/blockbustr/internal/media"
	"github.com/sysadmin/blockbustr/internal/store/pg/db"
	"github.com/sysadmin/blockbustr/internal/testutil"
)

// stubProber returns canned media info and counts probes per file.
type stubProber struct {
	mu    sync.Mutex
	calls map[string]int
	fail  map[string]bool
}

func (p *stubProber) Probe(_ context.Context, target string, _ media.Options) (*media.Info, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls[filepath.Base(target)]++
	if p.fail[filepath.Base(target)] {
		return nil, errors.New("ffprobe: invalid data found when processing input")
	}
	return &media.Info{
		Container: "matroska,webm", Duration: 24 * time.Minute, Size: 1000, Bitrate: 8_000_000,
		Streams: []media.Stream{
			{Index: 0, Type: media.StreamVideo, Codec: "h264", Width: 1920, Height: 1080, BitDepth: 8, VideoRange: "SDR", VideoRangeType: "SDR", Level: 41, AverageFrameRate: 23.976, IsDefault: true},
			{Index: 1, Type: media.StreamAudio, Codec: "flac", Language: "jpn", Channels: 2, SampleRate: 48000, IsDefault: true, IsOriginal: true},
			{Index: 2, Type: media.StreamSubtitle, Codec: "ass", Language: "eng"},
		},
		Chapters: []media.Chapter{{Start: 0, Title: "Prologue"}, {Start: 89 * time.Second, Title: "Part A"}},
	}, nil
}

func (p *stubProber) total() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, c := range p.calls {
		n += c
	}
	return n
}

type fixture struct {
	t      *testing.T
	root   string
	q      *db.Queries
	pool   *pgxpool.Pool
	cache  *cache.Cache
	prober *stubProber
	cfg    config.Scan
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	q, pool := testutil.Queries(t)
	return &fixture{t: t, root: t.TempDir(), q: q, pool: pool, cache: testutil.Cache(t),
		prober: &stubProber{calls: map[string]int{}, fail: map[string]bool{}},
		cfg:    config.Scan{ProbeWorkers: 2, ProbeTimeout: time.Minute, MissingGrace: time.Hour}}
}

func (f *fixture) write(rel, content string) string {
	f.t.Helper()
	p := filepath.Join(f.root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		f.t.Fatal(err)
	}
	return p
}

func (f *fixture) scanner() *Scanner {
	return NewScanner(f.pool, f.cache, f.prober, f.cfg, testutil.Discard())
}

func (f *fixture) libs(s *Scanner) (movies, shows db.Library) {
	f.t.Helper()
	libs, err := s.SyncLibraries(f.t.Context(), []config.Library{
		{Name: "Movies", Kind: "movies", Paths: []string{filepath.Join(f.root, "Movies")}},
		{Name: "Shows", Kind: "tvshows", Paths: []string{filepath.Join(f.root, "Series")}},
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return libs[0], libs[1]
}

func (f *fixture) scan(s *Scanner, lib db.Library) Result {
	f.t.Helper()
	res, err := s.Scan(f.t.Context(), lib)
	if err != nil {
		f.t.Fatal(err)
	}
	return res
}

// items returns "Type|Name|year/season/episode|parent name" lines, sorted.
func (f *fixture) items(lib db.Library) []string {
	f.t.Helper()
	rows, err := f.q.ListLibraryItems(f.t.Context(), lib.ID)
	if err != nil {
		f.t.Fatal(err)
	}
	names := map[string]string{}
	for _, r := range rows {
		names[r.ID.String()] = r.Name
	}
	var out []string
	for _, r := range rows {
		parent := ""
		if r.ParentID != nil {
			parent = names[r.ParentID.String()]
		}
		n := func(p *int32) int {
			if p == nil {
				return -1
			}
			return int(*p)
		}
		miss := ""
		if r.MissingSince != nil {
			miss = " (missing)"
		}
		out = append(out, strings.Join([]string{r.Type, r.Name, itoa(n(r.ProductionYear)) + "/" + itoa(n(r.ParentIndexNumber)) + "/" + itoa(n(r.IndexNumber)), parent + miss}, "|"))
	}
	sort.Strings(out)
	return out
}

func itoa(n int) string {
	if n < 0 {
		return "-"
	}
	return strconv.Itoa(n)
}

func sprintf(format string, a ...any) string { return fmt.Sprintf(format, a...) }

func TestScanBuildsHierarchy(t *testing.T) {
	f := newFixture(t)
	const ghost = "MF Ghost (2023) - S01E0%d.00%d - %s [Bluray-1080p Remux][8bit][h264][FLAC 2.0][JA+EN]-CRUCiBLE.mkv"
	f.write("Movies/Luca (2021)/Luca (2021).strm", "http://jellybird:8097/stream/realdebrid/X/1?sig=s\n")
	f.write("Movies/Mortal Kombat II (2026)/Mortal Kombat II (2026).mp4", "x")
	f.write("Movies/Mortal Kombat II (2026)/Featurettes/Making Of.mkv", "x")    // extra
	f.write("Movies/Mortal Kombat II (2026)/Mortal Kombat II-trailer.mkv", "x") // extra
	f.write("Movies/Broken.strm", "not a url")                                  // unreadable .strm
	f.write("Movies/notes.txt", "x")                                            // not media
	f.write("Movies/.hidden/Secret (2000).mkv", "x")                            // hidden dir
	f.write("Movies/@eaDir/Thumb (2000).mkv", "x")                              // NAS junk
	f.write("Series/MF Ghost/Season 01/"+sprintf(ghost, 1, 1, "The Challenger from England"), "x")
	f.write("Series/MF Ghost/Season 01/"+sprintf(ghost, 2, 2, "The Shocking New MFG Generation"), "x")
	f.write("Series/MF Ghost/Specials/MF Ghost S00E01.mkv", "x")
	f.write("Series/The Office (US)/Season 2/The.Office.US.S02E03.720p.mkv", "x")

	s := f.scanner()
	movies, shows := f.libs(s)
	mr := f.scan(s, movies)
	if mr.Files != 2 || mr.Skipped != 3 || mr.Probed != 1 || mr.RemoteSource != 1 {
		t.Errorf("movies result: %+v", mr)
	}
	sr := f.scan(s, shows)
	if sr.Files != 4 || sr.Probed != 4 {
		t.Errorf("shows result: %+v", sr)
	}

	wantMovies := []string{
		"CollectionFolder|Movies|-/-/-|",
		"Movie|Luca|2021/-/-|Movies",
		"Movie|Mortal Kombat II|2026/-/-|Movies",
	}
	if got := f.items(movies); !equal(got, wantMovies) {
		t.Errorf("movies:\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(wantMovies, "\n"))
	}
	wantShows := []string{
		"CollectionFolder|Shows|-/-/-|",
		"Episode|Episode 1|-/0/1|Specials",
		"Episode|Episode 3|-/2/3|Season 2",
		"Episode|The Challenger from England|-/1/1|Season 1",
		"Episode|The Shocking New MFG Generation|-/1/2|Season 1",
		"Season|Season 1|-/-/1|MF Ghost",
		"Season|Season 2|-/-/2|The Office (US)",
		"Season|Specials|-/-/0|MF Ghost",
		"Series|MF Ghost|-/-/-|Shows",
		"Series|The Office (US)|-/-/-|Shows",
	}
	if got := f.items(shows); !equal(got, wantShows) {
		t.Errorf("shows:\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(wantShows, "\n"))
	}

	// The probed MP4 has its streams, chapters and runtime; the .strm an
	// unprobed remote source.
	rows, _ := f.q.ListLibraryItems(t.Context(), movies.ID)
	for _, r := range rows {
		switch r.Name {
		case "Mortal Kombat II":
			src, err := f.q.GetMediaSourceByItem(t.Context(), r.ID)
			if err != nil || src.Protocol != "File" || *src.Container != "mkv" || src.ProbedAt == nil || *src.Bitrate != 8_000_000 || *src.RuntimeTicks != int64(24*time.Minute/100) {
				t.Fatalf("file source: %+v %v", src, err)
			}
			streams, _ := f.q.ListMediaStreams(t.Context(), src.ID)
			if len(streams) != 3 || streams[0].Type != "Video" || *streams[0].BitDepth != 8 || *streams[0].Level != 41 || streams[1].Type != "Audio" || !streams[1].IsOriginal || *streams[1].SampleRate != 48000 {
				t.Errorf("streams: %+v", streams)
			}
			chapters, _ := f.q.ListChapters(t.Context(), r.ID)
			if len(chapters) != 2 || chapters[1].Name != "Part A" || chapters[1].StartTicks != int64(89*time.Second/100) {
				t.Errorf("chapters: %+v", chapters)
			}
			if r.RuntimeTicks == nil || *r.RuntimeTicks != int64(24*time.Minute/100) {
				t.Errorf("item runtime: %v", r.RuntimeTicks)
			}
		case "Luca":
			src, err := f.q.GetMediaSourceByItem(t.Context(), r.ID)
			if err != nil || src.Protocol != "Http" || !src.IsRemote || src.ProbedAt != nil || !strings.HasPrefix(src.PathOrUrl, "http://jellybird") {
				t.Errorf("strm source: %+v %v", src, err)
			}
			if r.Etag == nil || strings.Contains(*r.Etag, "jellybird") || !strings.HasPrefix(*r.Etag, "strm-") {
				t.Errorf("strm etag must not contain the URL: %v", r.Etag)
			}
		}
	}
}

func TestRescanIsIdempotent(t *testing.T) {
	f := newFixture(t)
	f.write("Movies/Luca (2021)/Luca (2021).strm", "http://jellybird/a\n")
	ep := f.write("Series/MF Ghost/Season 01/MF Ghost S01E01.mkv", "x")
	f.write("Series/MF Ghost/Season 01/MF Ghost S01E02.mkv", "x")
	s := f.scanner()
	movies, shows := f.libs(s)
	f.scan(s, movies)
	f.scan(s, shows)
	before := f.items(shows)
	ids := map[string]string{}
	rows, _ := f.q.ListLibraryItems(t.Context(), shows.ID)
	for _, r := range rows {
		ids[r.Name] = r.ID.String()
	}
	probes := f.prober.total()

	r1 := f.scan(s, shows)
	r2 := f.scan(s, movies)
	if f.prober.total() != probes || r1.Probed != 0 || r2.RemoteSource != 0 {
		t.Errorf("unchanged files re-processed: probes %d→%d, %+v %+v", probes, f.prober.total(), r1, r2)
	}
	if after := f.items(shows); !equal(after, before) {
		t.Errorf("rescan changed items:\n%v\n%v", before, after)
	}
	rows, _ = f.q.ListLibraryItems(t.Context(), shows.ID)
	for _, r := range rows {
		if ids[r.Name] != r.ID.String() {
			t.Errorf("%s: id changed on rescan", r.Name)
		}
	}

	// Touching one file re-probes exactly that file.
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(ep, later, later); err != nil {
		t.Fatal(err)
	}
	if r := f.scan(s, shows); r.Probed != 1 || f.prober.calls["MF Ghost S01E01.mkv"] != 2 || f.prober.calls["MF Ghost S01E02.mkv"] != 1 {
		t.Errorf("changed file: %+v %v", r, f.prober.calls)
	}

	// A new .strm URL replaces the remote source.
	f.write("Movies/Luca (2021)/Luca (2021).strm", "http://jellybird/b\n")
	if r := f.scan(s, movies); r.RemoteSource != 1 {
		t.Errorf("strm change: %+v", r)
	}
	rows, _ = f.q.ListLibraryItems(t.Context(), movies.ID)
	for _, r := range rows {
		if r.Name == "Luca" {
			if src, _ := f.q.GetMediaSourceByItem(t.Context(), r.ID); src.PathOrUrl != "http://jellybird/b" {
				t.Errorf("strm source not replaced: %s", src.PathOrUrl)
			}
		}
	}
}

func TestMissingFilesHaveAGracePeriod(t *testing.T) {
	f := newFixture(t)
	gone := f.write("Series/Show/Season 01/Show S01E01.mkv", "x")
	f.write("Series/Other/Season 01/Other S01E01.mkv", "x")
	s := f.scanner()
	_, shows := f.libs(s)
	f.scan(s, shows)

	if err := os.RemoveAll(filepath.Dir(filepath.Dir(gone))); err != nil {
		t.Fatal(err)
	}
	r := f.scan(s, shows)
	if r.Missing != 2 || r.Deleted != 0 { // the episode and its series
		t.Errorf("within grace: %+v", r)
	}
	if got := f.items(shows); !contains(got, "Episode|Episode 1|-/1/1|Season 1 (missing)") {
		t.Errorf("missing episode not kept: %v", got)
	}

	// Past the grace period the item is deleted and empty containers go too.
	s.cfg.MissingGrace = 0
	r = f.scan(s, shows)
	if r.Deleted != 2 {
		t.Errorf("after grace: %+v", r)
	}
	want := []string{
		"CollectionFolder|Shows|-/-/-|",
		"Episode|Episode 1|-/1/1|Season 1",
		"Season|Season 1|-/-/1|Other",
		"Series|Other|-/-/-|Shows",
	}
	if got := f.items(shows); !equal(got, want) {
		t.Errorf("after delete:\n%s", strings.Join(got, "\n"))
	}

	// A file that comes back within the grace period is restored, not re-added.
	f.write("Series/Other/Season 01/Other S01E02.mkv", "x")
	s.cfg.MissingGrace = time.Hour
	f.scan(s, shows)
	tmp := filepath.Join(f.root, "Series/Other/Season 01/Other S01E02.mkv")
	if err := os.Rename(tmp, tmp+".bak"); err != nil {
		t.Fatal(err)
	}
	f.scan(s, shows)
	if err := os.Rename(tmp+".bak", tmp); err != nil {
		t.Fatal(err)
	}
	f.scan(s, shows)
	for _, line := range f.items(shows) {
		if strings.Contains(line, "(missing)") {
			t.Errorf("restored file still missing: %s", line)
		}
	}
}

func TestProbeFailureIsRecordedNotRetried(t *testing.T) {
	f := newFixture(t)
	f.write("Movies/Bad (2000)/Bad (2000).mkv", "garbage")
	f.prober.fail["Bad (2000).mkv"] = true
	s := f.scanner()
	movies, _ := f.libs(s)
	if r := f.scan(s, movies); r.ProbeFailed != 1 || r.Probed != 0 {
		t.Errorf("first scan: %+v", r)
	}
	if r := f.scan(s, movies); r.ProbeFailed != 0 || f.prober.calls["Bad (2000).mkv"] != 1 {
		t.Errorf("failed probe retried without a file change: %+v %v", r, f.prober.calls)
	}
	rows, _ := f.q.ListLibraryItems(t.Context(), movies.ID)
	for _, r := range rows {
		if r.Type == "Movie" {
			src, err := f.q.GetMediaSourceByItem(t.Context(), r.ID)
			if err != nil || src.ProbeError == nil || !strings.Contains(*src.ProbeError, "invalid data") || *src.Size != 7 {
				t.Errorf("failed source: %+v %v", src, err)
			}
		}
	}
}

func TestScanLockAndMissingRoot(t *testing.T) {
	f := newFixture(t)
	s := f.scanner()
	movies, _ := f.libs(s)
	if _, err := s.Scan(t.Context(), movies); err == nil {
		t.Error("scanning a library whose path doesn't exist should fail")
	}
	if err := os.MkdirAll(filepath.Join(f.root, "Movies"), 0o755); err != nil {
		t.Fatal(err)
	}
	lock, ok, err := f.cache.TryLock(t.Context(), cache.ScanLockKey(movies.ID), time.Minute)
	if err != nil || !ok {
		t.Fatal(err)
	}
	if _, err := s.Scan(t.Context(), movies); !errors.Is(err, ErrScanInProgress) {
		t.Errorf("concurrent scan: %v", err)
	}
	_ = lock.Unlock(t.Context())
	f.scan(s, movies)
	if _, err := s.Scan(t.Context(), movies); err != nil {
		t.Errorf("lock not released after scan: %v", err)
	}
}

func TestSyncLibrariesIsIdempotent(t *testing.T) {
	f := newFixture(t)
	s := f.scanner()
	a, _ := f.libs(s)
	b, _ := f.libs(s)
	if a.ID != b.ID {
		t.Error("library id changed on re-sync")
	}
	if got := f.items(a); len(got) != 1 {
		t.Errorf("collection folders: %v", got)
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
