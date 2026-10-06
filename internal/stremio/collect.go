package stremio

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/sysadmin/blockbustr/internal/provider"
)

// Offer is one stream an addon offers for a title.
type Offer struct {
	Stream   Stream
	AddonID  uuid.UUID
	Addon    string // the addon's name, for labels
	Priority int
	// Cached is set when a debrid account reported the torrent cached.
	Cached bool
}

// Collector gathers the streams every enabled addon offers for a title
// (DESIGN §7.3 step 1). Addons are asked in parallel; one that is slow or
// failing is logged and left out, never failing the collection.
type Collector struct {
	Registry *Registry
	// Timeout bounds each addon's answer (default 6s).
	Timeout time.Duration
	// Debrid accounts are asked which torrents they have cached, within
	// DebridTimeout (default 2s). Addon markers ("[RD+]") count too.
	Debrid        []provider.Provider
	DebridTimeout time.Duration
	// Known reports torrents already ready on an account (played before),
	// which count as cached too; nil skips it.
	Known func(ctx context.Context, hashes []string) map[string]bool
	Log   *slog.Logger

	addons func(ctx context.Context) ([]addonInfo, error) // tests
}

// Collect returns the offers for typ ("movie", "series") and id ("tt0133093",
// or "tt0944947:1:2" for an episode), grouped by addon in priority order.
func (c *Collector) Collect(ctx context.Context, typ, id string) ([]Offer, error) {
	list := c.addons
	if list == nil {
		list = func(ctx context.Context) ([]addonInfo, error) {
			_, order, err := c.Registry.openAddons(ctx, c.Log)
			return order, err
		}
	}
	addons, err := list(ctx)
	if err != nil {
		return nil, err
	}
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = 6 * time.Second
	}

	per := make([][]Offer, len(addons))
	var wg sync.WaitGroup
	for i, ad := range addons {
		if !ad.manifest.Supports("stream", typ, id) {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			actx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			streams, err := c.Registry.Client.Streams(actx, ad.base, typ, id)
			if err != nil {
				c.Log.Warn("stremio streams failed", "host", ad.row.Host, "type", typ, "id", id, "err", err)
				return
			}
			for _, s := range streams {
				per[i] = append(per[i], Offer{Stream: s, AddonID: ad.row.ID, Addon: ad.manifest.Name, Priority: int(ad.row.Priority)})
			}
		}()
	}
	wg.Wait()

	var out []Offer
	for _, o := range per {
		out = append(out, o...)
	}
	c.markCached(ctx, out)
	return out, nil
}

// markCached marks the torrents among offers that are cached: ready on an
// account already (Known), or reported cached by an account. Unanswered
// checks just leave offers unmarked.
func (c *Collector) markCached(ctx context.Context, offers []Offer) {
	if len(c.Debrid) == 0 && c.Known == nil {
		return
	}
	var hashes []string
	seen := map[string]bool{}
	for _, o := range offers {
		if h := o.Stream.InfoHash; h != "" && o.Stream.URL == "" && !seen[h] {
			seen[h] = true
			hashes = append(hashes, h)
		}
	}
	if len(hashes) == 0 {
		return
	}
	timeout := c.DebridTimeout
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var mu sync.Mutex
	cached := map[string]bool{}
	if c.Known != nil {
		for h, ok := range c.Known(ctx, hashes) {
			cached[h] = ok
		}
	}
	var wg sync.WaitGroup
	for _, p := range c.Debrid {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := p.InstantCheck(ctx, hashes)
			if err != nil {
				c.Log.Debug("debrid cache check failed", "provider", p.Name(), "err", err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			for _, r := range res {
				if r.Cached {
					cached[r.Hash] = true
				}
			}
		}()
	}
	wg.Wait()
	for i := range offers {
		if cached[offers[i].Stream.InfoHash] {
			offers[i].Cached = true
		}
	}
}
