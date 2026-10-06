package auth

import (
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sysadmin/blockbustr/internal/cache"
	"github.com/sysadmin/blockbustr/internal/store/pg/db"
	"github.com/sysadmin/blockbustr/internal/testutil"
)

func TestPasswords(t *testing.T) {
	h, err := HashPassword("hunter22")
	if err != nil || !strings.HasPrefix(h, "$2") {
		t.Fatalf("hash = %q %v", h, err)
	}
	if !CheckPassword(&h, "hunter22") || CheckPassword(&h, "Hunter22") || CheckPassword(&h, "") {
		t.Error("CheckPassword with hash")
	}
	if !CheckPassword(nil, "") || CheckPassword(nil, "x") {
		t.Error("passwordless user must accept only the empty password")
	}
	if _, err := HashPassword(strings.Repeat("a", 73)); !errors.Is(err, ErrPasswordTooLong) {
		t.Errorf("73-byte password: %v", err)
	}
}

var testPool *pgxpool.Pool // set by newService for exec()

func newService(t *testing.T) (*Service, *db.Queries, *cache.Cache) {
	t.Helper()
	q, pool := testutil.Queries(t)
	testPool = pool
	c := testutil.Cache(t)
	return New(q, c, testutil.Discard()), q, c
}

func TestBootstrapCreatesThenResets(t *testing.T) {
	s, q, _ := newService(t)
	ctx := t.Context()
	if err := s.Bootstrap(ctx, "", ""); err != nil {
		t.Fatalf("no admin configured: %v", err)
	}
	if err := s.Bootstrap(ctx, "Admin", "first-pass"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(ctx, "admin", "first-pass"); err != nil {
		t.Fatalf("login after create (case-insensitive name): %v", err)
	}
	// Lost-password recovery: a new configured password replaces the old one,
	// and the account is made an enabled admin again.
	u, _ := q.GetUserByName(ctx, "admin")
	if _, err := q.CreateUser(ctx, db.CreateUserParams{Name: "other"}); err != nil {
		t.Fatal(err)
	}
	exec(t, "UPDATE users SET is_admin = false, is_disabled = true WHERE id = $1", u.ID)
	if err := s.Bootstrap(ctx, "ADMIN", "second-pass"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(ctx, "admin", "first-pass"); !errors.Is(err, ErrInvalidCredentials) {
		t.Error("old password still works")
	}
	got, err := s.Authenticate(ctx, "Admin", "second-pass")
	if err != nil || !got.IsAdmin || got.IsDisabled || got.Name != "Admin" || got.LastLoginAt == nil {
		t.Fatalf("after reset: %+v %v", got, err)
	}
	if n, _ := q.CountUsers(ctx); n != 2 {
		t.Errorf("reset must not create a second admin: %d users", n)
	}
}

func TestAuthenticate(t *testing.T) {
	s, q, _ := newService(t)
	ctx := t.Context()
	h, _ := HashPassword("pw")
	if _, err := q.CreateUser(ctx, db.CreateUserParams{Name: "alice", PasswordHash: &h}); err != nil {
		t.Fatal(err)
	}
	if _, err := q.CreateUser(ctx, db.CreateUserParams{Name: "nopass"}); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name, pw string
		ok       bool
	}{{"alice", "pw", true}, {"ALICE", "pw", true}, {"alice", "nope", false}, {"bob", "pw", false}, {"nopass", "", true}, {"nopass", "x", false}} {
		_, err := s.Authenticate(ctx, c.name, c.pw)
		if (err == nil) != c.ok || (err != nil && !errors.Is(err, ErrInvalidCredentials)) {
			t.Errorf("Authenticate(%q, %q) = %v", c.name, c.pw, err)
		}
	}
	exec(t, "UPDATE users SET is_disabled = true WHERE name = 'alice'")
	if _, err := s.Authenticate(ctx, "alice", "pw"); !errors.Is(err, ErrInvalidCredentials) {
		t.Errorf("disabled user signed in: %v", err)
	}
}

func TestTokens(t *testing.T) {
	s, q, c := newService(t)
	ctx := t.Context()
	u, err := q.CreateUser(ctx, db.CreateUserParams{Name: "alice", IsAdmin: true})
	if err != nil {
		t.Fatal(err)
	}
	dev := Device{ID: "dev-1", Name: "Pixel", AppName: "Findroid", AppVersion: "1.1.0"}
	tok, err := s.IssueToken(ctx, u, dev)
	if err != nil || len(tok) != 32 {
		t.Fatalf("token %q %v", tok, err)
	}
	if _, err := s.IssueToken(ctx, u, Device{}); err == nil {
		t.Error("token without a device id")
	}

	sess, err := s.Resolve(ctx, tok)
	want := Session{UserID: u.ID, UserName: "alice", IsAdmin: true, DeviceID: "dev-1", DeviceName: "Pixel", AppName: "Findroid", AppVersion: "1.1.0"}
	if err != nil || sess != want {
		t.Fatalf("Resolve = %+v %v", sess, err)
	}
	for _, bad := range []string{"", "0123456789abcdef0123456789abcdef"} {
		if _, err := s.Resolve(ctx, bad); !errors.Is(err, ErrNoSession) {
			t.Errorf("Resolve(%q) = %v", bad, err)
		}
	}

	t.Run("database fallback when the cache is cold", func(t *testing.T) {
		if err := c.DeleteByPrefix(ctx); err != nil {
			t.Fatal(err)
		}
		got, err := s.Resolve(ctx, tok)
		if err != nil || got != want {
			t.Fatalf("cold Resolve = %+v %v", got, err)
		}
	})

	t.Run("cache hit doesn't need the database row", func(t *testing.T) {
		// Prove the cached path by making the database row unresolvable.
		exec(t, "UPDATE access_tokens SET revoked_at = now()")
		if _, err := s.Resolve(ctx, tok); err != nil {
			t.Fatalf("cached session not used: %v", err)
		}
		exec(t, "UPDATE access_tokens SET revoked_at = NULL")
	})

	t.Run("re-login on the same device revokes the old token, cached or not", func(t *testing.T) {
		tok2, err := s.IssueToken(ctx, u, dev)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Resolve(ctx, tok); !errors.Is(err, ErrNoSession) {
			t.Errorf("old token still valid: %v", err)
		}
		if _, err := s.Resolve(ctx, tok2); err != nil {
			t.Errorf("new token: %v", err)
		}
		tok = tok2
	})

	t.Run("revoke signs out immediately", func(t *testing.T) {
		if err := s.Revoke(ctx, tok); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Resolve(ctx, tok); !errors.Is(err, ErrNoSession) {
			t.Errorf("revoked token valid: %v", err)
		}
	})
}

// exec runs raw SQL against the current test database.
func exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := testPool.Exec(t.Context(), sql, args...); err != nil {
		t.Fatal(err)
	}
}
