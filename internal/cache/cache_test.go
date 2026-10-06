package cache

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"testing"
	"time"
)

// newTestCache connects to BLOCKBUSTR_TEST_REDIS_URL with a random key prefix
// and deletes every key under it when the test ends.
func newTestCache(t *testing.T) *Cache {
	t.Helper()
	url := os.Getenv("BLOCKBUSTR_TEST_REDIS_URL")
	if url == "" {
		t.Skip("BLOCKBUSTR_TEST_REDIS_URL not set (see `make test-integration`)")
	}
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	c, err := NewWithPrefix(t.Context(), url, "bbtest:"+hex.EncodeToString(b)+":")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		iter := c.rdb.Scan(ctx, 0, c.prefix+"*", 100).Iterator()
		for iter.Next(ctx) {
			_ = c.rdb.Del(ctx, iter.Val()).Err()
		}
		_ = c.Close()
	})
	return c
}

type session struct {
	UserID string
	Pos    int64
}

func TestJSONRoundTrip(t *testing.T) {
	c := newTestCache(t)
	ctx := t.Context()
	k := SessionKey("dev-1")

	var got session
	if ok, err := c.GetJSON(ctx, k, &got); ok || err != nil {
		t.Fatalf("miss: ok=%v err=%v", ok, err)
	}
	if err := c.SetJSON(ctx, k, session{UserID: "u1", Pos: 42}, time.Minute); err != nil {
		t.Fatal(err)
	}
	if ok, err := c.GetJSON(ctx, k, &got); !ok || err != nil || got != (session{"u1", 42}) {
		t.Fatalf("hit: ok=%v err=%v got=%+v", ok, err, got)
	}
	if ttl := c.rdb.PTTL(ctx, c.key(k)).Val(); ttl <= 0 || ttl > time.Minute {
		t.Errorf("ttl = %v", ttl)
	}
	if n := c.rdb.Exists(ctx, c.prefix+"sess:dev-1").Val(); n != 1 {
		t.Error("key is not stored under the prefix")
	}
	if err := c.Delete(ctx, k, SessionKey("never-set")); err != nil {
		t.Fatal(err)
	}
	if ok, _ := c.GetJSON(ctx, k, &got); ok {
		t.Error("still present after Delete")
	}
	if err := c.Delete(ctx); err != nil {
		t.Errorf("empty Delete: %v", err)
	}
}

func TestGetJSONExSlidesTTL(t *testing.T) {
	c := newTestCache(t)
	ctx := t.Context()
	k := TokenKey([]byte{1, 2, 3})
	if err := c.SetJSON(ctx, k, session{UserID: "u"}, 2*time.Second); err != nil {
		t.Fatal(err)
	}
	var got session
	if ok, err := c.GetJSONEx(ctx, k, &got, TokenTTL); !ok || err != nil {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if ttl := c.rdb.PTTL(ctx, c.key(k)).Val(); ttl < time.Hour {
		t.Errorf("ttl not extended: %v", ttl)
	}
}

func TestGetJSONBadData(t *testing.T) {
	c := newTestCache(t)
	ctx := t.Context()
	c.rdb.Set(ctx, c.key("bad"), "not json", time.Minute)
	var got session
	if ok, err := c.GetJSON(ctx, "bad", &got); ok || err == nil {
		t.Fatalf("want decode error, got ok=%v err=%v", ok, err)
	}
}

func TestLock(t *testing.T) {
	c := newTestCache(t)
	ctx := t.Context()
	k := TranscodeLockKey("play-1")

	l, ok, err := c.TryLock(ctx, k, time.Minute)
	if err != nil || !ok {
		t.Fatalf("first lock: ok=%v err=%v", ok, err)
	}
	if _, ok, err := c.TryLock(ctx, k, time.Minute); ok || err != nil {
		t.Fatalf("second lock should fail: ok=%v err=%v", ok, err)
	}

	impostor := &Lock{c: c, key: l.key, token: "not-the-owner"}
	if err := impostor.Unlock(ctx); !errors.Is(err, ErrLockNotHeld) {
		t.Errorf("impostor unlock: %v", err)
	}
	if err := impostor.Refresh(ctx, time.Hour); !errors.Is(err, ErrLockNotHeld) {
		t.Errorf("impostor refresh: %v", err)
	}

	if err := l.Refresh(ctx, time.Hour); err != nil {
		t.Fatal(err)
	}
	if ttl := c.rdb.PTTL(ctx, l.key).Val(); ttl < 59*time.Minute {
		t.Errorf("refresh didn't extend: %v", ttl)
	}
	if err := l.Unlock(ctx); err != nil {
		t.Fatal(err)
	}
	if err := l.Unlock(ctx); !errors.Is(err, ErrLockNotHeld) {
		t.Errorf("double unlock: %v", err)
	}

	short, ok, err := c.TryLock(ctx, k, 100*time.Millisecond)
	if err != nil || !ok {
		t.Fatalf("relock: ok=%v err=%v", ok, err)
	}
	time.Sleep(250 * time.Millisecond)
	if err := short.Refresh(ctx, time.Minute); !errors.Is(err, ErrLockNotHeld) {
		t.Errorf("refresh after expiry: %v", err)
	}
	if _, ok, err := c.TryLock(ctx, k, time.Minute); !ok || err != nil {
		t.Errorf("lock after expiry: ok=%v err=%v", ok, err)
	}
}

func TestNewErrors(t *testing.T) {
	if _, err := New(t.Context(), "not a url"); err == nil {
		t.Error("expected parse error")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if _, err := New(ctx, "redis://127.0.0.1:1/0"); err == nil {
		t.Error("expected connection error")
	}
}

func TestSets(t *testing.T) {
	c := newTestCache(t)
	ctx := t.Context()
	k := DeviceTokensKey("dev-1")
	if m, err := c.SetMembers(ctx, k); err != nil || len(m) != 0 {
		t.Fatalf("empty set: %v %v", m, err)
	}
	if err := c.AddToSet(ctx, k, time.Minute, "a", "b"); err != nil {
		t.Fatal(err)
	}
	if err := c.AddToSet(ctx, k, time.Hour, "b", "c"); err != nil {
		t.Fatal(err)
	}
	if err := c.AddToSet(ctx, k, time.Hour); err != nil {
		t.Fatal(err)
	}
	m, err := c.SetMembers(ctx, k)
	if err != nil || len(m) != 3 {
		t.Fatalf("members = %v %v", m, err)
	}
	if ttl := c.rdb.PTTL(ctx, c.key(k)).Val(); ttl < 59*time.Minute {
		t.Errorf("ttl not refreshed: %v", ttl)
	}
}
