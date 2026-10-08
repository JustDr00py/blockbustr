package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/sysadmin/blockbustr/internal/config"
	"github.com/sysadmin/blockbustr/internal/provider"
	"github.com/sysadmin/blockbustr/internal/secret"
	"github.com/sysadmin/blockbustr/internal/testutil"
)

// Configured keys land in debrid_accounts sealed, never in plain text, and
// open again with the same secret_key. They only add accounts: a stored
// key (the Debrid page's) isn't overwritten by a different one.
func TestBootstrapDebrid(t *testing.T) {
	q, _ := testutil.Queries(t)
	keyHex := strings.Repeat("ab", secret.KeyLen)
	cfg := config.Config{SecretKey: keyHex}
	cfg.Debrid.RealDebridAPIKey = "rd-test-key"
	if err := bootstrapDebrid(t.Context(), q, cfg, testutil.Discard()); err != nil {
		t.Fatal(err)
	}
	// A changed key leaves the stored one; TorBox is added on the next start.
	cfg.Debrid.RealDebridAPIKey = "rd-test-key-2"
	cfg.Debrid.TorBoxAPIKey = "tb-test-key"
	if err := bootstrapDebrid(t.Context(), q, cfg, testutil.Discard()); err != nil {
		t.Fatal(err)
	}
	rows, err := q.ListDebridAccounts(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	key, _ := secret.ParseKey(keyHex)
	want := map[string]string{"realdebrid": "rd-test-key", "torbox": "tb-test-key"}
	if len(rows) != len(want) {
		t.Fatalf("rows: %d", len(rows))
	}
	for _, r := range rows {
		if bytes.Contains(r.ApiKeyEnc, []byte("test-key")) {
			t.Errorf("%s stored in plain text", r.Provider)
		}
		plain, err := secret.Open(key, r.ApiKeyEnc)
		if err != nil || string(plain) != want[r.Provider] {
			t.Errorf("%s: %q %v", r.Provider, plain, err)
		}
	}

	// No keys configured: nothing to do, secret_key not needed.
	if err := bootstrapDebrid(t.Context(), q, config.Config{}, testutil.Discard()); err != nil {
		t.Errorf("no keys: %v", err)
	}
	bad := config.Config{SecretKey: "short"}
	bad.Debrid.TorBoxAPIKey = "tb"
	if err := bootstrapDebrid(t.Context(), q, bad, testutil.Discard()); err == nil {
		t.Error("bad secret_key accepted")
	}
}

// Stored accounts become resolver providers; ones that can't be opened are
// skipped, never fatal.
func TestLoadProviders(t *testing.T) {
	q, pool := testutil.Queries(t)
	log := testutil.Discard()
	keyHex := strings.Repeat("ab", secret.KeyLen)
	cfg := config.Config{SecretKey: keyHex}
	cfg.Debrid.RealDebridAPIKey = "rd-test-key"
	cfg.Debrid.TorBoxAPIKey = "tb-test-key"
	if err := bootstrapDebrid(t.Context(), q, cfg, log); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE debrid_accounts SET priority = 5 WHERE provider = 'torbox'`); err != nil {
		t.Fatal(err)
	}
	got, order, err := loadProviders(t.Context(), q, cfg, log)
	if err != nil || len(got) != 2 || got[provider.RealDebrid] == nil || got[provider.TorBox] == nil {
		t.Fatalf("%v %v", got, err)
	}
	if len(order) != 2 || order[0] != provider.TorBox || order[1] != provider.RealDebrid {
		t.Errorf("order = %v, want by priority", order)
	}
	if got[provider.RealDebrid].Name() != provider.RealDebrid || got[provider.TorBox].Name() != provider.TorBox {
		t.Error("provider mixed up")
	}

	if _, err := pool.Exec(t.Context(), `UPDATE debrid_accounts SET enabled = false WHERE provider = 'torbox'`); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := loadProviders(t.Context(), q, cfg, log); len(got) != 1 || got[provider.TorBox] != nil {
		t.Errorf("disabled account loaded: %v", got)
	}
	other := config.Config{SecretKey: strings.Repeat("cd", secret.KeyLen)}
	if got, _, err := loadProviders(t.Context(), q, other, log); err != nil || len(got) != 0 {
		t.Errorf("another secret_key: %v %v", got, err)
	}
	if got, _, err := loadProviders(t.Context(), q, config.Config{}, log); err != nil || len(got) != 0 {
		t.Errorf("no secret_key: %v %v", got, err)
	}
}
