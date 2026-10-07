package stremio

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"golang.org/x/sync/singleflight"

	"github.com/sysadmin/blockbustr/internal/cache"
	"github.com/sysadmin/blockbustr/internal/library"
	"github.com/sysadmin/blockbustr/internal/store/pg/db"
)

// CinemetaURL is Stremio's official metadata addon: the search source when
// no enabled addon has a search catalog (DESIGN §7.4).
const CinemetaURL = "https://v3-cinemeta.strem.io"

// discoverNS namespaces discover item ids: UUIDv5(discoverNS, "imdb:tt…").
var discoverNS = uuid.MustParse("6f1b5c2e-4b7a-5d3e-9c1a-2b8d7e4f0a91")

// DiscoverID is the id a title found by search always has.
func DiscoverID(imdb string) uuid.UUID { return uuid.NewSHA1(discoverNS, []byte("imdb:"+imdb)) }

// Discover is in-client search beyond the library (TASKS P3.12–P3.13,
// DESIGN §7.4): every enabled addon with a search catalog is asked (or
// Cinemeta when none has one), and the titles not already in a library are
// stored in the hidden discover library, so clients can open, play and
// favourite them like any other item. Series get their episodes when
// first opened. Titles nobody kept are cleaned up after Retention.
type Discover struct {
	Registry *Registry
	Cache    *cache.Cache
	Log      *slog.Logger
	// Timeout bounds a whole search (default 3s); sources that miss it are
	// left out.
	Timeout time.Duration
	// Retention keeps unplayed, unfavourited titles this long after they
	// were last found (default 7 days).
	Retention time.Duration
	// Refresh enriches the discover library's new titles (TMDB); nil skips.
	Refresh func(ctx context.Context, lib db.Library) error
	// Fallback is searched when no enabled addon has a search catalog;
	// "" searches nothing then.
	Fallback string

	mu      sync.Mutex
	lib     *db.Library
	refresh chan struct{}
	flight  singleflight.Group
}

// found is one search result.
type found struct {
	Type  string // movie | series
	Meta  Meta
	Addon uuid.UUID // uuid.Nil for the fallback
}

func (d *Discover) library(ctx context.Context) (db.Library, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.lib != nil {
		return *d.lib, nil
	}
	lib, err := d.Registry.q().EnsureDiscoverLibrary(ctx)
	if err != nil {
		return lib, err
	}
	d.lib = &lib
	return lib, nil
}

// SearchItems implements handlers.RemoteSearch: up to limit titles of types
// (Movie, Series) matching term, best first, library items where the title
// is already in one.
func (d *Discover) SearchItems(ctx context.Context, term string, types []string, limit int) ([]db.Item, error) {
	if limit <= 0 || limit > 50 {
		limit = 20
	}
	var kinds []string
	for _, t := range types {
		switch t {
		case "Movie":
			kinds = append(kinds, "movie")
		case "Series":
			kinds = append(kinds, "series")
		}
	}
	if len(kinds) == 0 || strings.TrimSpace(term) == "" {
		return nil, nil
	}
	results := d.search(ctx, term, kinds)
	if len(results) == 0 {
		return nil, nil
	}
	return d.store(ctx, results, types, limit)
}

type searchSource struct {
	base, catalog, kind string
	addon               uuid.UUID
}

// sources are the search catalogs to ask for kinds: one per enabled addon
// and kind, or the fallback for a kind no addon searches.
func (d *Discover) sources(ctx context.Context, kinds []string) []searchSource {
	var out []searchSource
	_, addons, err := d.Registry.openAddons(ctx, d.Log)
	if err != nil {
		d.Log.Warn("search: addons unavailable", "err", err)
	}
	for _, k := range kinds {
		n := 0
		for _, ad := range addons {
			for _, c := range ad.manifest.Catalogs {
				if c.Type != k || !c.Accepts("search") || slices.ContainsFunc(c.Requires(), func(r string) bool { return r != "search" }) {
					continue
				}
				out = append(out, searchSource{base: ad.base, catalog: c.ID, kind: k, addon: ad.row.ID})
				n++
				break // one search catalog per addon and kind
			}
		}
		if n == 0 && d.Fallback != "" {
			out = append(out, searchSource{base: d.Fallback, catalog: "top", kind: k})
		}
	}
	return out
}

// search asks the sources in parallel within the time budget and merges
// their results: kinds interleaved by rank, each title once (by IMDb id),
// higher-priority addons first.
func (d *Discover) search(ctx context.Context, term string, kinds []string) []found {
	timeout := d.Timeout
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	sources := d.sources(ctx, kinds)
	per := make([][]found, len(sources))
	var wg sync.WaitGroup
	for i, s := range sources {
		wg.Add(1)
		go func() {
			defer wg.Done()
			metas, err := d.catalogSearch(ctx, s, term)
			if err != nil {
				d.Log.Debug("search source failed", "host", Redact(s.base), "kind", s.kind, "err", err)
				return
			}
			for _, m := range metas {
				per[i] = append(per[i], found{Type: s.kind, Meta: m, Addon: s.addon})
			}
		}()
	}
	wg.Wait()

	byKind := map[string][]found{}
	seen := map[string]bool{}
	for _, list := range per {
		for _, f := range list {
			imdb := imdbID(f.Meta)
			if imdb == "" || seen[f.Type+imdb] {
				continue
			}
			seen[f.Type+imdb] = true
			byKind[f.Type] = append(byKind[f.Type], f)
		}
	}
	// Interleave kinds by rank: movie 1, series 1, movie 2…
	var out []found
	for i := 0; ; i++ {
		added := false
		for _, k := range kinds {
			if i < len(byKind[k]) {
				out = append(out, byKind[k][i])
				added = true
			}
		}
		if !added {
			return out
		}
	}
}

// catalogSearch runs one search catalog, cached for cache.SearchTTL.
func (d *Discover) catalogSearch(ctx context.Context, s searchSource, term string) ([]Meta, error) {
	key := cache.SearchKey(AddonKey(s.base)+":"+s.catalog, s.kind, term)
	var metas []Meta
	if d.Cache != nil {
		if ok, err := d.Cache.GetJSON(ctx, key, &metas); err == nil && ok {
			return metas, nil
		}
	}
	metas, err := d.Registry.Client.Catalog(ctx, s.base, s.kind, s.catalog, Extra{Search: term})
	if err != nil {
		return nil, err
	}
	if metas == nil {
		metas = []Meta{}
	}
	if d.Cache != nil {
		_ = d.Cache.SetJSON(ctx, key, metas, cache.SearchTTL)
	}
	return metas, nil
}

// imdbID is a result's IMDb id, or "" (only IMDb-keyed titles are kept:
// stream addons look titles up by them).
func imdbID(m Meta) string {
	if strings.HasPrefix(m.ID, "tt") {
		return m.ID
	}
	if strings.HasPrefix(m.IMDbID, "tt") {
		return m.IMDbID
	}
	return ""
}

// store returns the results as items: the library's own where the title is
// in one, else the discover copy (upserted now).
func (d *Discover) store(ctx context.Context, results []found, types []string, limit int) ([]db.Item, error) {
	q := d.Registry.q()
	imdbs := make([]string, 0, len(results))
	for _, f := range results {
		imdbs = append(imdbs, imdbID(f.Meta))
	}
	have, err := q.LibraryItemsByImdb(ctx, db.LibraryItemsByImdbParams{Types: types, Imdb: imdbs})
	if err != nil {
		return nil, err
	}
	inLibrary := map[string]db.Item{}
	for _, it := range have {
		var ids map[string]string
		_ = json.Unmarshal(it.ProviderIds, &ids)
		if key := it.Type + ids["Imdb"]; inLibrary[key].ID == uuid.Nil {
			inLibrary[key] = it
		}
	}
	lib, err := d.library(ctx)
	if err != nil {
		return nil, err
	}
	w := &catalogWriter{s: &Syncer{Registry: d.Registry, Log: d.Log}, q: q, lib: lib}

	var order, fresh []uuid.UUID
	byID := map[uuid.UUID]db.Item{}
	for _, f := range results {
		if len(order) >= limit {
			break
		}
		typ := "Movie"
		if f.Type == "series" {
			typ = "Series"
		}
		imdb := imdbID(f.Meta)
		if it, ok := inLibrary[typ+imdb]; ok {
			if _, dup := byID[it.ID]; !dup {
				byID[it.ID] = it
				order = append(order, it.ID)
			}
			continue
		}
		id, err := d.upsert(ctx, w, f, typ, imdb)
		if err != nil {
			return nil, err
		}
		if _, dup := byID[id]; !dup {
			byID[id] = db.Item{}
			order = append(order, id)
			fresh = append(fresh, id)
		}
	}
	if len(fresh) > 0 {
		rows, err := q.GetItemsByIDs(ctx, fresh)
		if err != nil {
			return nil, err
		}
		for _, it := range rows {
			byID[it.ID] = it
		}
		d.triggerRefresh()
	}
	out := make([]db.Item, 0, len(order))
	for _, id := range order {
		if it := byID[id]; it.ID != uuid.Nil {
			out = append(out, it)
		}
	}
	return out, nil
}

func (d *Discover) upsert(ctx context.Context, w *catalogWriter, f found, typ, imdb string) (uuid.UUID, error) {
	m := f.Meta
	m.ID = imdb
	name := strings.TrimSpace(m.Name)
	if name == "" {
		name = imdb
	}
	var year *int32
	if y := m.Year(); y > 0 {
		year = ptr(int32(y))
	}
	w.addon = f.Addon
	res, err := w.q.UpsertDiscoverItem(ctx, db.UpsertDiscoverItemParams{
		ID: DiscoverID(imdb), LibraryID: w.lib.ID, Type: typ, Name: name, SortName: library.SortName(name),
		Path: ptr("stremio:" + f.Type + ":" + imdb), StremioRef: w.ref(f.Type, imdb),
		ProductionYear: year, PremiereDate: date(m.Released), Overview: optString(m.Description), ProviderIds: providerIDs(m),
	})
	if err != nil {
		return uuid.Nil, err
	}
	if res.CatalogOwned {
		if err := w.artwork(ctx, res.ID, map[string]string{"Primary": m.Poster, "Backdrop": m.Background, "Logo": m.Logo}); err != nil {
			return uuid.Nil, err
		}
	}
	return res.ID, nil
}

// EnsureEpisodes implements handlers.RemoteSearch: a discover series gets
// its seasons and episodes from an addon's meta when first opened, and
// again at most every 12 hours (new episodes). Other items are left alone.
func (d *Discover) EnsureEpisodes(ctx context.Context, series db.Item) error {
	if series.Type != "Series" || series.SourceKind != "stremio" || series.Path == nil {
		return nil
	}
	lib, err := d.library(ctx)
	if err != nil || series.LibraryID != lib.ID {
		return err
	}
	id, ok := strings.CutPrefix(*series.Path, "stremio:series:")
	if !ok {
		return nil
	}
	key := cache.Key("discover:eps:" + series.ID.String())
	if d.Cache != nil {
		var done bool
		if ok, err := d.Cache.GetJSON(ctx, key, &done); err == nil && ok {
			return nil
		}
	}
	_, err, _ = d.flight.Do(series.ID.String(), func() (any, error) {
		ctx := context.WithoutCancel(ctx)
		_, addons, err := d.Registry.openAddons(ctx, d.Log)
		if err != nil {
			return nil, err
		}
		if d.Fallback != "" {
			cm := Manifest{Types: []string{"movie", "series"}, IDPrefixes: []string{"tt"}, Resources: []Resource{{Name: "meta"}}}
			addons = append(addons, addonInfo{base: d.Fallback, manifest: cm})
		}
		w := &catalogWriter{s: &Syncer{Registry: d.Registry, Log: d.Log}, q: d.Registry.q(), lib: lib}
		meta, err := w.fetchMeta(ctx, id, addons)
		if err != nil {
			return nil, err
		}
		n, err := w.seriesEpisodes(ctx, series.ID, meta)
		if err != nil {
			return nil, err
		}
		d.Log.Info("discover series episodes", "series", series.Name, "episodes", n)
		if d.Cache != nil {
			_ = d.Cache.SetJSON(ctx, key, true, 12*time.Hour)
		}
		d.triggerRefresh()
		return nil, nil
	})
	return err
}

func (d *Discover) triggerRefresh() {
	d.mu.Lock()
	ch := d.refresh
	d.mu.Unlock()
	if d.Refresh == nil || ch == nil {
		return
	}
	select {
	case ch <- struct{}{}:
	default: // one is already pending
	}
}

// Run enriches new titles as they're found and cleans up stale ones every
// hour, until ctx ends.
func (d *Discover) Run(ctx context.Context) {
	ch := make(chan struct{}, 1)
	d.mu.Lock()
	d.refresh = ch
	d.mu.Unlock()
	retention := d.Retention
	if retention <= 0 {
		retention = 7 * 24 * time.Hour
	}
	gc := time.NewTicker(time.Hour)
	defer gc.Stop()
	d.cleanup(ctx, retention)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ch:
			lib, err := d.library(ctx)
			if err == nil && d.Refresh != nil {
				err = d.Refresh(ctx, lib)
			}
			if err != nil && !errors.Is(err, context.Canceled) {
				d.Log.Warn("discover metadata refresh failed", "err", err)
			}
		case <-gc.C:
			d.cleanup(ctx, retention)
		}
	}
}

// Cleanup drops titles not found by a search within retention that nobody
// played, favourited or started; it returns how many went.
func (d *Discover) Cleanup(ctx context.Context, retention time.Duration) (int64, error) {
	return d.Registry.q().DeleteStaleDiscoverItems(ctx, ptr(time.Now().Add(-retention)))
}

func (d *Discover) cleanup(ctx context.Context, retention time.Duration) {
	n, err := d.Cleanup(ctx, retention)
	switch {
	case err != nil && !errors.Is(err, context.Canceled):
		d.Log.Warn("discover cleanup failed", "err", err)
	case n > 0:
		d.Log.Info("discover titles cleaned up", "titles", n)
	}
}
