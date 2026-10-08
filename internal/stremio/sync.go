package stremio

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"golang.org/x/sync/errgroup"

	"github.com/sysadmin/blockbustr/internal/cache"
	"github.com/sysadmin/blockbustr/internal/events"
	"github.com/sysadmin/blockbustr/internal/images"
	"github.com/sysadmin/blockbustr/internal/library"
	"github.com/sysadmin/blockbustr/internal/store/pg/db"
)

// CollectionType is the Jellyfin collection type for a catalog type, or ""
// for types blockbustr can't present as a library (anime, tv, channel…).
func CollectionType(catalogType string) string {
	switch catalogType {
	case "movie":
		return "movies"
	case "series":
		return "tvshows"
	}
	return ""
}

// Sync defaults (DESIGN §7.2).
const (
	DefaultPages        = 2
	DefaultMissingGrace = 24 * time.Hour
	metaWorkers         = 4
)

// Syncer turns enabled catalogs into stremio libraries (DESIGN §7.2): one
// library per catalog, items upserted from the catalog's first pages,
// series episodes from meta.videos, then TMDB enrichment.
type Syncer struct {
	Registry *Registry
	// Cache holds the per-library lock (shared with the file scanner's);
	// nil runs without one.
	Cache *cache.Cache
	Log   *slog.Logger
	// Pages of each catalog to sync; 0 means DefaultPages.
	Pages int
	// MissingGrace keeps titles that left a catalog before deleting them;
	// 0 means DefaultMissingGrace.
	MissingGrace time.Duration
	// Refresh enriches a library's new items (metadata.Refresher.Refresh);
	// nil skips enrichment.
	Refresh func(context.Context, db.Library) error
	// Events announces changed libraries; nil drops them.
	Events *events.Bus

	once    sync.Once
	trigger chan struct{}
}

func (s *Syncer) init() { s.once.Do(func() { s.trigger = make(chan struct{}, 1) }) }

// Trigger asks Run for a sync soon (after an addon or catalog changed).
func (s *Syncer) Trigger() {
	s.init()
	select {
	case s.trigger <- struct{}{}:
	default:
	}
}

// Run syncs at start, every interval (0: never on a timer), and on
// Trigger, until ctx ends.
func (s *Syncer) Run(ctx context.Context, interval time.Duration) {
	s.init()
	var tick <-chan time.Time
	if interval > 0 {
		t := time.NewTicker(interval)
		defer t.Stop()
		tick = t.C
	}
	for {
		if err := s.SyncAll(ctx); err != nil && ctx.Err() == nil {
			s.Log.Warn("catalog sync failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick:
		case <-s.trigger:
		}
	}
}

// addonInfo is an addon opened for one sync.
type addonInfo struct {
	row      db.StremioAddon
	base     string
	manifest Manifest
}

// SyncAll syncs every enabled catalog and switches off the libraries of
// catalogs (or addons) that were disabled or removed. One catalog failing
// doesn't stop the others.
func (s *Syncer) SyncAll(ctx context.Context) error {
	q := s.Registry.q()
	orphans, err := q.DisableOrphanStremioLibraries(ctx)
	if err != nil {
		return err
	}
	if len(orphans) > 0 {
		s.Log.Info("stremio libraries of removed catalogs switched off", "libraries", orphans)
	}
	rows, err := q.ListSyncCatalogs(ctx)
	if err != nil || len(rows) == 0 {
		return err
	}
	addons, order, err := s.Registry.openAddons(ctx, s.Log)
	if err != nil {
		return err
	}
	var errs []error
	for _, row := range rows {
		if !row.Enabled || !row.AddonEnabled {
			if row.LibraryID != nil {
				if err := q.SetLibraryEnabled(ctx, db.SetLibraryEnabledParams{ID: *row.LibraryID, Enabled: false}); err != nil {
					errs = append(errs, err)
				}
			}
			continue
		}
		ad, ok := addons[row.AddonID]
		if !ok {
			continue // couldn't be opened (logged)
		}
		if err := s.syncCatalog(ctx, ad, metaSources(ad, order), row); err != nil {
			errs = append(errs, fmt.Errorf("catalog %s/%s of %s: %w", row.CatalogType, row.CatalogID, ad.row.Host, err))
		}
	}
	return errors.Join(errs...)
}

// metaSources lists the addons to ask for a series' episodes: the catalog's
// own addon first, then the others by priority.
func metaSources(own addonInfo, order []addonInfo) []addonInfo {
	out := []addonInfo{own}
	for _, a := range order {
		if a.row.ID != own.row.ID {
			out = append(out, a)
		}
	}
	return out
}

// ensureLibrary returns the catalog's library (enabled) and its
// CollectionFolder, making them the first time.
func (s *Syncer) ensureLibrary(ctx context.Context, ad addonInfo, row db.ListSyncCatalogsRow) (db.Library, uuid.UUID, error) {
	q := s.Registry.q()
	folder := func(lib db.Library) (db.Library, uuid.UUID, error) {
		id, err := q.EnsureCollectionFolder(ctx, db.EnsureCollectionFolderParams{LibraryID: lib.ID, Name: lib.Name, SortName: library.SortName(lib.Name)})
		return lib, id, err
	}
	if row.LibraryID != nil {
		lib, err := q.GetLibrary(ctx, *row.LibraryID)
		if err == nil {
			if err := q.SetLibraryEnabled(ctx, db.SetLibraryEnabledParams{ID: lib.ID, Enabled: true}); err != nil {
				return lib, uuid.Nil, err
			}
			return folder(lib)
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return lib, uuid.Nil, err
		}
	}
	opts, _ := json.Marshal(map[string]string{
		"collectionType": CollectionType(row.CatalogType), "addonId": row.AddonID.String(),
		"catalogType": row.CatalogType, "catalogId": row.CatalogID,
	})
	label := row.Name
	if label == "" {
		label = row.CatalogID
	}
	lib, err := q.AdoptStremioLibrary(ctx, db.AdoptStremioLibraryParams{Options: opts, CatalogType: row.CatalogType, CatalogID: row.CatalogID})
	switch {
	case err == nil:
		if err := q.SetLibraryEnabled(ctx, db.SetLibraryEnabledParams{ID: lib.ID, Enabled: true}); err != nil {
			return lib, uuid.Nil, err
		}
		if err := q.SetStremioCatalogLibrary(ctx, db.SetStremioCatalogLibraryParams{
			AddonID: row.AddonID, CatalogType: row.CatalogType, CatalogID: row.CatalogID, LibraryID: &lib.ID,
		}); err != nil {
			return lib, uuid.Nil, err
		}
		s.Log.Info("stremio library adopted", "library", lib.Name, "host", ad.row.Host, "catalog", row.CatalogType+"/"+row.CatalogID)
		return folder(lib)
	case !errors.Is(err, pgx.ErrNoRows):
		return lib, uuid.Nil, err
	}
	name := LibraryName(row.CatalogType, label)
	for i := 1; ; i++ {
		try := name
		if i > 1 {
			try = fmt.Sprintf("%s (%d)", name, i)
		}
		lib, err = q.CreateStremioLibrary(ctx, db.CreateStremioLibraryParams{Name: try, Options: opts})
		if err == nil {
			break
		}
		if !errors.Is(err, pgx.ErrNoRows) || i == 20 {
			return lib, uuid.Nil, err
		}
	}
	if err := q.SetStremioCatalogLibrary(ctx, db.SetStremioCatalogLibraryParams{
		AddonID: row.AddonID, CatalogType: row.CatalogType, CatalogID: row.CatalogID, LibraryID: &lib.ID,
	}); err != nil {
		return lib, uuid.Nil, err
	}
	s.Log.Info("stremio library created", "library", lib.Name, "host", ad.row.Host, "catalog", row.CatalogType+"/"+row.CatalogID)
	return folder(lib)
}

// syncCatalog refreshes one catalog's library.
func (s *Syncer) syncCatalog(ctx context.Context, ad addonInfo, sources []addonInfo, row db.ListSyncCatalogsRow) error {
	if CollectionType(row.CatalogType) == "" {
		return nil // can't be enabled through the API; ignore a stray row
	}
	lib, folder, err := s.ensureLibrary(ctx, ad, row)
	if err != nil {
		return err
	}
	if s.Cache != nil {
		lock, ok, err := s.Cache.TryLock(ctx, cache.ScanLockKey(lib.ID), cache.ScanLockTTL)
		if err != nil {
			return err
		}
		if !ok {
			return nil // another instance is syncing it
		}
		defer func() { _ = lock.Unlock(context.WithoutCancel(ctx)) }()
	}
	start := time.Now()
	q := s.Registry.q()
	dbStart, err := q.DBNow(ctx)
	if err != nil {
		return err
	}
	metas, err := s.page(ctx, ad, row)
	if err != nil {
		return err
	}
	if len(metas) == 0 {
		s.Log.Warn("stremio catalog empty; library left as it was", "library", lib.Name)
		return nil
	}
	w := &catalogWriter{s: s, q: q, lib: lib, folder: &folder, addon: ad.row.ID}
	var series []Meta
	var seriesIDs []uuid.UUID
	for _, m := range metas {
		id, err := w.title(ctx, m, row.CatalogType)
		if err != nil {
			return err
		}
		if row.CatalogType == "series" {
			series, seriesIDs = append(series, m), append(seriesIDs, id)
		}
	}
	if len(series) > 0 {
		w.episodes(ctx, series, seriesIDs, sources)
	}
	missing, err := q.MarkUnseenMissing(ctx, db.MarkUnseenMissingParams{LibraryID: lib.ID, ScanStart: &dbStart})
	if err != nil {
		return err
	}
	grace := s.MissingGrace
	if grace <= 0 {
		grace = DefaultMissingGrace
	}
	deleted, err := q.DeleteMissingBefore(ctx, db.DeleteMissingBeforeParams{LibraryID: lib.ID, Cutoff: ptr(dbStart.Add(-grace))})
	if err != nil {
		return err
	}
	if _, err := q.DeleteEmptySeasons(ctx, lib.ID); err != nil {
		return err
	}
	// Empty series stay: their episodes may come later (P3.15).
	s.Log.Info("stremio catalog synced", "library", lib.Name, "titles", len(metas), "episodes", w.episodeCount,
		"episode_fetch_failed", w.metaFailed, "missing", missing, "deleted", deleted, "took", time.Since(start).Round(time.Millisecond))
	if s.Refresh != nil {
		if err := s.Refresh(ctx, lib); err != nil {
			s.Log.Warn("stremio library metadata refresh failed", "library", lib.Name, "err", err)
		}
	}
	s.Events.Publish(ctx, events.Event{Kind: events.LibraryChanged, Libraries: []uuid.UUID{lib.ID}})
	return nil
}

// page fetches the catalog's first pages (skip by the count so far, since
// page sizes differ between addons), deduplicated by id.
func (s *Syncer) page(ctx context.Context, ad addonInfo, row db.ListSyncCatalogsRow) ([]Meta, error) {
	var def CatalogDef
	for _, d := range ad.manifest.Catalogs {
		if d.Type == row.CatalogType && d.ID == row.CatalogID {
			def = d
		}
	}
	pages := s.Pages
	if pages <= 0 {
		pages = DefaultPages
	}
	seen := map[string]bool{}
	var out []Meta
	skip := 0
	for p := 0; p < pages; p++ {
		got, err := s.Registry.Client.Catalog(ctx, ad.base, row.CatalogType, row.CatalogID, Extra{Skip: skip})
		if err != nil {
			if p == 0 {
				return nil, err
			}
			s.Log.Warn("stremio catalog page failed; syncing the pages before it", "host", ad.row.Host, "page", p+1, "err", err)
			break
		}
		for _, m := range got {
			if m.ID = strings.TrimSpace(m.ID); m.ID != "" && strings.TrimSpace(m.Name) != "" && !seen[m.ID] {
				seen[m.ID] = true
				out = append(out, m)
			}
		}
		if len(got) == 0 || !def.Accepts("skip") {
			break
		}
		skip += len(got)
	}
	return out, nil
}

type catalogWriter struct {
	s      *Syncer
	q      *db.Queries
	lib    db.Library
	folder *uuid.UUID // the library's CollectionFolder; nil for discover
	addon  uuid.UUID

	mu           sync.Mutex
	episodeCount int
	metaFailed   int
}

func (w *catalogWriter) ref(typ, id string) []byte {
	b, _ := json.Marshal(map[string]string{"addonId": w.addon.String(), "type": typ, "id": id})
	return b
}

// providerIDs takes the ids a catalog entry carries: IMDb from a "tt" id,
// TMDB from Cinemeta's moviedb_id.
func providerIDs(m Meta) []byte {
	ids := map[string]string{}
	imdb := m.IMDbID
	if imdb == "" && strings.HasPrefix(m.ID, "tt") {
		imdb = m.ID
	}
	if imdb != "" {
		ids["Imdb"] = imdb
	}
	if n, err := strconv.Atoi(string(m.MovieDBID)); err == nil && n > 0 {
		ids["Tmdb"] = strconv.Itoa(n)
	}
	b, _ := json.Marshal(ids)
	return b
}

// date reads the day of an ISO 8601 timestamp ("2011-04-17T00:00:00.000Z").
func date(s string) *time.Time {
	if len(s) >= 10 {
		if t, err := time.Parse("2006-01-02", s[:10]); err == nil {
			return &t
		}
	}
	return nil
}

func optString(s string) *string {
	if s = strings.TrimSpace(s); s == "" {
		return nil
	}
	return &s
}

func ptr[T any](v T) *T { return &v }

// title upserts a movie or series and, while the catalog still owns its
// metadata, its artwork.
func (w *catalogWriter) title(ctx context.Context, m Meta, catalogType string) (uuid.UUID, error) {
	typ := "Movie"
	if catalogType == "series" {
		typ = "Series"
	}
	var year *int32
	if y := m.Year(); y > 0 {
		year = ptr(int32(y))
	}
	name := strings.TrimSpace(m.Name)
	res, err := w.q.UpsertStremioItem(ctx, db.UpsertStremioItemParams{
		LibraryID: w.lib.ID, ParentID: w.folder, TopParentID: w.folder, Type: typ,
		Name: name, SortName: library.SortName(name), Path: ptr("stremio:" + catalogType + ":" + m.ID),
		StremioRef: w.ref(catalogType, m.ID), ProductionYear: year, PremiereDate: date(m.Released),
		Overview: optString(m.Description), ProviderIds: providerIDs(m),
	})
	if err != nil {
		return uuid.Nil, err
	}
	if res.CatalogOwned {
		err = w.artwork(ctx, res.ID, map[string]string{"Primary": m.Poster, "Backdrop": m.Background, "Logo": m.Logo})
	}
	return res.ID, err
}

// artwork replaces an item's remote images with the catalog's.
func (w *catalogWriter) artwork(ctx context.Context, item uuid.UUID, urls map[string]string) error {
	if err := w.q.DeleteRemoteImages(ctx, item); err != nil {
		return err
	}
	types := make([]string, 0, len(urls))
	for t := range urls {
		types = append(types, t)
	}
	sort.Strings(types)
	for _, typ := range types {
		u := strings.TrimSpace(urls[typ])
		if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
			continue
		}
		if err := w.q.UpsertImage(ctx, db.UpsertImageParams{ItemID: item, Type: typ, Idx: 0, SourceUrl: &u, Tag: images.Tag(u)}); err != nil {
			return err
		}
	}
	return nil
}

// episodes fetches each series' meta (a few at a time) and upserts its
// aired episodes. A series whose meta can't be fetched keeps the episodes
// it had.
func (w *catalogWriter) episodes(ctx context.Context, series []Meta, ids []uuid.UUID, sources []addonInfo) {
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(metaWorkers)
	for i, m := range series {
		seriesID := ids[i]
		g.Go(func() error {
			meta, err := w.fetchMeta(gctx, m.ID, sources)
			if err != nil {
				w.mu.Lock()
				w.metaFailed++
				w.mu.Unlock()
				w.s.Log.Debug("series episodes unavailable", "series", m.Name, "err", err)
				return w.q.TouchStremioEpisodes(gctx, db.TouchStremioEpisodesParams{LibraryID: w.lib.ID, Prefix: "stremio:series:" + m.ID + ":"})
			}
			n, err := w.seriesEpisodes(gctx, seriesID, meta)
			w.mu.Lock()
			w.episodeCount += n
			w.mu.Unlock()
			return err
		})
	}
	if err := g.Wait(); err != nil {
		w.s.Log.Warn("stremio episodes sync failed", "library", w.lib.Name, "err", err)
	}
}

func (w *catalogWriter) fetchMeta(ctx context.Context, id string, sources []addonInfo) (Meta, error) {
	last := ErrNotFound
	for _, a := range sources {
		if !a.manifest.Supports("meta", "series", id) {
			continue
		}
		m, err := w.s.Registry.Client.Meta(ctx, a.base, "series", id)
		if err == nil && len(m.Videos) > 0 {
			return m, nil
		}
		if err != nil {
			last = err
		}
	}
	return Meta{}, last
}

func (w *catalogWriter) seriesEpisodes(ctx context.Context, seriesID uuid.UUID, meta Meta) (int, error) {
	today := time.Now().UTC().Format("2006-01-02")
	seasons := map[int]uuid.UUID{}
	n := 0
	for _, v := range meta.Videos {
		ep := v.EpisodeNumber()
		if v.ID == "" || v.Season < 0 || ep <= 0 {
			continue
		}
		if len(v.Released) >= 10 && v.Released[:10] > today {
			continue // not aired yet: nothing to play
		}
		seasonID, ok := seasons[v.Season]
		if !ok {
			name := fmt.Sprintf("Season %d", v.Season)
			if v.Season == 0 {
				name = "Specials"
			}
			var err error
			seasonID, err = w.q.UpsertSeason(ctx, db.UpsertSeasonParams{
				LibraryID: w.lib.ID, ParentID: &seriesID, TopParentID: w.folder,
				Name: name, SortName: fmt.Sprintf("%04d", v.Season), IndexNumber: ptr(int32(v.Season)),
			})
			if err != nil {
				return n, err
			}
			seasons[v.Season] = seasonID
		}
		name := strings.TrimSpace(v.Title)
		if name == "" {
			name = strings.TrimSpace(v.Name)
		}
		if name == "" {
			name = fmt.Sprintf("Episode %d", ep)
		}
		overview := v.Overview
		if overview == "" {
			overview = v.Description
		}
		res, err := w.q.UpsertStremioItem(ctx, db.UpsertStremioItemParams{
			LibraryID: w.lib.ID, ParentID: &seasonID, TopParentID: w.folder, Type: "Episode",
			Name: name, SortName: fmt.Sprintf("%04d %s", ep, library.SortName(name)), Path: ptr("stremio:series:" + v.ID),
			StremioRef: w.ref("series", v.ID), IndexNumber: ptr(int32(ep)), ParentIndexNumber: ptr(int32(v.Season)),
			PremiereDate: date(v.Released), Overview: optString(overview), ProviderIds: []byte("{}"),
		})
		if err != nil {
			return n, err
		}
		if res.CatalogOwned {
			if err := w.artwork(ctx, res.ID, map[string]string{"Primary": v.Thumbnail}); err != nil {
				return n, err
			}
		}
		n++
	}
	return n, nil
}

// LibraryName is a new catalog library's name: the catalog's own, which is
// what clients show as the library ("Popular Movies"), with the kind added
// when it doesn't say ("Popular" → "Popular Shows"). The addon's name is
// left out ("AIOStreams | name Popular" read badly); a clash gets a number,
// and admins can rename (POST /blockbustr/libraries/{id}).
func LibraryName(catalogType, label string) string {
	label = strings.TrimSpace(label)
	l := strings.ToLower(label)
	has := func(words ...string) bool {
		for _, w := range words {
			if strings.Contains(l, w) {
				return true
			}
		}
		return false
	}
	switch {
	case catalogType == "movie" && !has("movie", "film", "cinema"):
		return label + " Movies"
	case catalogType == "series" && !has("show", "series", "tv"):
		return label + " Shows"
	}
	return label
}
