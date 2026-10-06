// Package pg is blockbustr's PostgreSQL store: connection setup and the
// embedded goose migrations (DESIGN §4).
package pg

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"log/slog"

	"github.com/pressly/goose/v3"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// Migration commands accepted by Migrate (and `blockbustr -migrate`).
const (
	MigrateUp      = "up"       // apply every pending migration
	MigrateDown    = "down"     // roll back the most recent migration
	MigrateDownAll = "down-all" // roll back everything (dev only)
	MigrateStatus  = "status"   // log applied/pending migrations
)

func newProvider(db *sql.DB) (*goose.Provider, error) {
	sub, err := fs.Sub(migrationFiles, "migrations")
	if err != nil {
		return nil, err
	}
	return goose.NewProvider(goose.DialectPostgres, db, sub)
}

// Migrate runs a migration command against db and logs what it did.
func Migrate(ctx context.Context, db *sql.DB, command string, log *slog.Logger) error {
	p, err := newProvider(db)
	if err != nil {
		return fmt.Errorf("migrations: %w", err)
	}
	switch command {
	case MigrateUp:
		res, err := p.Up(ctx)
		for _, r := range res {
			log.Info("migration applied", "version", r.Source.Version, "file", r.Source.Path, "took", r.Duration)
		}
		if err != nil {
			return fmt.Errorf("migrate up: %w", err)
		}
	case MigrateDown:
		r, err := p.Down(ctx)
		if err != nil {
			return fmt.Errorf("migrate down: %w", err)
		}
		log.Info("migration rolled back", "version", r.Source.Version, "file", r.Source.Path)
	case MigrateDownAll:
		res, err := p.DownTo(ctx, 0)
		for _, r := range res {
			log.Info("migration rolled back", "version", r.Source.Version, "file", r.Source.Path)
		}
		if err != nil {
			return fmt.Errorf("migrate down-all: %w", err)
		}
	case MigrateStatus:
		st, err := p.Status(ctx)
		if err != nil {
			return fmt.Errorf("migrate status: %w", err)
		}
		for _, s := range st {
			log.Info("migration", "version", s.Source.Version, "file", s.Source.Path, "state", s.State, "applied_at", s.AppliedAt)
		}
	default:
		return fmt.Errorf("unknown migrate command %q (want up, down, down-all or status)", command)
	}
	v, err := p.GetDBVersion(ctx)
	if err != nil {
		return fmt.Errorf("migrate: read version: %w", err)
	}
	log.Info("database schema version", "version", v)
	return nil
}
