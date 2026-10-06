package library

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sysadmin/blockbustr/internal/events"
	"github.com/sysadmin/blockbustr/internal/store/pg/db"
)

func TestFindSidecars(t *testing.T) {
	dir := t.TempDir()
	video := filepath.Join(dir, "Luca (2021).mkv")
	for _, n := range []string{"Luca (2021).mkv", "Luca (2021).srt", "Luca (2021).en.srt", "Luca (2021).ita.forced.ass",
		"Luca (2021).pt-BR.sdh.default.vtt", "Luca (2021) - Extras.srt", "Luca (2021).nfo", "Luca (2021).notalang.ssa", "Other.srt"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got := findSidecars(video)
	want := []struct {
		file, codec, lang       string
		forced, def, hearingImp bool
	}{
		{"Luca (2021).en.srt", "subrip", "eng", false, false, false},
		{"Luca (2021).ita.forced.ass", "ass", "ita", true, false, false},
		{"Luca (2021).notalang.ssa", "ssa", "", false, false, false},
		{"Luca (2021).pt-BR.sdh.default.vtt", "webvtt", "por", false, true, true},
		{"Luca (2021).srt", "subrip", "", false, false, false},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d sidecars: %+v", len(got), got)
	}
	for i, w := range want {
		g := got[i]
		if filepath.Base(g.Path) != w.file || g.Codec != w.codec || g.Language != w.lang || g.IsForced != w.forced ||
			g.IsDefault != w.def || g.IsHearingImpaired != w.hearingImp || !g.IsExternal {
			t.Errorf("%d: %+v, want %+v", i, g, w)
		}
	}

	sig := sidecarSignature(video)
	if sig == "" || sidecarSignature(filepath.Join(dir, "Nothing.mkv")) != "" {
		t.Errorf("signature %q", sig)
	}
	later := time.Now().Add(time.Hour)
	_ = os.Chtimes(filepath.Join(dir, "Luca (2021).en.srt"), later, later)
	if sidecarSignature(video) == sig {
		t.Error("an edited sidecar must change the signature")
	}
}

func TestScanStoresSidecars(t *testing.T) {
	f := newFixture(t)
	f.write("Movies/Luca (2021)/Luca (2021).mkv", "x")
	f.write("Movies/Luca (2021)/Luca (2021).en.srt", "1\n00:00:01,000 --> 00:00:02,000\nHi\n")
	s := f.scanner()
	movies, _ := f.libs(s)
	f.scan(s, movies)
	luca := f.itemNamed(movies, "Luca")
	streams := func() (out []string) {
		srcs, err := f.q.ListMediaSourcesForItems(t.Context(), []uuid.UUID{luca})
		if err != nil || len(srcs) != 1 {
			t.Fatal(srcs, err)
		}
		sts, _ := f.q.ListMediaStreamsForSources(t.Context(), []uuid.UUID{srcs[0].ID})
		for _, st := range sts {
			out = append(out, st.Type+"/"+deref(st.Codec)+"/"+deref(st.Language)+"/"+filepath.Base(deref(st.ExternalPath)))
			if st.IsExternal != (st.ExternalPath != nil) {
				t.Errorf("is_external and external_path disagree: %+v", st)
			}
		}
		return out
	}
	// The stub probe has video 0, audio 1, subtitle 2; the sidecar is 3.
	if got := streams(); len(got) != 4 || got[3] != "Subtitle/subrip/eng/Luca (2021).en.srt" {
		t.Errorf("streams: %v", got)
	}
	probes := f.prober.calls["Luca (2021).mkv"]
	f.write("Movies/Luca (2021)/Luca (2021).fre.forced.ass", "x")
	f.scan(s, movies)
	if f.prober.calls["Luca (2021).mkv"] != probes+1 {
		t.Errorf("a new sidecar re-reads the source: %d probes", f.prober.calls["Luca (2021).mkv"])
	}
	if got := streams(); len(got) != 5 || got[3] != "Subtitle/subrip/eng/Luca (2021).en.srt" || got[4] != "Subtitle/ass/fra/Luca (2021).fre.forced.ass" {
		t.Errorf("streams after adding one: %v", got)
	}
	f.scan(s, movies)
	if f.prober.calls["Luca (2021).mkv"] != probes+1 {
		t.Error("unchanged sidecars must not re-probe")
	}
}

func TestScanPublishesLibraryChanged(t *testing.T) {
	f := newFixture(t)
	bus := &events.Bus{Cache: f.cache}
	evs, err := bus.Subscribe(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	f.write("Movies/Luca (2021)/Luca (2021).mkv", "x")
	s := f.scanner()
	s.SetEvents(bus)
	movies, _ := f.libs(s)
	s.ScanAll(t.Context(), []db.Library{movies})
	select {
	case e := <-evs:
		if e.Kind != events.LibraryChanged || len(e.Libraries) != 1 || e.Libraries[0] != movies.ID {
			t.Errorf("event: %+v", e)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no LibraryChanged after a scan that found a file")
	}
	s.ScanAll(t.Context(), []db.Library{movies}) // nothing changed
	select {
	case e := <-evs:
		t.Errorf("unchanged rescan announced %+v", e)
	case <-time.After(300 * time.Millisecond):
	}
}
