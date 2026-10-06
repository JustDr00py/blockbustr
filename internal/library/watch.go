package library

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/google/uuid"

	"github.com/sysadmin/blockbustr/internal/store/pg/db"
)

// Filesystem watching (TASKS P1.14b, DESIGN §6 Scanner): a change anywhere
// under a library's paths rescans that library once its files have been
// quiet for scan.watch_delay, so a long copy is one scan, not hundreds.
// inotify isn't recursive, so every folder is watched and new folders are
// added as they appear. Network shares deliver no events; the periodic scan
// covers them.

// partialSuffixes mark files still being written by downloaders and copiers.
var partialSuffixes = []string{".part", ".partial", ".tmp", ".crdownload", ".download", ".!qb", ".!ut"}

// ignoredName reports whether path names something a change to which never
// needs a rescan: hidden files and folders, and partial downloads.
func ignoredName(path string) bool {
	base := filepath.Base(path)
	if strings.HasPrefix(base, ".") {
		return true
	}
	lower := strings.ToLower(base)
	for _, s := range partialSuffixes {
		if strings.HasSuffix(lower, s) {
			return true
		}
	}
	return false
}

// Watch watches libs' folders until ctx ends, asking for a scan of a library
// (TriggerLibrary) after its files change.
func (s *Scanner) Watch(ctx context.Context, libs []db.Library) error {
	return watch(ctx, libs, s.cfg.WatchDelay, s.log, s.TriggerLibrary)
}

type watchRoot struct {
	path string
	lib  uuid.UUID
}

func watch(ctx context.Context, libs []db.Library, delay time.Duration, log *slog.Logger, changed func(uuid.UUID)) error {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer func() { _ = w.Close() }()

	var roots []watchRoot
	names := map[uuid.UUID]string{}
	for _, lib := range libs {
		names[lib.ID] = lib.Name
		for _, p := range lib.Paths {
			roots = append(roots, watchRoot{filepath.Clean(p), lib.ID})
		}
	}
	// The library of the deepest root containing path (roots may nest).
	libOf := func(path string) (uuid.UUID, bool) {
		var best watchRoot
		for _, r := range roots {
			if (path == r.path || strings.HasPrefix(path, r.path+string(filepath.Separator))) && len(r.path) > len(best.path) {
				best = r
			}
		}
		return best.lib, best.path != ""
	}
	full := false // the inotify watch limit was hit; logged once
	addTree := func(dir string) {
		_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			if err != nil || !d.IsDir() {
				return nil // unreadable entries are the scanner's to report
			}
			if p != dir && ignoredName(p) {
				return filepath.SkipDir
			}
			if err := w.Add(p); err != nil {
				if errors.Is(err, syscall.ENOSPC) {
					if !full {
						log.Warn("inotify watch limit reached; raise fs.inotify.max_user_watches. Unwatched folders are still scanned every scan.interval", "path", p)
						full = true
					}
					return filepath.SkipAll
				}
				log.Warn("cannot watch folder; it is still scanned every scan.interval", "path", p, "err", err)
			}
			return nil
		})
	}
	for _, r := range roots {
		addTree(r.path)
	}
	log.Info("watching libraries for changes", "folders", len(w.WatchList()), "delay", delay)

	// due holds each changed library's scan time, pushed back by every new
	// change; a ticker finer than delay fires the ones that are due.
	due := map[uuid.UUID]time.Time{}
	tick := time.NewTicker(max(min(delay/5, time.Second), 10*time.Millisecond))
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case err, ok := <-w.Errors:
			if !ok {
				return nil
			}
			if errors.Is(err, fsnotify.ErrEventOverflow) { // events were lost: rescan everything
				for _, r := range roots {
					due[r.lib] = time.Now().Add(delay)
				}
			}
			log.Warn("file watcher error", "err", err)
		case ev, ok := <-w.Events:
			if !ok {
				return nil
			}
			if ev.Op == fsnotify.Chmod || ignoredName(ev.Name) {
				continue
			}
			id, ok := libOf(ev.Name)
			if !ok {
				continue
			}
			if ev.Has(fsnotify.Create) {
				if fi, err := os.Stat(ev.Name); err == nil && fi.IsDir() {
					addTree(ev.Name) // a new or moved-in folder: watch it and what's inside
				}
			}
			due[id] = time.Now().Add(delay)
		case now := <-tick.C:
			for id, at := range due {
				if !now.Before(at) {
					delete(due, id)
					log.Info("library changed on disk; rescanning", "library", names[id])
					changed(id)
				}
			}
		}
	}
}
