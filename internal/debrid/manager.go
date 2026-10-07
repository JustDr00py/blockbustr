// Package debrid manages the debrid accounts from the admin UI (TASKS P4.3,
// DESIGN §4): keys are sealed under secret_key like the config bootstrap
// does, and every change reloads the live provider.Set the resolver and the
// stream collector read, so it applies without a restart. Keys are never
// returned.
package debrid

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/sysadmin/blockbustr/internal/provider"
	"github.com/sysadmin/blockbustr/internal/secret"
	"github.com/sysadmin/blockbustr/internal/store/pg/db"
)

// Errors.
var (
	ErrNoSecretKey = errors.New("debrid: secret_key is required to store API keys (BLOCKBUSTR_SECRET_KEY)")
	ErrNotFound    = errors.New("debrid: no such account")
	ErrBadInput    = errors.New("debrid: bad input")
)

// CanStore reports whether keys can be stored (a secret_key is set).
func (m *Manager) CanStore() bool { return m.Key != nil }

// Manager changes accounts and keeps Set current.
type Manager struct {
	Q   *db.Queries
	Key []byte // secret_key; nil when unset
	Set *provider.Set
	// Load opens the enabled accounts (main's loadProviders).
	Load func(ctx context.Context) (map[provider.Name]provider.Provider, []provider.Name, error)
	Log  *slog.Logger

	mu sync.Mutex // serialises changes and their reload
}

// Account is an account as the admin UI shows it.
type Account struct {
	Provider provider.Name
	Enabled  bool
	Priority int
	// Active: opened and in use (enabled, and its key could be opened).
	Active bool
}

// Status is a live check of an account's key.
type Status struct {
	OK           bool
	PremiumUntil time.Time `json:",omitzero"`
	Error        string    `json:",omitempty"`
}

// List returns every stored account, highest priority first.
func (m *Manager) List(ctx context.Context) ([]Account, error) {
	rows, err := m.Q.ListDebridAccounts(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Account, 0, len(rows))
	for _, r := range rows {
		name := provider.Name(r.Provider)
		out = append(out, Account{Provider: name, Enabled: r.Enabled, Priority: int(r.Priority), Active: m.Set.Get(name) != nil})
	}
	return out, nil
}

// Put stores a new key (when apiKey is set; this also enables the account)
// and/or changes enabled and priority, then reloads.
func (m *Manager) Put(ctx context.Context, name provider.Name, apiKey *string, enabled *bool, priority *int) error {
	if _, err := provider.ParseName(string(name)); err != nil {
		return fmt.Errorf("%w: %w", ErrBadInput, err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if apiKey != nil {
		if m.Key == nil {
			return ErrNoSecretKey
		}
		if *apiKey == "" {
			return fmt.Errorf("%w: empty API key", ErrBadInput)
		}
		sealed, err := secret.Seal(m.Key, []byte(*apiKey))
		if err != nil {
			return err
		}
		if err := m.Q.UpsertDebridAccount(ctx, db.UpsertDebridAccountParams{Provider: string(name), ApiKeyEnc: sealed}); err != nil {
			return err
		}
	}
	if enabled != nil || priority != nil {
		p := db.SetDebridAccountParams{Provider: string(name), Enabled: enabled}
		if priority != nil {
			p.Priority = ptr(int32(*priority))
		}
		n, err := m.Q.SetDebridAccount(ctx, p)
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrNotFound
		}
	}
	return m.reload(ctx)
}

// Delete removes an account (and what blockbustr remembered of its
// torrents), then reloads. A key in config.yaml or .env comes back on the
// next start.
func (m *Manager) Delete(ctx context.Context, name provider.Name) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.Q.DeleteDebridAccountByProvider(ctx, string(name)); err != nil {
		return err
	}
	return m.reload(ctx)
}

// Check asks the provider whether the account's key works.
func (m *Manager) Check(ctx context.Context, name provider.Name) Status {
	p := m.Set.Get(name)
	if p == nil {
		return Status{Error: "not active (disabled, or its key couldn't be opened)"}
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	acct, err := p.AccountInfo(ctx)
	if err != nil {
		return Status{Error: err.Error()}
	}
	st := Status{OK: true, PremiumUntil: acct.PremiumUntil}
	if !acct.PremiumUntil.IsZero() && acct.PremiumUntil.Before(time.Now()) {
		st.OK, st.Error = false, "premium expired"
	}
	return st
}

func (m *Manager) reload(ctx context.Context) error {
	byName, order, err := m.Load(ctx)
	if err != nil {
		return fmt.Errorf("debrid: reload: %w", err)
	}
	m.Set.Replace(byName, order)
	if m.Log != nil {
		m.Log.InfoContext(ctx, "debrid accounts reloaded", "active", len(order))
	}
	return nil
}

func ptr[T any](v T) *T { return &v }
