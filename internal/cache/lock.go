package cache

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"time"

	"github.com/redis/go-redis/v9"
)

// ErrLockNotHeld means the lock expired or belongs to someone else.
var ErrLockNotHeld = errors.New("cache: lock not held")

// Lock is a single-instance Redis lock (SET NX with an owner token). Unlock and
// Refresh only act if the caller still owns it, checked atomically in Lua.
type Lock struct {
	c     *Cache
	key   string
	token string
}

var (
	unlockScript = redis.NewScript(`
if redis.call("GET", KEYS[1]) == ARGV[1] then return redis.call("DEL", KEYS[1]) end
return 0`)
	refreshScript = redis.NewScript(`
if redis.call("GET", KEYS[1]) == ARGV[1] then return redis.call("PEXPIRE", KEYS[1], ARGV[2]) end
return 0`)
)

// TryLock acquires k for ttl without waiting. It returns ok=false if someone
// else holds it.
func (c *Cache) TryLock(ctx context.Context, k Key, ttl time.Duration) (lock *Lock, ok bool, err error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return nil, false, err
	}
	l := &Lock{c: c, key: c.key(k), token: hex.EncodeToString(b)}
	err = c.rdb.SetArgs(ctx, l.key, l.token, redis.SetArgs{Mode: "NX", TTL: ttl}).Err()
	if errors.Is(err, redis.Nil) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return l, true, nil
}

// Unlock releases the lock if it's still ours.
func (l *Lock) Unlock(ctx context.Context) error {
	n, err := unlockScript.Run(ctx, l.c.rdb, []string{l.key}, l.token).Int()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrLockNotHeld
	}
	return nil
}

// Refresh extends the lock to ttl from now if it's still ours. Long jobs
// (scans, transcodes) call it periodically.
func (l *Lock) Refresh(ctx context.Context, ttl time.Duration) error {
	n, err := refreshScript.Run(ctx, l.c.rdb, []string{l.key}, l.token, ttl.Milliseconds()).Int()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrLockNotHeld
	}
	return nil
}
