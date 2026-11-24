package tenant

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rizkysiregar/marpokat/server/internal/store"
)

// ObjectRemover removes every object under a key prefix. A tenant's files are all under "<tenant id>/".
type ObjectRemover interface {
	RemovePrefix(ctx context.Context, prefix string) (int, error)
}

// Purger removes the projects whose deletion grace period is over (PLAN D75): their files first, then every row of
// theirs in one transaction, the tenant row last. The tenant stays locked (FOR UPDATE SKIP LOCKED) for the whole time,
// so two workers never purge the same one; if anything fails the transaction is rolled back and the next round tries
// again. Files already removed stay removed, which is what was asked.
type Purger struct {
	Pool    *pgxpool.Pool
	Objects ObjectRemover // nil: no object storage, nothing to remove there
	Log     *slog.Logger
	Every   time.Duration // default 1 minute: an admin's "delete now" is done within it (PLAN D79)
}

// Once purges every project that is due and returns how many went.
func (p *Purger) Once(ctx context.Context) (int, error) {
	purged := 0
	for {
		done, err := p.one(ctx)
		if err != nil || !done {
			return purged, err
		}
		purged++
	}
}

func (p *Purger) one(ctx context.Context) (bool, error) {
	found := false
	err := pgx.BeginFunc(ctx, p.Pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		t, err := q.LockDueTenantDeletion(ctx)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("lock: %w", err)
		}
		found = true
		files := 0
		if p.Objects != nil {
			if files, err = p.Objects.RemovePrefix(ctx, t.ID.String()+"/"); err != nil {
				return fmt.Errorf("files of %s: %w", t.Slug, err)
			}
		}
		steps := []struct {
			name string
			run  func() error
		}{
			{"channels", func() error { return q.PurgeTenantChannels(ctx, t.ID) }},
			{"attachments", func() error { return q.PurgeTenantAttachments(ctx, t.ID) }},
			{"devices", func() error { return q.PurgeTenantDevices(ctx, t.ID) }},
			{"outbox", func() error { return q.PurgeTenantOutbox(ctx, t.ID) }},
			{"users", func() error { return q.PurgeTenantUsers(ctx, t.ID) }},
			{"tenant", func() error { return q.DeleteTenantRow(ctx, t.ID) }},
		}
		for _, s := range steps {
			if err := s.run(); err != nil {
				return fmt.Errorf("%s of %s: %w", s.name, t.Slug, err)
			}
		}
		if t.AccountID.Valid { // the owner sees it in their security log
			slug := t.Slug
			if err := q.InsertAudit(ctx, store.InsertAuditParams{AccountID: t.AccountID.UUID, Action: "project_deleted", Target: &slug}); err != nil {
				return fmt.Errorf("audit: %w", err)
			}
		}
		p.Log.Info("project deleted for good", "slug", t.Slug, "tenant", t.ID, "files", files)
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("tenant purge: %w", err)
	}
	return found, nil
}

// Run purges until ctx ends.
func (p *Purger) Run(ctx context.Context) {
	every := p.Every
	if every <= 0 {
		every = time.Minute
	}
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		if n, err := p.Once(ctx); err != nil {
			p.Log.Warn("purging deleted projects failed", "error", err, "purged", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
