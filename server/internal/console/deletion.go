package console

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/rizkysiregar/marpokat/server/internal/platform/httpx"
	"github.com/rizkysiregar/marpokat/server/internal/store"
)

var errDeletionScheduled = httpx.NewError(409, CodeDeletionScheduled, "this project is about to be deleted; restore it first")

// DeleteProject schedules a project for deletion (PLAN D75): it is switched off now and removed with all its data by
// the worker when the grace period is over. Until then the owner can restore it. Asks the password, like every
// action that cannot be undone.
func (s *Service) DeleteProject(ctx context.Context, p Principal, id uuid.UUID, password string, c Caller) (store.Tenant, error) {
	row, err := s.project(ctx, p, id)
	if err != nil {
		return store.Tenant{}, err
	}
	if row.DeleteAfter != nil { // asking twice changes nothing, and needs no password
		return row, nil
	}
	if err := s.confirm(ctx, p, password, c); err != nil {
		return store.Tenant{}, err
	}
	// through the tenant service first, so that the API stops accepting its keys (cache: 30 seconds at most)
	if err := s.tenants.SetDisabled(ctx, row.Slug, true); err != nil {
		return store.Tenant{}, err
	}
	out, err := s.q.ScheduleTenantDeletion(ctx, store.ScheduleTenantDeletionParams{ID: row.ID, DeleteAfter: s.now().Add(s.opts.DeletionGrace)})
	if err != nil {
		return store.Tenant{}, fmt.Errorf("console: schedule deletion: %w", err)
	}
	s.audit(ctx, p.Account.ID, "project_deletion_scheduled", strp(row.Slug), c)
	return out, nil
}

// RestoreProject cancels a scheduled deletion. The project stays switched off: its owner switches it on when ready.
func (s *Service) RestoreProject(ctx context.Context, p Principal, id uuid.UUID, c Caller) (store.Tenant, error) {
	row, err := s.project(ctx, p, id)
	if err != nil {
		return store.Tenant{}, err
	}
	if row.DeleteAfter == nil {
		return row, nil
	}
	out, err := s.q.CancelTenantDeletion(ctx, row.ID)
	if errors.Is(err, pgx.ErrNoRows) { // purged in the meantime
		return store.Tenant{}, errProjectNotFound
	}
	if err != nil {
		return store.Tenant{}, fmt.Errorf("console: restore: %w", err)
	}
	s.audit(ctx, p.Account.ID, "project_restored", strp(row.Slug), c)
	return out, nil
}

// EndSession signs out one of the account's own sessions (a lost laptop). Ending the current one is a sign out.
func (s *Service) EndSession(ctx context.Context, p Principal, sessionID uuid.UUID, c Caller) error {
	n, err := s.q.DeleteSessionOfAccount(ctx, store.DeleteSessionOfAccountParams{ID: sessionID, AccountID: p.Account.ID})
	if err != nil {
		return fmt.Errorf("console: end session: %w", err)
	}
	if n == 0 {
		return httpx.ErrNotFound("session not found")
	}
	s.audit(ctx, p.Account.ID, "session_ended", nil, c)
	return nil
}

// RegistrationOpen says whether the sign up page should offer the form.
func (s *Service) RegistrationOpen() bool { return !s.opts.RegistrationClosed }

// likePattern turns a search into an ILIKE pattern that matches it anywhere, with the wildcards of the search itself
// escaped. Empty stays empty (no filter).
func likePattern(search string) string {
	search = strings.TrimSpace(search)
	if search == "" {
		return ""
	}
	if len(search) > 100 {
		search = search[:100]
	}
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return "%" + r.Replace(search) + "%"
}
