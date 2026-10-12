package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sysadmin/blockbustr/internal/resolve"
	"github.com/sysadmin/blockbustr/internal/store/pg/db"
	"github.com/sysadmin/blockbustr/internal/testutil"
)

// matchboxStreams is a French "VFF" release: French audio and forced French
// subtitles are its defaults.
func matchboxStreams() []db.MediaStream {
	return []db.MediaStream{
		{Idx: 0, Type: "Video"},
		{Idx: 1, Type: "Audio", Language: ptr("fre"), IsDefault: true},
		{Idx: 2, Type: "Audio", Language: ptr("eng")},
		{Idx: 3, Type: "Audio", Language: ptr("fre"), Title: ptr("AD")},
		{Idx: 4, Type: "Audio", Language: ptr("eng"), Title: ptr("AD")},
		{Idx: 5, Type: "Subtitle", Language: ptr("fre"), Title: ptr("Forced"), IsDefault: true, IsForced: true},
		{Idx: 6, Type: "Subtitle", Language: ptr("fre")},
		{Idx: 7, Type: "Subtitle", Language: ptr("fre"), IsHearingImpaired: true},
		{Idx: 8, Type: "Subtitle", Language: ptr("eng")},
	}
}

func TestPreferAudioClearsOtherSubtitleDefaults(t *testing.T) {
	streams := matchboxStreams()
	preferAudio(streams, "eng")
	for _, st := range streams {
		switch {
		case st.Type == "Audio" && st.IsDefault != (st.Idx == 2):
			t.Errorf("audio %d default = %v", st.Idx, st.IsDefault)
		case st.Type == "Subtitle" && st.IsDefault:
			t.Errorf("subtitle %d is still the default", st.Idx)
		}
	}
	if !streams[5].IsForced {
		t.Error("the forced flag was cleared")
	}
}

func TestPreferAudioKeepsPreferredSubtitleDefault(t *testing.T) {
	streams := matchboxStreams()
	streams[5].IsDefault, streams[8].IsDefault = false, true
	preferAudio(streams, "eng")
	if !streams[8].IsDefault {
		t.Error("the English subtitle lost its default")
	}
}

func TestApplyProbesRanksForeignStartLast(t *testing.T) {
	fre, eng, unprobed := uuid.New(), uuid.New(), uuid.New()
	probes := map[uuid.UUID]choiceProbe{
		fre: {Streams: matchboxStreams()},
		eng: {Streams: []db.MediaStream{
			{Idx: 0, Type: "Video"},
			{Idx: 1, Type: "Audio", Language: ptr("eng"), IsDefault: true},
			{Idx: 2, Type: "Audio", Language: ptr("fre")},
		}},
	}
	order := func(lang string) []uuid.UUID {
		sources := []db.MediaSource{{ID: fre}, {ID: eng}, {ID: unprobed}}
		applyProbes(sources, map[uuid.UUID][]db.MediaStream{}, probes, lang)
		var ids []uuid.UUID
		for _, s := range sources {
			ids = append(ids, s.ID)
		}
		return ids
	}
	if got, want := order("eng"), []uuid.UUID{eng, unprobed, fre}; !slices.Equal(got, want) {
		t.Errorf("order = %v, want %v (eng, unprobed, fre)", got, want)
	}
	if got, want := order(""), []uuid.UUID{fre, eng, unprobed}; !slices.Equal(got, want) {
		t.Errorf("without a language, order = %v, want %v", got, want)
	}
}

// insertCatalogMovie adds a catalog movie at path (a stremio ref) to the
// integration test database.
func insertCatalogMovie(t *testing.T, path string) db.Item {
	t.Helper()
	lib, it := uuid.New(), db.Item{ID: uuid.New(), Type: "Movie", SourceKind: "stremio", Path: &path}
	execSQL(t, `INSERT INTO libraries (id, name, kind) VALUES ($1, 'Catalog', 'stremio')`, lib)
	execSQL(t, `INSERT INTO items (id, library_id, type, name, sort_name, source_kind, path, stremio_ref)
		VALUES ($1, $2, 'Movie', 'Movie', 'movie', 'stremio', $3, '{"id":"tt0295297"}')`, it.ID, lib, path)
	return it
}

// saveChoiceProbe stores p as choice's probe.
func saveChoiceProbe(t *testing.T, q *db.Queries, item, choice uuid.UUID, p choiceProbe) {
	t.Helper()
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := q.SaveChoiceProbe(t.Context(), db.SaveChoiceProbeParams{ChoiceID: choice, ItemID: item, Probe: raw}); err != nil {
		t.Fatal(err)
	}
}

// Probes are kept in Postgres: emptying Redis (a restart) doesn't probe
// every version again. A probe queued while the same version was being
// probed isn't redone once its turn comes (2026-10-11: details and
// PlaybackInfo each probed one version, 2 s on play).
func TestChoiceProbesKept(t *testing.T) {
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { serveVideo(w, "x") }))
	t.Cleanup(cdn.Close)
	_, d := newIntegrationServer(t)
	pr := &multiAudioProber{}
	d.ChoiceProber = pr
	d.Resolver = &resolve.Resolver{Cache: d.Cache, Log: testutil.Discard()}
	a := &api{Deps: d, probeSem: make(chan struct{}, probeParallel)}
	it := insertCatalogMovie(t, "stremio:movie:tt0295297")
	c := StreamChoice{ID: uuid.NewSHA1(it.ID, []byte("url:"+cdn.URL+"/a.mkv")), Target: cdn.URL + "/a.mkv", Ready: true}

	a.probeChoice(t.Context(), it.ID, c)
	a.probeChoice(t.Context(), it.ID, c) // its turn came after the first ended
	if n := pr.calls.Load(); n != 1 {
		t.Errorf("probed %d times, want 1", n)
	}

	d.Cache = testutil.Cache(t) // Redis emptied
	b := &api{Deps: d, probeSem: make(chan struct{}, probeParallel)}
	p, ok := b.loadChoiceProbes(t.Context(), []StreamChoice{c})[c.ID]
	if !ok || len(p.Streams) != 4 {
		t.Fatalf("after Redis emptied: probe %+v, %v; want its 4 tracks", p, ok)
	}
	<-b.probeChoices(t.Context(), it.ID, []StreamChoice{c}, 0)
	if n := pr.calls.Load(); n != 1 {
		t.Errorf("probed %d times after Redis emptied, want 1", n)
	}

	// Offered again after a month: read, it's marked used and kept;
	// unread, it's pruned.
	execSQL(t, `UPDATE choice_probes SET used_at = now() - interval '40 days'`)
	b.loadChoiceProbes(t.Context(), []StreamChoice{c})
	month := time.Now().Add(-30 * 24 * time.Hour)
	if n, err := d.Queries.PruneChoiceProbes(t.Context(), month); err != nil || n != 0 {
		t.Errorf("pruned %d (%v) of a probe just read, want 0", n, err)
	}
	execSQL(t, `UPDATE choice_probes SET used_at = now() - interval '40 days'`)
	if n, err := d.Queries.PruneChoiceProbes(t.Context(), month); err != nil || n != 1 {
		t.Errorf("pruned %d (%v) of a probe unread for 40 days, want 1", n, err)
	}

	// A title's probes go with it.
	saveChoiceProbe(t, d.Queries, it.ID, c.ID, p)
	execSQL(t, `DELETE FROM items WHERE id = $1`, it.ID)
	if _, ok := b.loadChoiceProbes(t.Context(), []StreamChoice{c})[c.ID]; ok {
		t.Error("a deleted title's probe is still there")
	}
}
