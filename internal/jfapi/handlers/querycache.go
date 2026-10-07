package handlers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"time"

	"github.com/sysadmin/blockbustr/internal/cache"
	"github.com/sysadmin/blockbustr/internal/store/pg"
)

// queryItems runs an item query with its total cached in Redis.
func (a *api) queryItems(ctx context.Context, q pg.ItemQuery) (pg.ItemQueryResult, error) {
	q.Counts = a.cachedCount
	return pg.QueryItems(ctx, a.DB, q)
}

// cachedCount keeps item query totals (DESIGN §5 q:count). Counting a page
// of a large library cost 20–30 ms, more than the page itself (TASKS P4.5).
func (a *api) cachedCount(ctx context.Context, query string, count func() (int, error)) (int, error) {
	return cachedQuery(ctx, a, "count", query, count)
}

// cachedQuery keeps a query's result in Redis for one generation of library
// and user-data events (DESIGN §5 q:*), at most QueryTTL. query identifies
// it (SQL or parameters, user included). Without Redis, or on a Redis
// error, it just runs the query.
func cachedQuery[T any](ctx context.Context, a *api, kind, query string, run func() (T, error)) (T, error) {
	if a.Cache == nil {
		return run()
	}
	var gen int64
	ok, err := a.Cache.GetJSON(ctx, cache.QueryGenKey(), &gen)
	if err != nil {
		return run()
	}
	if !ok { // lost (flushed or evicted): start a generation newer than any kept result
		gen = time.Now().UnixNano()
		if err := a.Cache.SetJSON(ctx, cache.QueryGenKey(), gen, 0); err != nil {
			return run()
		}
	}
	sum := sha256.Sum256([]byte(query))
	key := cache.QueryKey(kind, gen, hex.EncodeToString(sum[:16]))
	var v T
	if ok, err := a.Cache.GetJSON(ctx, key, &v); err == nil && ok {
		return v, nil
	}
	v, err = run()
	if err == nil {
		if err := a.Cache.SetJSON(ctx, key, v, cache.QueryTTL); err != nil {
			a.Log.WarnContext(ctx, "caching a query result failed", "kind", kind, "err", err)
		}
	}
	return v, err
}
