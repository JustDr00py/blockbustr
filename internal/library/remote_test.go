package library

import (
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/sysadmin/blockbustr/internal/store/pg/db"
)

// itemNamed finds a scanned item's id by name.
func (f *fixture) itemNamed(lib db.Library, name string) uuid.UUID {
	f.t.Helper()
	rows, err := f.q.ListLibraryItems(f.t.Context(), lib.ID)
	if err != nil {
		f.t.Fatal(err)
	}
	for _, r := range rows {
		if r.Name == name {
			return r.ID
		}
	}
	f.t.Fatalf("no item %q", name)
	return uuid.Nil
}

func TestProbeRemote(t *testing.T) {
	f := newFixture(t)
	f.write("Movies/Luca (2021)/Luca (2021).strm", "http://jellybird:8097/stream/realdebrid/X/1?sig=s\n")
	f.write("Movies/Broken (2020)/Broken (2020).strm", "http://jellybird:8097/stream/broken\n")
	f.write("Movies/Local (2019)/Local (2019).mkv", "x")
	s := f.scanner()
	movies, _ := f.libs(s)
	f.scan(s, movies)
	luca, broken, local := f.itemNamed(movies, "Luca"), f.itemNamed(movies, "Broken"), f.itemNamed(movies, "Local")
	if n := f.prober.calls["1?sig=s"]; n != 0 {
		t.Fatalf("scans don't probe remote sources: %d", n)
	}

	// Parallel first plays share one probe.
	var wg sync.WaitGroup
	for range 5 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if ok, err := s.ProbeRemote(t.Context(), luca); !ok || err != nil {
				t.Errorf("probe: %v %v", ok, err)
			}
		}()
	}
	wg.Wait()
	if n := f.prober.calls["1?sig=s"]; n < 1 || n > 2 { // at most a late second caller
		t.Errorf("probes: %d", n)
	}
	srcs, err := f.q.ListMediaSourcesForItems(t.Context(), []uuid.UUID{luca})
	if err != nil || len(srcs) != 1 {
		t.Fatal(srcs, err)
	}
	src := srcs[0]
	streams, _ := f.q.ListMediaStreamsForSources(t.Context(), []uuid.UUID{src.ID})
	if !src.IsRemote || src.ProbedAt == nil || src.ProbeError != nil || deref(src.Container) != "mkv" ||
		deref(src.Bitrate) != 8_000_000 || src.RuntimeTicks == nil || len(streams) != 3 || src.PathOrUrl != "http://jellybird:8097/stream/realdebrid/X/1?sig=s" {
		t.Errorf("probed source: %+v, %d streams", src, len(streams))
	}
	// A rescan keeps the probed source (same .strm).
	f.scan(s, movies)
	if again, _ := f.q.ListMediaSourcesForItems(t.Context(), []uuid.UUID{luca}); len(again) != 1 || again[0].ProbedAt == nil {
		t.Errorf("rescan dropped the probe: %+v", again)
	}

	f.prober.fail["broken"] = true
	if ok, err := s.ProbeRemote(t.Context(), broken); ok || err != nil {
		t.Errorf("failed probe: %v %v", ok, err)
	}
	if b, _ := f.q.ListMediaSourcesForItems(t.Context(), []uuid.UUID{broken}); len(b) != 1 || b[0].ProbedAt == nil || b[0].ProbeError == nil {
		t.Errorf("failure recorded: %+v", b)
	}
	if ok, err := s.ProbeRemote(t.Context(), local); ok || err != nil {
		t.Errorf("a local file isn't probed remotely: %v %v", ok, err)
	}
}

func deref[T any](p *T) T {
	var zero T
	if p == nil {
		return zero
	}
	return *p
}
