// Package cache is blockbustr's Redis layer (DESIGN §5). Redis is a cache and
// coordination store only: everything in it can be rebuilt from Postgres, so
// callers must treat a miss (or an empty Redis) as normal.
package cache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// DefaultPrefix namespaces every key, so one Redis can be shared with other apps.
const DefaultPrefix = "bb:"

// Key is an unprefixed key name. Build them with the helpers in keys.go.
type Key string

// Cache wraps a Redis client with blockbustr's key prefix.
type Cache struct {
	rdb    *redis.Client
	prefix string
}

// New connects to the redis:// or rediss:// URL and pings it.
func New(ctx context.Context, url string) (*Cache, error) {
	return NewWithPrefix(ctx, url, DefaultPrefix)
}

// NewWithPrefix is New with a custom key prefix (tests use a random one).
func NewWithPrefix(ctx context.Context, url, prefix string) (*Cache, error) {
	opts, err := redis.ParseURL(url)
	if err != nil {
		return nil, fmt.Errorf("redis: %w", err)
	}
	rdb := redis.NewClient(opts)
	if err := rdb.Ping(ctx).Err(); err != nil {
		_ = rdb.Close()
		return nil, fmt.Errorf("redis: %w", err)
	}
	return &Cache{rdb: rdb, prefix: prefix}, nil
}

// Close releases the connection pool.
func (c *Cache) Close() error { return c.rdb.Close() }

func (c *Cache) key(k Key) string { return c.prefix + string(k) }

// SetJSON stores v as JSON under k for ttl.
func (c *Cache) SetJSON(ctx context.Context, k Key, v any, ttl time.Duration) error {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("cache: encode %s: %w", k, err)
	}
	return c.rdb.Set(ctx, c.key(k), b, ttl).Err()
}

// GetJSON decodes the value under k into dst. It reports false (and no error)
// on a miss.
func (c *Cache) GetJSON(ctx context.Context, k Key, dst any) (bool, error) {
	return decode(k, c.rdb.Get(ctx, c.key(k)), dst)
}

// GetJSONEx is GetJSON that also resets the key's TTL, for sliding expiry
// such as access tokens (DESIGN §5 tok:*).
func (c *Cache) GetJSONEx(ctx context.Context, k Key, dst any, ttl time.Duration) (bool, error) {
	return decode(k, c.rdb.GetEx(ctx, c.key(k), ttl), dst)
}

func decode(k Key, cmd *redis.StringCmd, dst any) (bool, error) {
	b, err := cmd.Bytes()
	if errors.Is(err, redis.Nil) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := json.Unmarshal(b, dst); err != nil {
		return false, fmt.Errorf("cache: decode %s: %w", k, err)
	}
	return true, nil
}

// Delete removes keys; missing keys are not an error.
func (c *Cache) Delete(ctx context.Context, keys ...Key) error {
	if len(keys) == 0 {
		return nil
	}
	full := make([]string, len(keys))
	for i, k := range keys {
		full[i] = c.key(k)
	}
	return c.rdb.Del(ctx, full...).Err()
}

// AddToSet adds members to the set at k and (re)sets the set's TTL.
func (c *Cache) AddToSet(ctx context.Context, k Key, ttl time.Duration, members ...string) error {
	if len(members) == 0 {
		return nil
	}
	pipe := c.rdb.TxPipeline()
	pipe.SAdd(ctx, c.key(k), toAny(members)...)
	pipe.Expire(ctx, c.key(k), ttl)
	_, err := pipe.Exec(ctx)
	return err
}

// SetMembers returns the members of the set at k (empty on a miss).
func (c *Cache) SetMembers(ctx context.Context, k Key) ([]string, error) {
	return c.rdb.SMembers(ctx, c.key(k)).Result()
}

// Prefix returns the key prefix, for tests that inspect Redis directly.
func (c *Cache) Prefix() string { return c.prefix }

func toAny(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

// DeleteByPrefix removes every key under this cache's prefix. It is for
// tests and maintenance; never call it on the shared DefaultPrefix casually.
func (c *Cache) DeleteByPrefix(ctx context.Context) error {
	iter := c.rdb.Scan(ctx, 0, c.prefix+"*", 200).Iterator()
	for iter.Next(ctx) {
		if err := c.rdb.Del(ctx, iter.Val()).Err(); err != nil {
			return err
		}
	}
	return iter.Err()
}
