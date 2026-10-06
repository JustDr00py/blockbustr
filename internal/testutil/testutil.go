// Package testutil provides throwaway Postgres databases and prefixed Redis
// caches for integration tests. Tests skip unless the URLs are set; `make
// test-integration` sets them for the compose stack.
package testutil

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"log/slog"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sysadmin/blockbustr/internal/cache"
	"github.com/sysadmin/blockbustr/internal/store/pg"
	"github.com/sysadmin/blockbustr/internal/store/pg/db"
)

// Discard is a logger for code under test.
func Discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func randHex() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Queries returns sqlc queries over a fresh, migrated database that is
// dropped when the test ends.
func Queries(t *testing.T) (*db.Queries, *pgxpool.Pool) {
	t.Helper()
	admin := os.Getenv("BLOCKBUSTR_TEST_DATABASE_URL")
	if admin == "" {
		t.Skip("BLOCKBUSTR_TEST_DATABASE_URL not set (see `make test-integration`)")
	}
	ctx := t.Context()
	adminDB, err := pg.Open(ctx, admin, 5*time.Second, Discard())
	if err != nil {
		t.Fatal(err)
	}
	name := "bbtest_" + randHex()
	if _, err := adminDB.ExecContext(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = adminDB.ExecContext(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
		_ = adminDB.Close()
	})
	u, err := url.Parse(admin)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	sqlDB, err := pg.Open(ctx, u.String(), 5*time.Second, Discard())
	if err != nil {
		t.Fatal(err)
	}
	if err := pg.Migrate(ctx, sqlDB, pg.MigrateUp, Discard()); err != nil {
		t.Fatal(err)
	}
	_ = sqlDB.Close()
	pool, err := pg.NewPool(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return db.New(pool), pool
}

// Cache returns a cache under a random key prefix whose keys are deleted
// when the test ends.
func Cache(t *testing.T) *cache.Cache {
	t.Helper()
	url := os.Getenv("BLOCKBUSTR_TEST_REDIS_URL")
	if url == "" {
		t.Skip("BLOCKBUSTR_TEST_REDIS_URL not set (see `make test-integration`)")
	}
	c, err := cache.NewWithPrefix(t.Context(), url, "bbtest:"+randHex()+":")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = c.DeleteByPrefix(context.Background())
		_ = c.Close()
	})
	return c
}
