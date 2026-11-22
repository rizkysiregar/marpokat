// Package migrate applies the embedded SQL migrations with goose.
//
// It runs as its own Kubernetes Job (Argo CD PreSync hook) before api and worker roll out, so it uses no
// lock: goose's Postgres locker takes a session advisory lock, which pgbouncer in transaction mode cannot hold.
// Only one Job runs at a time anyway.
package migrate

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"

	"github.com/pressly/goose/v3"

	"github.com/rizkysiregar/marpokat/server/migrations"
)

func provider(db *sql.DB) (*goose.Provider, error) {
	p, err := goose.NewProvider(goose.DialectPostgres, db, migrations.FS)
	if err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return p, nil
}

// Up applies every pending migration.
func Up(ctx context.Context, db *sql.DB, log *slog.Logger) error {
	p, err := provider(db)
	if err != nil {
		return err
	}
	results, err := p.Up(ctx)
	for _, r := range results {
		log.Info("migration applied", "migration", r.Source.Version, "file", r.Source.Path, "duration", r.Duration.String())
	}
	if err != nil {
		return fmt.Errorf("migrate up: %w", err)
	}
	v, err := p.GetDBVersion(ctx)
	if err != nil {
		return fmt.Errorf("migrate: read version: %w", err)
	}
	log.Info("database is up to date", "db_version", v, "applied_now", len(results))
	return nil
}

// Down rolls back the latest migration only. Meant for local development, never run by the Job.
func Down(ctx context.Context, db *sql.DB, log *slog.Logger) error {
	p, err := provider(db)
	if err != nil {
		return err
	}
	r, err := p.Down(ctx)
	if err != nil {
		return fmt.Errorf("migrate down: %w", err)
	}
	log.Info("migration rolled back", "migration", r.Source.Version, "file", r.Source.Path)
	return nil
}

// Status writes one line per migration: applied time or "pending".
func Status(ctx context.Context, db *sql.DB, w io.Writer) error {
	p, err := provider(db)
	if err != nil {
		return err
	}
	statuses, err := p.Status(ctx)
	if err != nil {
		return fmt.Errorf("migrate status: %w", err)
	}
	for _, s := range statuses {
		applied := "pending"
		if s.State == goose.StateApplied {
			applied = s.AppliedAt.UTC().Format("2006-01-02 15:04:05 UTC")
		}
		_, _ = fmt.Fprintf(w, "%05d  %-30s  %s\n", s.Source.Version, s.Source.Path, applied)
	}
	return nil
}
