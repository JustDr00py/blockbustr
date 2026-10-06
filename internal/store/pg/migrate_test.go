package pg

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"io"
	"log/slog"
	"net/url"
	"os"
	"testing"
	"time"
)

// newTestDB opens a fresh throwaway database (see newTestDatabase).
func newTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := Open(t.Context(), newTestDatabase(t), 5*time.Second, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// newTestDatabase creates a throwaway database on the server named by
// BLOCKBUSTR_TEST_DATABASE_URL (any database the role can CREATE DATABASE
// from, e.g. .../postgres), drops it when the test ends, and returns its URL.
func newTestDatabase(t *testing.T) string {
	t.Helper()
	admin := os.Getenv("BLOCKBUSTR_TEST_DATABASE_URL")
	if admin == "" {
		t.Skip("BLOCKBUSTR_TEST_DATABASE_URL not set (see `make test-db`)")
	}
	ctx := t.Context()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	adminDB, err := Open(ctx, admin, 5*time.Second, log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = adminDB.Close() })

	b := make([]byte, 6)
	_, _ = rand.Read(b)
	name := "bbtest_" + hex.EncodeToString(b)
	if _, err := adminDB.ExecContext(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(admin)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	// Registered before the caller's cleanups, so it runs after they close
	// their connections; FORCE covers any that are still open.
	t.Cleanup(func() {
		_, _ = adminDB.ExecContext(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
	})
	return u.String()
}

func migrate(t *testing.T, db *sql.DB, cmd string) {
	t.Helper()
	if err := Migrate(t.Context(), db, cmd, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatalf("migrate %s: %v", cmd, err)
	}
}

var appTables = []string{
	"libraries", "items", "media_sources", "media_streams", "images",
	"users", "devices", "access_tokens", "user_data", "display_preferences",
	"server_settings", "chapters", "genres", "item_genres", "studios", "item_studios", "people", "item_people",
}

func tableCount(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	err := db.QueryRowContext(t.Context(),
		`SELECT count(*) FROM information_schema.tables WHERE table_schema = 'public' AND table_name = ANY($1)`,
		appTables).Scan(&n)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestMigrationsRoundTrip(t *testing.T) {
	db := newTestDB(t)
	migrate(t, db, MigrateUp)
	if n := tableCount(t, db); n != len(appTables) {
		t.Fatalf("after up: %d/%d tables", n, len(appTables))
	}
	migrate(t, db, MigrateUp) // idempotent
	migrate(t, db, MigrateStatus)

	// Every migration must be reversible (AGENTS definition of done).
	migrate(t, db, MigrateDownAll)
	if n := tableCount(t, db); n != 0 {
		t.Fatalf("after down-all: %d tables left", n)
	}
	var fn int
	if err := db.QueryRowContext(t.Context(), `SELECT count(*) FROM pg_proc WHERE proname IN ('f_unaccent', 'item_search_vector')`).Scan(&fn); err != nil || fn != 0 {
		t.Fatalf("functions left behind: %d %v", fn, err)
	}
	migrate(t, db, MigrateUp)
	if n := tableCount(t, db); n != len(appTables) {
		t.Fatalf("after re-up: %d tables", n)
	}
}

func TestSchemaBehaviour(t *testing.T) {
	db := newTestDB(t)
	migrate(t, db, MigrateUp)
	ctx := t.Context()
	exec := func(q string, args ...any) error { _, err := db.ExecContext(ctx, q, args...); return err }

	var lib, folder string
	if err := db.QueryRowContext(ctx, `INSERT INTO libraries (name, kind) VALUES ('Movies', 'movies') RETURNING id`).Scan(&lib); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `INSERT INTO items (library_id, type, name, sort_name) VALUES ($1, 'CollectionFolder', 'Movies', 'movies') RETURNING id`, lib).Scan(&folder); err != nil {
		t.Fatal(err)
	}
	if err := exec(`INSERT INTO items (library_id, parent_id, top_parent_id, type, name, sort_name, source_kind, path)
	                VALUES ($1, $2, $2, 'Movie', 'Pokémon: Le Film', 'pokemon le film', 'file', '/media/Movies/p.mkv')`, lib, folder); err != nil {
		t.Fatal(err)
	}

	t.Run("accent-insensitive full-text search", func(t *testing.T) {
		var n int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM items WHERE item_search_vector(name, original_title) @@ plainto_tsquery('simple', f_unaccent('pokemon'))`).Scan(&n); err != nil || n != 1 {
			t.Fatalf("got %d, %v", n, err)
		}
	})
	t.Run("trigram fuzzy match", func(t *testing.T) {
		var n int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM items WHERE f_unaccent(name) % 'pokemon le flim'`).Scan(&n); err != nil || n != 1 {
			t.Fatalf("got %d, %v", n, err)
		}
	})
	t.Run("strm items need a url", func(t *testing.T) {
		if exec(`INSERT INTO items (library_id, type, name, sort_name, source_kind) VALUES ($1, 'Movie', 'x', 'x', 'strm')`, lib) == nil {
			t.Fatal("expected CHECK violation")
		}
	})
	t.Run("one item per library path", func(t *testing.T) {
		if exec(`INSERT INTO items (library_id, type, name, sort_name, source_kind, path) VALUES ($1, 'Movie', 'dup', 'dup', 'file', '/media/Movies/p.mkv')`, lib) == nil {
			t.Fatal("expected unique violation")
		}
	})
	t.Run("user names are case-insensitive", func(t *testing.T) {
		if err := exec(`INSERT INTO users (name) VALUES ('Alice')`); err != nil {
			t.Fatal(err)
		}
		if exec(`INSERT INTO users (name) VALUES ('alice')`) == nil {
			t.Fatal("expected unique violation for differently-cased name")
		}
	})
	t.Run("tokens are 32-byte hashes", func(t *testing.T) {
		if err := exec(`INSERT INTO devices (id, user_id) SELECT 'dev1', id FROM users WHERE name = 'alice'`); err != nil {
			t.Fatal(err)
		}
		if exec(`INSERT INTO access_tokens (token_sha, user_id, device_id) SELECT '\x00'::bytea, id, 'dev1' FROM users`) == nil {
			t.Fatal("expected CHECK violation for short hash")
		}
		if err := exec(`INSERT INTO access_tokens (token_sha, user_id, device_id) SELECT digest('tok', 'sha256'), id, 'dev1' FROM users`); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("deleting a library cascades", func(t *testing.T) {
		if err := exec(`DELETE FROM libraries WHERE id = $1`, lib); err != nil {
			t.Fatal(err)
		}
		var n int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM items`).Scan(&n); err != nil || n != 0 {
			t.Fatalf("items left: %d %v", n, err)
		}
	})
}

func TestMigrateUnknownCommand(t *testing.T) {
	if err := Migrate(t.Context(), &sql.DB{}, "sideways", slog.Default()); err == nil {
		t.Fatal("expected error")
	}
}
