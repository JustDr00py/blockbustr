// Package library builds the item hierarchy from media folders (DESIGN §6):
// walk, classify, parse names, upsert Movies or Series → Season → Episode,
// probe local files, and retire items that disappeared.
package library

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/sync/singleflight"

	"github.com/sysadmin/blockbustr/internal/cache"
	"github.com/sysadmin/blockbustr/internal/config"
	"github.com/sysadmin/blockbustr/internal/events"
	"github.com/sysadmin/blockbustr/internal/media"
	"github.com/sysadmin/blockbustr/internal/store/pg/db"
	"github.com/sysadmin/blockbustr/internal/strm"
)

// ErrScanInProgress means another scan of the same library holds the lock.
var ErrScanInProgress = errors.New("library: scan already in progress")

// Prober is what the scanner needs from media.Prober (stubbed in tests).
type Prober interface {
	Probe(ctx context.Context, target string, opts media.Options) (*media.Info, error)
}

// Scanner scans libraries into the database.
type Scanner struct {
	q      *db.Queries
	pool   *pgxpool.Pool
	cache  *cache.Cache
	prober Prober
	cfg    config.Scan
	log    *slog.Logger
	post   []postScan // see AddPostScan

	trigger chan struct{}
	mu      sync.Mutex
	wantAll bool               // Trigger: scan every library
	want    map[uuid.UUID]bool // TriggerLibrary: scan these
	remote  singleflight.Group // ProbeRemote, one probe per item at a time
	events  *events.Bus        // LibraryChanged after scans and probes; nil = none
}

// SetEvents makes scans and first-play probes announce library changes.
func (s *Scanner) SetEvents(b *events.Bus) { s.events = b }

// NewScanner returns a Scanner.
func NewScanner(pool *pgxpool.Pool, c *cache.Cache, p Prober, cfg config.Scan, log *slog.Logger) *Scanner {
	return &Scanner{q: db.New(pool), pool: pool, cache: c, prober: p, cfg: cfg, log: log, trigger: make(chan struct{}, 1)}
}

// Result summarises one library scan.
type Result struct {
	Library      string
	Files        int // playable files found
	Skipped      int // extras, unreadable .strm files
	Probed       int
	ProbeFailed  int
	RemoteSource int // .strm items given an (unprobed) remote source
	Missing      int64
	Deleted      int64
	Took         time.Duration
}

// changed reports whether the scan added, re-read or lost anything.
func (r Result) changed() bool {
	return r.Probed+r.ProbeFailed+r.RemoteSource > 0 || r.Missing > 0 || r.Deleted > 0
}

var videoExts = map[string]bool{
	".mkv": true, ".mp4": true, ".m4v": true, ".avi": true, ".mov": true, ".wmv": true, ".webm": true,
	".ts": true, ".m2ts": true, ".mts": true, ".mpg": true, ".mpeg": true, ".flv": true, ".ogv": true,
}

// skipDir names are never descended into (NAS metadata, recycle bins).
var skipDir = map[string]bool{"@eadir": true, "#recycle": true, "#snapshot": true, "lost+found": true, "$recycle.bin": true}

// SyncLibraries makes the database libraries match the configuration (by
// name) and returns them in configuration order.
func (s *Scanner) SyncLibraries(ctx context.Context, libs []config.Library) ([]db.Library, error) {
	out := make([]db.Library, 0, len(libs))
	for _, l := range libs {
		lib, err := s.q.UpsertLibrary(ctx, db.UpsertLibraryParams{Name: l.Name, Kind: l.Kind, Paths: l.Paths})
		if err != nil {
			return nil, fmt.Errorf("library %s: %w", l.Name, err)
		}
		if _, err := s.q.EnsureCollectionFolder(ctx, db.EnsureCollectionFolderParams{
			LibraryID: lib.ID, Name: lib.Name, SortName: SortName(lib.Name),
		}); err != nil {
			return nil, fmt.Errorf("library %s: %w", l.Name, err)
		}
		out = append(out, lib)
	}
	keep := make([]uuid.UUID, len(out))
	for i, l := range out {
		keep[i] = l.ID
	}
	if n, err := s.q.DisableLibrariesExcept(ctx, keep); err != nil {
		return nil, err
	} else if n > 0 {
		s.log.Warn("libraries no longer in config disabled (their items are kept)", "count", n)
	}
	return out, nil
}

// Scan scans one library. Only one scan per library runs at a time across
// all instances (Redis lock).
func (s *Scanner) Scan(ctx context.Context, lib db.Library) (Result, error) {
	res := Result{Library: lib.Name}
	start := time.Now()
	lock, ok, err := s.cache.TryLock(ctx, cache.ScanLockKey(lib.ID), cache.ScanLockTTL)
	if err != nil {
		return res, err
	}
	if !ok {
		return res, ErrScanInProgress
	}
	stopRefresh := keepLocked(ctx, lock, s.log)
	defer func() {
		stopRefresh()
		if err := lock.Unlock(context.WithoutCancel(ctx)); err != nil && !errors.Is(err, cache.ErrLockNotHeld) {
			s.log.Warn("scan unlock failed", "library", lib.Name, "err", err)
		}
	}()

	// Seen-ness is compared with timestamps the database sets, so take the
	// scan start from the database clock too.
	scanStart, err := s.q.DBNow(ctx)
	if err != nil {
		return res, err
	}
	folder, err := s.q.EnsureCollectionFolder(ctx, db.EnsureCollectionFolderParams{LibraryID: lib.ID, Name: lib.Name, SortName: SortName(lib.Name)})
	if err != nil {
		return res, err
	}
	w := walker{s: s, lib: lib, folder: folder, res: &res, series: map[string]uuid.UUID{}, seasons: map[string]uuid.UUID{}}
	for _, root := range lib.Paths {
		if err := w.walk(ctx, root); err != nil {
			return res, err
		}
	}
	if err := s.refreshSources(ctx, lib, &res); err != nil {
		return res, err
	}

	// Anything the walk didn't touch is gone from disk: hide it now, delete it
	// once it has been missing longer than the grace period.
	if res.Missing, err = s.q.MarkUnseenMissing(ctx, db.MarkUnseenMissingParams{LibraryID: lib.ID, ScanStart: &scanStart}); err != nil {
		return res, err
	}
	cutoff := scanStart.Add(-s.cfg.MissingGrace)
	if res.Deleted, err = s.q.DeleteMissingBefore(ctx, db.DeleteMissingBeforeParams{LibraryID: lib.ID, Cutoff: &cutoff}); err != nil {
		return res, err
	}
	if _, err := s.q.DeleteEmptySeasons(ctx, lib.ID); err != nil {
		return res, err
	}
	if _, err := s.q.DeleteEmptySeries(ctx, lib.ID); err != nil {
		return res, err
	}
	res.Took = time.Since(start)
	s.log.Info("library scanned", "library", lib.Name, "files", res.Files, "probed", res.Probed, "probe_failed", res.ProbeFailed,
		"remote", res.RemoteSource, "skipped", res.Skipped, "missing", res.Missing, "deleted", res.Deleted, "took", res.Took.Round(time.Millisecond))
	return res, nil
}

// keepLocked refreshes the scan lock until stopped, for scans longer than its TTL.
func keepLocked(ctx context.Context, lock *cache.Lock, log *slog.Logger) (stop func()) {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(cache.ScanLockTTL / 3)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := lock.Refresh(ctx, cache.ScanLockTTL); err != nil {
					log.Warn("scan lock refresh failed", "err", err)
				}
			}
		}
	}()
	return func() { cancel(); <-done }
}

type walker struct {
	s       *Scanner
	lib     db.Library
	folder  uuid.UUID
	res     *Result
	series  map[string]uuid.UUID // series folder → item
	seasons map[string]uuid.UUID // series id + number → item
}

func (w *walker) walk(ctx context.Context, root string) error {
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if path == root {
				return fmt.Errorf("library %s: %w", w.lib.Name, err)
			}
			w.s.log.Warn("unreadable path skipped", "path", path, "err", err)
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		name := d.Name()
		if d.IsDir() {
			if path != root && (strings.HasPrefix(name, ".") || skipDir[strings.ToLower(name)]) {
				return filepath.SkipDir
			}
			return nil
		}
		ext := strings.ToLower(filepath.Ext(name))
		if !videoExts[ext] && ext != ".strm" || strings.HasPrefix(name, ".") {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		if strm.IsExtra(rel) {
			w.res.Skipped++
			return nil
		}
		if err := w.file(ctx, root, rel, path, d); err != nil {
			if errors.Is(err, errSkip) {
				w.res.Skipped++
				return nil
			}
			return err
		}
		w.res.Files++
		return nil
	})
}

var errSkip = errors.New("skip file")

// file upserts one playable file (and its Series/Season for TV).
func (w *walker) file(ctx context.Context, root, rel, path string, d fs.DirEntry) error {
	kind, strmURL, etag, err := w.identify(path, d)
	if err != nil {
		w.s.log.Warn("file skipped", "path", path, "err", err)
		return errSkip
	}
	base := filepath.Base(path)
	parsed := strm.Parse(base)
	if w.lib.Kind == "tvshows" {
		return w.episode(ctx, root, rel, path, base, parsed, kind, strmURL, etag)
	}
	title, year := parsed.Title, parsed.Year
	if year == 0 || title == "" {
		// "Luca (2021)/luca.mkv": the folder carries the title and year.
		if t, y := folderTitle(filepath.Base(filepath.Dir(path))); y != 0 && filepath.Dir(path) != filepath.Clean(root) {
			title, year = t, y
		}
	}
	if title == "" {
		title = strings.TrimSuffix(base, filepath.Ext(base))
	}
	_, err = w.s.q.UpsertPathItem(ctx, db.UpsertPathItemParams{
		LibraryID: w.lib.ID, ParentID: &w.folder, TopParentID: &w.folder,
		Type: "Movie", Name: title, SortName: SortName(title), SourceKind: kind,
		Path: &path, StrmUrl: strmURL, ProductionYear: optInt(year), Etag: &etag,
	})
	return err
}

func (w *walker) episode(ctx context.Context, root, rel, path, base string, p strm.Parsed, kind string, strmURL *string, etag string) error {
	parts := strings.Split(filepath.ToSlash(rel), "/")
	var seriesPath, seriesName string
	var seriesYear int
	if len(parts) > 1 {
		seriesPath = filepath.Join(root, parts[0])
		seriesName, seriesYear = folderTitle(parts[0])
	} else {
		// A file directly in the library root: group it by its parsed show
		// title under a virtual series path.
		seriesName = p.ShowTitle
		if seriesName == "" {
			seriesName = p.Title
		}
		seriesPath, seriesYear = filepath.Join(root, seriesName), p.Year
	}
	seriesID, ok := w.series[seriesPath]
	if !ok {
		id, err := w.s.q.UpsertPathItem(ctx, db.UpsertPathItemParams{
			LibraryID: w.lib.ID, ParentID: &w.folder, TopParentID: &w.folder,
			Type: "Series", Name: seriesName, SortName: SortName(seriesName), SourceKind: "virtual",
			Path: &seriesPath, ProductionYear: optInt(seriesYear),
		})
		if err != nil {
			return err
		}
		seriesID, w.series[seriesPath] = id, id
	}

	season := 1
	switch n, ok := seasonFromDir(rel); {
	case ok:
		season = n
	case p.Specials:
		season = 0
	case p.Kind == strm.KindTV && !p.SeasonAssumed:
		season = p.Season
	}
	seasonKey := fmt.Sprintf("%s#%d", seriesID, season)
	seasonID, ok := w.seasons[seasonKey]
	if !ok {
		id, err := w.s.q.UpsertSeason(ctx, db.UpsertSeasonParams{
			LibraryID: w.lib.ID, ParentID: &seriesID, TopParentID: &w.folder,
			Name: seasonName(season), SortName: fmt.Sprintf("%04d", season), IndexNumber: optInt32(season),
		})
		if err != nil {
			return err
		}
		seasonID, w.seasons[seasonKey] = id, id
	}

	ep := p.Episode
	if ep == 0 {
		ep = strm.EpisodeFromFileName(base)
	}
	name := episodeTitle(base)
	if name == "" && ep > 0 {
		name = fmt.Sprintf("Episode %d", ep)
	}
	if name == "" {
		name = strings.TrimSuffix(base, filepath.Ext(base))
	}
	_, err := w.s.q.UpsertPathItem(ctx, db.UpsertPathItemParams{
		LibraryID: w.lib.ID, ParentID: &seasonID, TopParentID: &w.folder,
		Type: "Episode", Name: name, SortName: fmt.Sprintf("%04d %s", ep, SortName(name)), SourceKind: kind,
		Path: &path, StrmUrl: strmURL, IndexNumber: optInt(ep), ParentIndexNumber: optInt32(season), Etag: &etag,
	})
	return err
}

// identify classifies a file and computes its etag: size+mtime for media
// files, a hash of the target for .strm (the URL itself must not leak into
// etags, which clients see).
func (w *walker) identify(path string, d fs.DirEntry) (kind string, strmURL *string, etag string, err error) {
	info, err := d.Info()
	if err != nil {
		return "", nil, "", err
	}
	if strings.EqualFold(filepath.Ext(path), ".strm") {
		target, err := strm.ReadFile(path)
		if err != nil {
			return "", nil, "", err
		}
		sum := sha256.Sum256([]byte(target))
		return "strm", &target, "strm-" + hex.EncodeToString(sum[:8]) + sidecarSignature(path), nil
	}
	return "file", nil, fmt.Sprintf("%x-%x", info.Size(), info.ModTime().UnixNano()) + sidecarSignature(path), nil
}

// refreshSources builds media sources for items that lack an up-to-date
// one: .strm items get an unprobed remote source (probed at first play,
// DESIGN §6); local files are probed in a worker pool.
func (s *Scanner) refreshSources(ctx context.Context, lib db.Library, res *Result) error {
	rows, err := s.q.ItemsNeedingSource(ctx, lib.ID)
	if err != nil {
		return err
	}
	jobs := make(chan db.ItemsNeedingSourceRow)
	var mu sync.Mutex
	var firstErr error
	workers := s.cfg.ProbeWorkers
	if workers <= 0 {
		workers = max(1, runtime.NumCPU()/2)
	}
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for row := range jobs {
				ok, err := s.probeFile(ctx, row)
				mu.Lock()
				switch {
				case err != nil && firstErr == nil:
					firstErr = err
				case ok:
					res.Probed++
				case err == nil:
					res.ProbeFailed++
				}
				mu.Unlock()
			}
		}()
	}
	for _, row := range rows {
		if row.SourceKind == "strm" {
			if err := s.storeSource(ctx, row, remoteSource(row), nil); err != nil {
				close(jobs)
				wg.Wait()
				return err
			}
			res.RemoteSource++
			continue
		}
		jobs <- row
	}
	close(jobs)
	wg.Wait()
	return firstErr
}

func remoteSource(row db.ItemsNeedingSourceRow) db.InsertMediaSourceParams {
	return db.InsertMediaSourceParams{
		ItemID: row.ID, Name: stem(*row.Path), PathOrUrl: *row.StrmUrl, Protocol: "Http", IsRemote: true,
		Container: optStr(containerFromURL(*row.StrmUrl)), Etag: row.Etag,
	}
}

// probeFile probes a local file and stores the result. ok=false with a nil
// error means ffprobe failed; the failure is recorded on the source so the
// file isn't retried until it changes.
func (s *Scanner) probeFile(ctx context.Context, row db.ItemsNeedingSourceRow) (bool, error) {
	pctx, cancel := context.WithTimeout(ctx, s.cfg.ProbeTimeout)
	defer cancel()
	info, perr := s.prober.Probe(pctx, *row.Path, media.Options{})
	src := db.InsertMediaSourceParams{
		ItemID: row.ID, Name: stem(*row.Path), PathOrUrl: *row.Path, Protocol: "File", Etag: row.Etag,
	}
	now := time.Now()
	src.ProbedAt = &now
	if perr != nil {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		s.log.Warn("probe failed", "path", *row.Path, "err", perr)
		msg := perr.Error()
		src.ProbeError = &msg
		if fi, err := os.Stat(*row.Path); err == nil {
			src.Size = ptr(fi.Size())
		}
		return false, s.storeSource(ctx, row, src, nil)
	}
	src.Container = optStr(containerName(info.Container))
	src.Size = optInt64(info.Size)
	src.Bitrate = optInt32(int(min(info.Bitrate, 1<<31-1)))
	if info.Duration > 0 {
		src.RuntimeTicks = ptr(int64(info.Duration / 100))
	}
	return true, s.storeSource(ctx, row, src, info)
}

// storeSource replaces an item's media source, streams and chapters in one
// transaction.
func (s *Scanner) storeSource(ctx context.Context, row db.ItemsNeedingSourceRow, src db.InsertMediaSourceParams, info *media.Info) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		if err := q.DeleteMediaSources(ctx, row.ID); err != nil {
			return err
		}
		id, err := q.InsertMediaSource(ctx, src)
		if err != nil {
			return err
		}
		if err := q.DeleteChapters(ctx, row.ID); err != nil {
			return err
		}
		if info == nil {
			return nil
		}
		if row.Path != nil {
			info = withSidecars(info, *row.Path)
		}
		for _, st := range info.Streams {
			if err := q.InsertMediaStream(ctx, streamParams(id, st)); err != nil {
				return err
			}
		}
		for i, ch := range info.Chapters {
			if err := q.InsertChapter(ctx, db.InsertChapterParams{ItemID: row.ID, Idx: int32(i), StartTicks: int64(ch.Start / 100), Name: ch.Title}); err != nil {
				return err
			}
		}
		return q.SetItemRuntime(ctx, db.SetItemRuntimeParams{ID: row.ID, RuntimeTicks: src.RuntimeTicks})
	})
}

func streamParams(source uuid.UUID, st media.Stream) db.InsertMediaStreamParams {
	p := db.InsertMediaStreamParams{
		MediaSourceID: source, Idx: int32(st.Index), Type: string(st.Type),
		Codec: optStr(st.Codec), Language: optStr(st.Language), Title: optStr(st.Title), Profile: optStr(st.Profile),
		IsDefault: st.IsDefault, IsForced: st.IsForced, IsHearingImpaired: st.IsHearingImpaired, IsOriginal: st.IsOriginal,
		Width: optInt32(st.Width), Height: optInt32(st.Height), Bitrate: optInt32(int(min(st.Bitrate, 1<<31-1))),
		Channels: optInt32(st.Channels), ChannelLayout: optStr(st.ChannelLayout), SampleRate: optInt32(st.SampleRate),
		VideoRange: optStr(st.VideoRange), VideoRangeType: optStr(st.VideoRangeType), PixelFormat: optStr(st.PixelFormat),
		BitDepth: optInt32(st.BitDepth), AspectRatio: optStr(st.AspectRatio), IsInterlaced: st.IsInterlaced,
		ColorTransfer: optStr(st.ColorTransfer), ColorPrimaries: optStr(st.ColorPrimaries), ColorSpace: optStr(st.ColorSpace),
		ColorRange: optStr(st.ColorRange), TimeBase: optStr(st.TimeBase), ExternalPath: optStr(st.Path),
	}
	if st.Level != 0 {
		p.Level = ptr(float32(st.Level))
	}
	if st.AverageFrameRate > 0 {
		p.AverageFrameRate = ptr(float32(st.AverageFrameRate))
	}
	if st.RealFrameRate > 0 {
		p.RealFrameRate = ptr(float32(st.RealFrameRate))
	}
	if st.DoVi != nil {
		p.DvProfile, p.DvLevel, p.DvBlCompatID = ptr(int32(st.DoVi.Profile)), ptr(int32(st.DoVi.Level)), ptr(int32(st.DoVi.BLSignalCompatibilityID))
		p.DvVersionMajor, p.DvVersionMinor = ptr(int32(st.DoVi.VersionMajor)), ptr(int32(st.DoVi.VersionMinor))
		p.DvRpuPresent, p.DvElPresent, p.DvBlPresent = ptr(st.DoVi.RPU), ptr(st.DoVi.EL), ptr(st.DoVi.BL)
	}
	return p
}

// containerFromURL guesses a remote source's container from the URL path
// ("…/Luca.mkv?sig=…" → "mkv"); probing at first play fills it in properly.
func containerFromURL(u string) string {
	p, _, _ := strings.Cut(u, "?")
	switch ext := strings.TrimPrefix(strings.ToLower(filepath.Ext(p)), "."); ext {
	case "mkv", "mp4", "m4v", "avi", "mov", "ts", "webm":
		return ext
	}
	return ""
}

func stem(path string) string {
	b := filepath.Base(path)
	return strings.TrimSuffix(b, filepath.Ext(b))
}

func ptr[T any](v T) *T { return &v }

func optStr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func optInt(n int) *int32 {
	if n == 0 {
		return nil
	}
	return ptr(int32(n))
}

// optInt32 keeps zero (season 0, specials) but maps negatives to nil.
func optInt32(n int) *int32 {
	if n < 0 {
		return nil
	}
	if n == 0 {
		return ptr(int32(0))
	}
	return ptr(int32(n))
}

func optInt64(n int64) *int64 {
	if n == 0 {
		return nil
	}
	return &n
}
