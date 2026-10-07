// Package events carries typed server events over Redis pub/sub (DESIGN §2,
// TASKS P2.10), so every instance's WebSocket hub hears about changes made
// anywhere: user data written, libraries scanned. Domain packages publish
// here without depending on the HTTP layer.
package events

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/sysadmin/blockbustr/internal/cache"
)

// Kind is an event type, named after the WebSocket message it becomes.
type Kind string

const (
	// UserDataChanged: a user's played/position/favourite state of items
	// changed.
	UserDataChanged Kind = "UserDataChanged"
	// LibraryChanged: items of libraries were added, updated or removed.
	LibraryChanged Kind = "LibraryChanged"
)

// Event is one change.
type Event struct {
	Kind      Kind
	UserID    uuid.UUID   `json:",omitzero"`  // UserDataChanged
	ItemIDs   []uuid.UUID `json:",omitempty"` // UserDataChanged: the items whose data changed
	Libraries []uuid.UUID `json:",omitempty"` // LibraryChanged: libraries whose content changed
	Updated   []uuid.UUID `json:",omitempty"` // LibraryChanged: items updated in place
}

// Bus publishes and receives events. A nil Bus drops what's published, so
// callers needn't check.
type Bus struct {
	Cache *cache.Cache
	Log   *slog.Logger
}

// Publish sends e to every instance. Failures are logged, never returned:
// a missed live update must not fail the change itself.
func (b *Bus) Publish(ctx context.Context, e Event) {
	if b == nil || b.Cache == nil {
		return
	}
	data, err := json.Marshal(e)
	if err == nil {
		err = b.Cache.Publish(context.WithoutCancel(ctx), cache.EventsChannel, data)
	}
	if err != nil && b.Log != nil {
		b.Log.WarnContext(ctx, "publishing event failed", "kind", e.Kind, "err", err)
	}
	// Items or user data changed: cached query results are stale now.
	if e.Kind == LibraryChanged || e.Kind == UserDataChanged {
		if err := b.Cache.SetJSON(context.WithoutCancel(ctx), cache.QueryGenKey(), time.Now().UnixNano(), 0); err != nil && b.Log != nil {
			b.Log.WarnContext(ctx, "retiring cached queries failed", "err", err)
		}
	}
}

// Subscribe delivers events until ctx ends.
func (b *Bus) Subscribe(ctx context.Context) (<-chan Event, error) {
	raw, err := b.Cache.Subscribe(ctx, cache.EventsChannel)
	if err != nil {
		return nil, err
	}
	out := make(chan Event, 64)
	go func() {
		defer close(out)
		for data := range raw {
			var e Event
			if err := json.Unmarshal(data, &e); err != nil {
				continue
			}
			select {
			case out <- e:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}
