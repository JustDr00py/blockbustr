package images

import (
	"context"
	"log/slog"

	"github.com/sysadmin/blockbustr/internal/store/pg/db"
)

// Prefetcher fills in dimensions and blurhashes for stored images, so item
// responses can carry ImageBlurHashes before any client asks for the image.
type Prefetcher struct {
	Store *Store
	Q     *db.Queries
	Log   *slog.Logger
	Batch int // images per run; 0 = 200
}

// Run processes lib's images missing info. It runs after every scan of the
// library; failures are logged and retried on the next run.
func (p Prefetcher) Run(ctx context.Context, lib db.Library) error {
	batch := p.Batch
	if batch <= 0 {
		batch = 200
	}
	rows, err := p.Q.ImagesMissingInfo(ctx, db.ImagesMissingInfoParams{LibraryID: lib.ID, RowLimit: int32(batch)})
	if err != nil {
		return err
	}
	done, failed := 0, 0
	for _, r := range rows {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		info, err := p.Store.Inspect(ctx, Source{URL: deref(r.SourceUrl), LocalPath: deref(r.LocalPath), Tag: r.Tag})
		if err != nil {
			failed++
			p.Log.Warn("image prefetch failed", "item", r.ItemID, "type", r.Type, "err", err)
			continue
		}
		if err := p.Q.SetImageInfo(ctx, db.SetImageInfoParams{
			ItemID: r.ItemID, Type: r.Type, Idx: r.Idx,
			Width: ptr(int32(info.Width)), Height: ptr(int32(info.Height)), Blurhash: &info.Blurhash,
		}); err != nil {
			return err
		}
		done++
	}
	if len(rows) > 0 {
		p.Log.Info("images prefetched", "library", lib.Name, "done", done, "failed", failed)
	}
	return nil
}

func ptr[T any](v T) *T { return &v }

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
