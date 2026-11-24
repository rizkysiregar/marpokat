package tenant

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rizkysiregar/marpokat/server/internal/store"
)

// ObjectRemover removes every object under a key prefix. A tenant's files are all under "<tenant id>/".
type ObjectRemover interface {
	RemovePrefix(ctx context.Context, prefix string) (int, error)
}

// Purger removes what was deleted once its grace period is over (PLAN D75, D80):
//
//   - a project: its files first, then every row of it in one transaction, the tenant row last; its owner gets
//     project_deleted in their security log;
//   - an account: every project of it the same way, then the account (its sessions, tokens and security log go with
//     it), in one transaction.
//
// The row being purged stays locked (FOR UPDATE SKIP LOCKED) for the whole time, so two workers never purge the same
// one; if anything fails the transaction is rolled back and the next round tries again. Files already removed stay
// removed, which is what was asked.
type Purger struct {
	Pool    *pgxpool.Pool
	Objects ObjectRemover // nil: no object storage, nothing to remove there
	Log     *slog.Logger
	Every   time.Duration // default 1 minute: an admin's "delete now" is done within it (PLAN D79)
}

// Once purges every project and account that is due and returns how many went (projects and accounts together).
func (p *Purger) Once(ctx context.Context) (int, error) {
	purged := 0
	for _, step := range []func(context.Context) (bool, error){p.oneProject, p.oneAccount} {
		for {
			done, err := step(ctx)
			if err != nil {
				return purged, err
			}
			if !done {
				break
			}
			purged++
		}
	}
	return purged, nil
}

func (p *Purger) oneProject(ctx context.Context) (bool, error) {
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
		if err := p.purgeTenant(ctx, q, t.ID, t.Slug); err != nil {
			return err
		}
		if t.AccountID.Valid { // the owner sees it in their security log
			slug := t.Slug
			if err := q.InsertAudit(ctx, store.InsertAuditParams{AccountID: t.AccountID.UUID, Action: "project_deleted", Target: &slug}); err != nil {
				return fmt.Errorf("audit: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("project purge: %w", err)
	}
	return found, nil
}

func (p *Purger) oneAccount(ctx context.Context) (bool, error) {
	found := false
	err := pgx.BeginFunc(ctx, p.Pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		acc, err := q.LockDueAccountDeletion(ctx)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("lock: %w", err)
		}
		found = true
		projects, err := q.ListTenantIDsOfAccount(ctx, uuid.NullUUID{UUID: acc.ID, Valid: true})
		if err != nil {
			return fmt.Errorf("projects of %s: %w", acc.ID, err)
		}
		for _, t := range projects {
			if err := p.purgeTenant(ctx, q, t.ID, t.Slug); err != nil {
				return err
			}
		}
		if err := q.DeleteAccountRow(ctx, acc.ID); err != nil {
			return fmt.Errorf("account %s: %w", acc.ID, err)
		}
		p.Log.Info("account deleted for good", "account", acc.ID, "projects", len(projects))
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("account purge: %w", err)
	}
	return found, nil
}

// purgeTenant removes one tenant's files and every row of it, through q (the caller's transaction).
func (p *Purger) purgeTenant(ctx context.Context, q *store.Queries, id uuid.UUID, slug string) error {
	files := 0
	if p.Objects != nil {
		var err error
		if files, err = p.Objects.RemovePrefix(ctx, id.String()+"/"); err != nil {
			return fmt.Errorf("files of %s: %w", slug, err)
		}
	}
	steps := []struct {
		name string
		run  func() error
	}{
		// channels take their members, messages, reactions and message attachments with them (ON DELETE CASCADE)
		{"channels", func() error { return q.PurgeTenantChannels(ctx, id) }},
		{"attachments", func() error { return q.PurgeTenantAttachments(ctx, id) }},
		{"devices", func() error { return q.PurgeTenantDevices(ctx, id) }},
		{"outbox", func() error { return q.PurgeTenantOutbox(ctx, id) }},
		{"users", func() error { return q.PurgeTenantUsers(ctx, id) }},
		{"tenant", func() error { return q.DeleteTenantRow(ctx, id) }},
	}
	for _, s := range steps {
		if err := s.run(); err != nil {
			return fmt.Errorf("%s of %s: %w", s.name, slug, err)
		}
	}
	p.Log.Info("project deleted for good", "slug", slug, "tenant", id, "files", files)
	return nil
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
			p.Log.Warn("purging deleted projects and accounts failed", "error", err, "purged", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
