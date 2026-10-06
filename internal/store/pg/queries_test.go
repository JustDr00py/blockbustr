package pg

import (
	"crypto/sha256"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sysadmin/blockbustr/internal/store/pg/db"
)

// newTestQueries returns sqlc queries over a pgx pool on a freshly migrated
// throwaway database.
func newTestQueries(t *testing.T) (*db.Queries, func(sql string, args ...any)) {
	t.Helper()
	url := newTestDatabase(t)
	sqlDB, err := Open(t.Context(), url, 5*time.Second, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	migrate(t, sqlDB, MigrateUp)
	_ = sqlDB.Close()

	pool, err := NewPool(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(t.Context(), sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	return db.New(pool), exec
}

func ptr[T any](v T) *T { return &v }

func TestUserQueries(t *testing.T) {
	q, _ := newTestQueries(t)
	ctx := t.Context()

	alice, err := q.CreateUser(ctx, db.CreateUserParams{Name: "Alice", PasswordHash: ptr("hash"), IsAdmin: true})
	if err != nil {
		t.Fatal(err)
	}
	if alice.ID == uuid.Nil || alice.CreatedAt.IsZero() || alice.LastLoginAt != nil {
		t.Errorf("unexpected new user: %+v", alice)
	}
	if _, err := q.CreateUser(ctx, db.CreateUserParams{Name: "ALICE"}); err == nil {
		t.Error("names must be unique case-insensitively")
	}

	got, err := q.GetUserByName(ctx, "alice")
	if err != nil || got.ID != alice.ID || got.Name != "Alice" {
		t.Fatalf("GetUserByName = %+v, %v", got, err)
	}
	if _, err := q.GetUserByName(ctx, "bob"); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("missing user: want ErrNoRows, got %v", err)
	}

	if err := q.SetUserLastLogin(ctx, alice.ID); err != nil {
		t.Fatal(err)
	}
	got, err = q.GetUserByID(ctx, alice.ID)
	if err != nil || got.LastLoginAt == nil {
		t.Fatalf("last login not set: %+v, %v", got, err)
	}

	if _, err := q.CreateUser(ctx, db.CreateUserParams{Name: "bob"}); err != nil {
		t.Fatal(err)
	}
	users, err := q.ListUsers(ctx)
	if err != nil || len(users) != 2 || users[0].Name != "Alice" || users[1].PasswordHash != nil {
		t.Fatalf("ListUsers = %+v, %v", users, err)
	}
	if n, err := q.CountUsers(ctx); err != nil || n != 2 {
		t.Fatalf("CountUsers = %d, %v", n, err)
	}
}

func TestTokenQueries(t *testing.T) {
	q, exec := newTestQueries(t)
	ctx := t.Context()

	alice, err := q.CreateUser(ctx, db.CreateUserParams{Name: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	bob, err := q.CreateUser(ctx, db.CreateUserParams{Name: "bob"})
	if err != nil {
		t.Fatal(err)
	}
	dev := db.UpsertDeviceParams{ID: "dev-1", UserID: alice.ID, Name: "Pixel", AppName: "Findroid", AppVersion: "1.1.0"}
	if err := q.UpsertDevice(ctx, dev); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("token-a"))
	if err := q.CreateAccessToken(ctx, db.CreateAccessTokenParams{TokenSha: sum[:], UserID: alice.ID, DeviceID: "dev-1"}); err != nil {
		t.Fatal(err)
	}

	s, err := q.GetTokenSession(ctx, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	if s.UserID != alice.ID || s.UserName != "alice" || s.DeviceID != "dev-1" || s.AppName != "Findroid" || s.AppVersion != "1.1.0" {
		t.Errorf("session = %+v", s)
	}
	if err := q.TouchAccessToken(ctx, sum[:]); err != nil {
		t.Fatal(err)
	}
	unknown := sha256.Sum256([]byte("nope"))
	if _, err := q.GetTokenSession(ctx, unknown[:]); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("unknown token: want ErrNoRows, got %v", err)
	}

	t.Run("device moves to the user who signs in last", func(t *testing.T) {
		dev.UserID, dev.AppVersion = bob.ID, "1.2.0"
		if err := q.UpsertDevice(ctx, dev); err != nil {
			t.Fatal(err)
		}
		s, err := q.GetTokenSession(ctx, sum[:])
		if err != nil || s.AppVersion != "1.2.0" {
			t.Fatalf("device not updated: %+v, %v", s, err)
		}
	})

	t.Run("revoking device tokens signs it out", func(t *testing.T) {
		n, err := q.RevokeDeviceTokens(ctx, "dev-1")
		if err != nil || n != 1 {
			t.Fatalf("revoked %d, %v", n, err)
		}
		if _, err := q.GetTokenSession(ctx, sum[:]); !errors.Is(err, pgx.ErrNoRows) {
			t.Errorf("revoked token still resolves: %v", err)
		}
		if n, _ := q.RevokeAccessToken(ctx, sum[:]); n != 0 {
			t.Errorf("revoking twice affected %d rows", n)
		}
	})

	t.Run("disabled users' tokens don't resolve", func(t *testing.T) {
		sumB := sha256.Sum256([]byte("token-b"))
		if err := q.CreateAccessToken(ctx, db.CreateAccessTokenParams{TokenSha: sumB[:], UserID: bob.ID, DeviceID: "dev-1"}); err != nil {
			t.Fatal(err)
		}
		if _, err := q.GetTokenSession(ctx, sumB[:]); err != nil {
			t.Fatalf("fresh token: %v", err)
		}
		exec(`UPDATE users SET is_disabled = true WHERE id = $1`, bob.ID)
		if _, err := q.GetTokenSession(ctx, sumB[:]); !errors.Is(err, pgx.ErrNoRows) {
			t.Errorf("disabled user's token resolves: %v", err)
		}
	})
}

func TestItemQueries(t *testing.T) {
	q, exec := newTestQueries(t)
	ctx := t.Context()

	lib, err := q.CreateLibrary(ctx, db.CreateLibraryParams{Name: "Movies", Kind: "movies", Paths: []string{"/media/Movies"}})
	if err != nil {
		t.Fatal(err)
	}
	if libs, err := q.ListLibraries(ctx); err != nil || len(libs) != 1 || libs[0].Paths[0] != "/media/Movies" {
		t.Fatalf("ListLibraries = %+v, %v", libs, err)
	}
	folder, err := q.CreateItem(ctx, db.CreateItemParams{LibraryID: lib.ID, Type: "CollectionFolder", Name: "Movies", SortName: "movies", SourceKind: "virtual"})
	if err != nil {
		t.Fatal(err)
	}

	byName := map[string]uuid.UUID{}
	for _, m := range []struct{ name, sort string }{
		{"The Matrix", "matrix"}, {"Alien", "alien"}, {"Luca", "luca"}, {"Zodiac", "zodiac"}, {"Brazil", "brazil"},
	} {
		it, err := q.CreateItem(ctx, db.CreateItemParams{
			LibraryID: lib.ID, ParentID: &folder.ID, TopParentID: &folder.ID,
			Type: "Movie", Name: m.name, SortName: m.sort, SourceKind: "file", Path: ptr("/media/Movies/" + m.name + ".mkv"),
			ProductionYear: ptr(int32(1999)),
		})
		if err != nil {
			t.Fatal(err)
		}
		byName[m.name] = it.ID
	}
	exec(`UPDATE items SET forced_sort_name = 'aaa' WHERE id = $1`, byName["Zodiac"])

	page := func(off int32) []string {
		t.Helper()
		rows, err := q.ListChildren(ctx, db.ListChildrenParams{ParentID: &folder.ID, RowLimit: 2, RowOffset: off})
		if err != nil {
			t.Fatal(err)
		}
		names := make([]string, len(rows))
		for i, r := range rows {
			names[i] = r.Name
		}
		return names
	}
	got := append(append(page(0), page(2)...), page(4)...)
	want := []string{"Zodiac", "Alien", "Brazil", "Luca", "The Matrix"} // forced sort name first
	if len(got) != len(want) {
		t.Fatalf("pages = %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
	if n, err := q.CountChildren(ctx, &folder.ID); err != nil || n != 5 {
		t.Fatalf("CountChildren = %d, %v", n, err)
	}

	it, err := q.GetItem(ctx, byName["Luca"])
	if err != nil {
		t.Fatal(err)
	}
	if it.DateCreated.IsZero() || string(it.ProviderIds) != "{}" || it.StremioRef != nil || *it.ProductionYear != 1999 || it.PremiereDate != nil {
		t.Errorf("GetItem = %+v", it)
	}

	strm, err := q.CreateItem(ctx, db.CreateItemParams{
		LibraryID: lib.ID, ParentID: &folder.ID, Type: "Movie", Name: "Luca", SortName: "luca",
		SourceKind: "strm", Path: ptr("/media/Movies/Luca (2021)/Luca (2021).strm"), StrmUrl: ptr("http://jellybird:8097/stream/x"),
	})
	if err != nil || strm.StrmUrl == nil {
		t.Fatalf("strm item: %+v, %v", strm, err)
	}
	if _, err := q.GetItem(ctx, uuid.New()); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("missing item: want ErrNoRows, got %v", err)
	}
}

func TestEnsureServerIDIsStable(t *testing.T) {
	q, _ := newTestQueries(t)
	first, err := EnsureServerID(t.Context(), q)
	if err != nil || first == uuid.Nil {
		t.Fatalf("first: %v %v", first, err)
	}
	for range 3 {
		again, err := EnsureServerID(t.Context(), q)
		if err != nil || again != first {
			t.Fatalf("server id changed: %v → %v (%v)", first, again, err)
		}
	}
}
