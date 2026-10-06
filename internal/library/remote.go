package library

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sysadmin/blockbustr/internal/events"
	"github.com/sysadmin/blockbustr/internal/media"
	"github.com/sysadmin/blockbustr/internal/store/pg/db"
)

// Remote sources are probed at first play, not during scans (TASKS P2.4b,
// DESIGN §6 .strm): probing over the network is slow and debrid links may
// not be resolvable until someone wants to watch.

// RemoteProbeTimeout bounds a first-play probe; PlaybackInfo waits for it.
const RemoteProbeTimeout = 15 * time.Second

// ProbeRemote probes the .strm item's target and stores the result as its
// media source. Concurrent calls for one item share a single probe. ok is
// false (with a nil error) when the item isn't a .strm or ffprobe failed;
// the failure is recorded and not retried until the .strm changes.
func (s *Scanner) ProbeRemote(ctx context.Context, item uuid.UUID) (bool, error) {
	v, err, _ := s.remote.Do(item.String(), func() (any, error) {
		return s.probeRemote(context.WithoutCancel(ctx), item)
	})
	if err != nil {
		return false, err
	}
	return v.(bool), nil
}

func (s *Scanner) probeRemote(ctx context.Context, item uuid.UUID) (bool, error) {
	row, err := s.q.GetStrmItem(ctx, item)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	r := db.ItemsNeedingSourceRow(row)
	src := remoteSource(r)
	pctx, cancel := context.WithTimeout(ctx, RemoteProbeTimeout)
	defer cancel()
	start := time.Now()
	info, perr := s.prober.Probe(pctx, *row.StrmUrl, media.Options{Remote: true})
	now := time.Now()
	src.ProbedAt = &now
	if perr != nil {
		s.log.Warn("remote probe failed", "item", item, "err", perr, "took", time.Since(start).Round(time.Millisecond))
		msg := perr.Error()
		src.ProbeError = &msg
		return false, s.storeSource(ctx, r, src, nil)
	}
	if c := containerName(info.Container); c != "" {
		src.Container = &c
	}
	src.Size = optInt64(info.Size)
	src.Bitrate = optInt32(int(min(info.Bitrate, 1<<31-1)))
	if info.Bitrate == 0 {
		src.Bitrate = nil // unknown, not zero (media.Decide assumes a high one)
	}
	if info.Duration > 0 {
		src.RuntimeTicks = ptr(int64(info.Duration / 100))
	}
	s.log.Info("probed remote source", "item", item, "container", info.Container, "streams", len(info.Streams), "took", time.Since(start).Round(time.Millisecond))
	if err := s.storeSource(ctx, r, src, info); err != nil {
		return false, err
	}
	// As Jellyfin does after probing a .strm (captured: ItemsUpdated).
	s.events.Publish(ctx, events.Event{Kind: events.LibraryChanged, Updated: []uuid.UUID{item}})
	return true, nil
}
