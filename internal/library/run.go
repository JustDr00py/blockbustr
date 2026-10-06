package library

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/sysadmin/blockbustr/internal/events"
	"github.com/sysadmin/blockbustr/internal/store/pg/db"
)

// MetadataRefresher fills in metadata after a scan (metadata.Refresher).
type MetadataRefresher interface {
	Refresh(ctx context.Context, lib db.Library) error
}

// SetMetadata makes every successful scan follow up with a metadata refresh.
func (s *Scanner) SetMetadata(m MetadataRefresher) { s.AddPostScan("metadata", m.Refresh) }

type postScan struct {
	name string
	fn   func(context.Context, db.Library) error
}

// AddPostScan registers work to run, in order, after every successful scan
// of a library (metadata refresh, image prefetch, …).
func (s *Scanner) AddPostScan(name string, fn func(context.Context, db.Library) error) {
	s.post = append(s.post, postScan{name, fn})
}

// Trigger asks the running scanner to scan every library now. It never
// blocks; triggers while a scan is pending are merged.
func (s *Scanner) Trigger() {
	s.mu.Lock()
	s.wantAll = true
	s.mu.Unlock()
	s.signal()
}

// TriggerLibrary asks the running scanner to scan one library (the file
// watcher's request). Merged like Trigger.
func (s *Scanner) TriggerLibrary(id uuid.UUID) {
	s.mu.Lock()
	if s.want == nil {
		s.want = map[uuid.UUID]bool{}
	}
	s.want[id] = true
	s.mu.Unlock()
	s.signal()
}

func (s *Scanner) signal() {
	select {
	case s.trigger <- struct{}{}:
	default:
	}
}

// takePending returns the libraries triggered since the last call, in libs
// order, and clears the requests.
func (s *Scanner) takePending(libs []db.Library) []db.Library {
	s.mu.Lock()
	defer s.mu.Unlock()
	all, want := s.wantAll, s.want
	s.wantAll, s.want = false, nil
	if all {
		return libs
	}
	var out []db.Library
	for _, lib := range libs {
		if want[lib.ID] {
			out = append(out, lib)
		}
	}
	return out
}

// ScanAll scans libraries one after another, logging failures.
func (s *Scanner) ScanAll(ctx context.Context, libs []db.Library) {
	for _, lib := range libs {
		res, err := s.Scan(ctx, lib)
		if err != nil {
			if errors.Is(err, ErrScanInProgress) {
				s.log.Info("scan skipped: already running elsewhere", "library", lib.Name)
				continue
			}
			if ctx.Err() != nil {
				return
			}
			s.log.Error("library scan failed", "library", lib.Name, "err", err)
			continue
		}
		for _, p := range s.post {
			if err := p.fn(ctx, lib); err != nil && ctx.Err() == nil {
				s.log.Error("post-scan step failed", "step", p.name, "library", lib.Name, "err", err)
			}
		}
		// Announced after metadata and artwork, so clients refetch whole items.
		if res.changed() {
			s.events.Publish(ctx, events.Event{Kind: events.LibraryChanged, Libraries: []uuid.UUID{lib.ID}})
		}
	}
}

// Run scans at startup (if configured), every scan.interval, and whenever
// Trigger is called, until ctx ends.
func (s *Scanner) Run(ctx context.Context, libs []db.Library) {
	if len(libs) == 0 {
		s.log.Warn("no libraries configured; add `libraries:` to config.yaml")
		return
	}
	if s.cfg.OnStart {
		s.ScanAll(ctx, libs)
	}
	var tick <-chan time.Time
	if s.cfg.Interval > 0 {
		t := time.NewTicker(s.cfg.Interval)
		defer t.Stop()
		tick = t.C
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick:
			s.ScanAll(ctx, libs)
		case <-s.trigger:
			s.ScanAll(ctx, s.takePending(libs))
		}
	}
}
