package stremio

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sysadmin/blockbustr/internal/secret"
	"github.com/sysadmin/blockbustr/internal/store/pg/db"
)

// Registry errors.
var (
	ErrNoSecretKey     = errors.New("stremio: secret_key is required to store addon URLs (BLOCKBUSTR_SECRET_KEY)")
	ErrDuplicate       = errors.New("stremio: addon already added")
	ErrNotFound        = errors.New("stremio: no such addon or catalog")
	ErrCatalogNeedsArg = errors.New("stremio: catalog needs an extra (search, genre…) and can't be a library")
)

// Registry stores the configured addons and their catalogs (DESIGN §4, §7).
// Addon URLs are sealed under secret_key and only opened for requests.
type Registry struct {
	Pool   *pgxpool.Pool
	Client *Client
	// Key is the parsed secret_key; nil refuses to add addons.
	Key []byte
}

// Addon is an addon as the admin API shows it: never with its URL.
type Addon struct {
	ID            uuid.UUID
	Host          string
	Manifest      Manifest
	Enabled       bool
	Priority      int
	LastFetchedAt time.Time
	Catalogs      []Catalog
}

// Catalog is one of an addon's catalogs.
type Catalog struct {
	Type, ID, Name string
	Enabled        bool
	LibraryID      *uuid.UUID
	// Requires lists the extras it can't be fetched without; such a
	// catalog can't be a library.
	Requires []string
}

func (r *Registry) q() *db.Queries { return db.New(r.Pool) }

// Add fetches the manifest at rawURL (as users paste it) and stores the
// addon with its catalogs, all disabled.
func (r *Registry) Add(ctx context.Context, rawURL string, priority int) (Addon, error) {
	if r.Key == nil {
		return Addon{}, ErrNoSecretKey
	}
	base, err := BaseURL(rawURL)
	if err != nil {
		return Addon{}, err
	}
	m, raw, err := r.manifest(ctx, base)
	if err != nil {
		return Addon{}, err
	}
	enc, err := secret.Seal(r.Key, []byte(base))
	if err != nil {
		return Addon{}, err
	}
	sum := sha256.Sum256([]byte(base))
	var row db.StremioAddon
	err = pgx.BeginFunc(ctx, r.Pool, func(tx pgx.Tx) error {
		q := db.New(tx)
		row, err = q.InsertStremioAddon(ctx, db.InsertStremioAddonParams{
			UrlEnc: enc, UrlSha: sum[:], Host: Redact(base), Manifest: raw, Priority: int32(priority),
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrDuplicate
		}
		if err != nil {
			return err
		}
		return syncCatalogs(ctx, q, row.ID, m)
	})
	if err != nil {
		return Addon{}, err
	}
	return r.view(ctx, row)
}

// manifest fetches and re-encodes the manifest (dropping fields blockbustr
// doesn't read, such as a configured addon's echoed settings).
func (r *Registry) manifest(ctx context.Context, base string) (Manifest, []byte, error) {
	m, err := r.Client.Manifest(ctx, base)
	if err != nil {
		return m, nil, err
	}
	raw, err := json.Marshal(m)
	return m, raw, err
}

func syncCatalogs(ctx context.Context, q *db.Queries, addon uuid.UUID, m Manifest) error {
	keep := make([]string, 0, len(m.Catalogs))
	for _, c := range m.Catalogs {
		if err := q.UpsertStremioCatalog(ctx, db.UpsertStremioCatalogParams{
			AddonID: addon, CatalogType: c.Type, CatalogID: c.ID, Name: c.Name,
		}); err != nil {
			return err
		}
		keep = append(keep, c.Type+"/"+c.ID)
	}
	return q.DeleteStremioCatalogsExcept(ctx, db.DeleteStremioCatalogsExceptParams{AddonID: addon, Keep: keep})
}

// Refresh refetches an addon's manifest: catalogs it added appear
// disabled, renamed ones keep their state, removed ones go.
func (r *Registry) Refresh(ctx context.Context, id uuid.UUID) (Addon, error) {
	row, base, err := r.open(ctx, id)
	if err != nil {
		return Addon{}, err
	}
	m, raw, err := r.manifest(ctx, base)
	if err != nil {
		return Addon{}, err
	}
	err = pgx.BeginFunc(ctx, r.Pool, func(tx pgx.Tx) error {
		q := db.New(tx)
		if err := q.SetStremioAddonManifest(ctx, db.SetStremioAddonManifestParams{ID: id, Manifest: raw}); err != nil {
			return err
		}
		return syncCatalogs(ctx, q, id, m)
	})
	if err != nil {
		return Addon{}, err
	}
	row.Manifest, row.LastFetchedAt = raw, time.Now()
	return r.view(ctx, row)
}

// open loads an addon and its base URL.
func (r *Registry) open(ctx context.Context, id uuid.UUID) (db.StremioAddon, string, error) {
	row, err := r.q().GetStremioAddon(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return row, "", ErrNotFound
	}
	if err != nil {
		return row, "", err
	}
	base, err := r.Base(row)
	return row, base, err
}

// Base opens an addon's URL for requests (sync, stream collection).
func (r *Registry) Base(row db.StremioAddon) (string, error) {
	if r.Key == nil {
		return "", ErrNoSecretKey
	}
	b, err := secret.Open(r.Key, row.UrlEnc)
	if err != nil {
		return "", fmt.Errorf("stremio %s: %w", row.Host, err)
	}
	return string(b), nil
}

// List returns every addon, highest priority first.
func (r *Registry) List(ctx context.Context) ([]Addon, error) {
	rows, err := r.q().ListStremioAddons(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Addon, 0, len(rows))
	for _, row := range rows {
		a, err := r.view(ctx, row)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, nil
}

// Get returns one addon.
func (r *Registry) Get(ctx context.Context, id uuid.UUID) (Addon, error) {
	row, err := r.q().GetStremioAddon(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return Addon{}, ErrNotFound
	}
	if err != nil {
		return Addon{}, err
	}
	return r.view(ctx, row)
}

// Update changes whether an addon is used and its priority; nil leaves a
// field as it is.
func (r *Registry) Update(ctx context.Context, id uuid.UUID, enabled *bool, priority *int) (Addon, error) {
	row, err := r.q().GetStremioAddon(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return Addon{}, ErrNotFound
	}
	if err != nil {
		return Addon{}, err
	}
	p := db.UpdateStremioAddonParams{ID: id, Enabled: row.Enabled, Priority: row.Priority}
	if enabled != nil {
		p.Enabled = *enabled
	}
	if priority != nil {
		p.Priority = int32(*priority)
	}
	if row, err = r.q().UpdateStremioAddon(ctx, p); err != nil {
		return Addon{}, err
	}
	return r.view(ctx, row)
}

// Delete removes an addon and its catalogs.
func (r *Registry) Delete(ctx context.Context, id uuid.UUID) error {
	n, err := r.q().DeleteStremioAddon(ctx, id)
	if err == nil && n == 0 {
		err = ErrNotFound
	}
	return err
}

// SetCatalogEnabled turns one catalog into a library or back (the library
// itself is made by the sync, P3.5).
func (r *Registry) SetCatalogEnabled(ctx context.Context, id uuid.UUID, typ, catalog string, enabled bool) (Addon, error) {
	a, err := r.Get(ctx, id)
	if err != nil {
		return Addon{}, err
	}
	var found *Catalog
	for i := range a.Catalogs {
		if a.Catalogs[i].Type == typ && a.Catalogs[i].ID == catalog {
			found = &a.Catalogs[i]
		}
	}
	if found == nil {
		return Addon{}, ErrNotFound
	}
	if enabled && len(found.Requires) > 0 {
		return Addon{}, ErrCatalogNeedsArg
	}
	if _, err := r.q().SetStremioCatalogEnabled(ctx, db.SetStremioCatalogEnabledParams{
		AddonID: id, CatalogType: typ, CatalogID: catalog, Enabled: enabled,
	}); err != nil {
		return Addon{}, err
	}
	found.Enabled = enabled
	return a, nil
}

func (r *Registry) view(ctx context.Context, row db.StremioAddon) (Addon, error) {
	a := Addon{ID: row.ID, Host: row.Host, Enabled: row.Enabled, Priority: int(row.Priority), LastFetchedAt: row.LastFetchedAt}
	if err := json.Unmarshal(row.Manifest, &a.Manifest); err != nil {
		return a, fmt.Errorf("stremio %s: stored manifest: %w", row.Host, err)
	}
	defs := map[string]CatalogDef{}
	for _, d := range a.Manifest.Catalogs {
		defs[d.Type+"/"+d.ID] = d
	}
	cats, err := r.q().ListStremioCatalogs(ctx, row.ID)
	if err != nil {
		return a, err
	}
	a.Catalogs = make([]Catalog, 0, len(cats))
	for _, c := range cats {
		a.Catalogs = append(a.Catalogs, Catalog{
			Type: c.CatalogType, ID: c.CatalogID, Name: c.Name, Enabled: c.Enabled,
			LibraryID: c.LibraryID, Requires: defs[c.CatalogType+"/"+c.CatalogID].Requires(),
		})
	}
	return a, nil
}
