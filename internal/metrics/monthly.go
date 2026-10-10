package metrics

import (
	"context"
	"log/slog"
	"sync"
	"time"

	dto "github.com/prometheus/client_model/go"
)

// MonthStore keeps proxied-byte totals per hour (hour: its start), which
// the Stats page groups into months of the admin's time zone.
type MonthStore interface {
	AddProxiedBytes(ctx context.Context, hour time.Time, bytes int64) error
}

// MonthlyBytes persists the proxied-bytes counter, which restarts at zero
// with the process, as per-hour totals that don't. Run flushes the growth
// since the last flush every interval; Flush does it once (at shutdown, when
// the last streams have finished).
type MonthlyBytes struct {
	Store MonthStore
	Log   *slog.Logger

	mu      sync.Mutex
	flushed float64 // counter value already stored
}

// NewMonthlyBytes starts counting from the counter's current value, so what
// was proxied before it existed isn't stored twice.
func NewMonthlyBytes(store MonthStore, log *slog.Logger) *MonthlyBytes {
	return &MonthlyBytes{Store: store, Log: log, flushed: proxiedNow()}
}

// Unflushed is what has been proxied but not stored yet.
func (b *MonthlyBytes) Unflushed() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return int64(proxiedNow() - b.flushed)
}

// Flush stores the growth since the last flush against the current hour.
// On failure it keeps the growth for the next try.
func (b *MonthlyBytes) Flush(ctx context.Context) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := proxiedNow()
	n := int64(now - b.flushed)
	if n <= 0 {
		return
	}
	if err := b.Store.AddProxiedBytes(ctx, time.Now().UTC().Truncate(time.Hour), n); err != nil {
		b.Log.Warn("storing the monthly proxied bytes failed", "err", err)
		return
	}
	b.flushed += float64(n)
}

// Run flushes every interval until ctx ends.
func (b *MonthlyBytes) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			b.Flush(ctx)
		}
	}
}

func proxiedNow() float64 {
	var m dto.Metric
	if err := ProxiedBytes.Write(&m); err != nil {
		return 0
	}
	return m.GetCounter().GetValue()
}
