package console

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/rizkysiregar/marpokat/server/internal/platform/httpx"
	"github.com/rizkysiregar/marpokat/server/internal/store"
)

// Codes of the platform admin's control over projects (PLAN D79).
const (
	CodeProjectSuspended = "project_suspended"
	CodeTargetIsAdmin    = "target_is_admin"
)

var (
	errProjectSuspended = httpx.NewError(409, CodeProjectSuspended, "this project is suspended by Marpokat; contact us to lift the suspension")
	errTargetIsAdmin    = httpx.NewError(403, CodeTargetIsAdmin, "this belongs to a platform administrator: take the admin rights away first (marpokat account demote)")
)

var environments = map[string]bool{"development": true, "staging": true, "production": true}

// environmentOf checks a project label (PLAN D78): "" means none.
func environmentOf(raw string) (*string, error) {
	v := strings.ToLower(strings.TrimSpace(raw))
	if v == "" {
		return nil, nil
	}
	if !environments[v] {
		return nil, httpx.FieldErrors{"environment": "must be development, staging, production or empty"}
	}
	return &v, nil
}

// adminTarget loads a project for an admin action, with its owner. A project of another platform administrator is
// out of reach: a leaked admin session must not be able to take down another administrator's work.
func (s *Service) adminTarget(ctx context.Context, p Principal, id uuid.UUID) (store.Tenant, *store.Account, error) {
	if err := requireAdmin(p); err != nil {
		return store.Tenant{}, nil, err
	}
	row, err := s.q.GetTenantByID(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return store.Tenant{}, nil, errProjectNotFound
	}
	if err != nil {
		return store.Tenant{}, nil, fmt.Errorf("console: project: %w", err)
	}
	if !row.AccountID.Valid {
		return row, nil, nil
	}
	owner, err := s.q.GetAccountByID(ctx, row.AccountID.UUID)
	if err != nil {
		return store.Tenant{}, nil, fmt.Errorf("console: owner: %w", err)
	}
	if owner.IsPlatformAdmin && owner.ID != p.Account.ID {
		return store.Tenant{}, nil, errTargetIsAdmin
	}
	return row, &owner, nil
}

func validReason(raw string) (string, error) {
	r := strings.TrimSpace(raw)
	if r == "" || utf8.RuneCountInString(r) > 500 {
		return "", httpx.FieldErrors{"reason": "say why, in 1 to 500 characters: the owner reads it"}
	}
	return r, nil
}

// AdminSuspendProject stops a project at once (keys refused, sockets closed, no pushes) until an admin lifts it. Its
// data stays. The owner is told why, by e-mail and on the project page, and cannot switch it back on.
func (s *Service) AdminSuspendProject(ctx context.Context, p Principal, id uuid.UUID, rawReason string, c Caller) (store.Tenant, error) {
	reason, err := validReason(rawReason)
	if err != nil {
		return store.Tenant{}, err
	}
	row, owner, err := s.adminTarget(ctx, p, id)
	if err != nil {
		return store.Tenant{}, err
	}
	if err := s.tenants.SetSuspended(ctx, row.ID, true, reason); err != nil {
		return store.Tenant{}, err
	}
	s.audit(ctx, p.Account.ID, "project_suspended_by_admin", strp(row.Slug), c)
	if owner != nil {
		s.audit(ctx, owner.ID, "project_suspended", strp(row.Slug), c)
		s.sendMail(projectSuspendedMessage(owner.Email, owner.Name, row.Name, reason, s.opts.BaseURL+"/app/projects/"+row.ID.String()))
	}
	return s.q.GetTenantByID(ctx, row.ID)
}

// AdminUnsuspendProject lifts a suspension. A project its owner had switched off stays off.
func (s *Service) AdminUnsuspendProject(ctx context.Context, p Principal, id uuid.UUID, c Caller) (store.Tenant, error) {
	row, owner, err := s.adminTarget(ctx, p, id)
	if err != nil {
		return store.Tenant{}, err
	}
	if row.SuspendedAt == nil {
		return row, nil
	}
	if err := s.tenants.SetSuspended(ctx, row.ID, false, ""); err != nil {
		return store.Tenant{}, err
	}
	s.audit(ctx, p.Account.ID, "project_unsuspended_by_admin", strp(row.Slug), c)
	if owner != nil {
		s.audit(ctx, owner.ID, "project_unsuspended", strp(row.Slug), c)
		s.sendMail(projectUnsuspendedMessage(owner.Email, owner.Name, row.Name))
	}
	return s.q.GetTenantByID(ctx, row.ID)
}

// AdminDeleteProject deletes a project: after the grace period like its owner would, or now (the worker removes all
// its data within a minute). The admin types the project's slug and their own password, since "now" cannot be undone.
func (s *Service) AdminDeleteProject(ctx context.Context, p Principal, id uuid.UUID, confirmSlug, password string, now bool, c Caller) (store.Tenant, error) {
	row, owner, err := s.adminTarget(ctx, p, id)
	if err != nil {
		return store.Tenant{}, err
	}
	if strings.TrimSpace(confirmSlug) != row.Slug {
		return store.Tenant{}, httpx.FieldErrors{"confirm": "type the project's slug exactly"}
	}
	if err := s.confirm(ctx, p, password, c); err != nil {
		return store.Tenant{}, err
	}
	if err := s.tenants.SetDisabled(ctx, row.Slug, true); err != nil {
		return store.Tenant{}, err
	}
	var out store.Tenant
	if now {
		out, err = s.q.SetTenantDeleteNow(ctx, row.ID)
	} else {
		out, err = s.q.ScheduleTenantDeletion(ctx, store.ScheduleTenantDeletionParams{ID: row.ID, DeleteAfter: s.now().Add(s.opts.DeletionGrace)})
	}
	if err != nil {
		return store.Tenant{}, fmt.Errorf("console: delete project: %w", err)
	}
	action := "project_deletion_scheduled_by_admin"
	if now {
		action = "project_deleted_now_by_admin"
	}
	s.audit(ctx, p.Account.ID, action, strp(row.Slug), c)
	if owner != nil {
		s.audit(ctx, owner.ID, "project_deleted_by_admin", strp(row.Slug), c)
		when := "within a few minutes"
		if !now && out.DeleteAfter != nil {
			when = "on " + out.DeleteAfter.UTC().Format("2 January 2006, 15:04 UTC")
		}
		s.sendMail(projectDeletedByAdminMessage(owner.Email, owner.Name, row.Name, when))
	}
	return out, nil
}

// AdminRestoreProject cancels a deletion that is still in its grace period. The project stays switched off.
func (s *Service) AdminRestoreProject(ctx context.Context, p Principal, id uuid.UUID, c Caller) (store.Tenant, error) {
	row, owner, err := s.adminTarget(ctx, p, id)
	if err != nil {
		return store.Tenant{}, err
	}
	if row.DeleteAfter == nil {
		return row, nil
	}
	if !row.DeleteAfter.After(s.now().Add(5 * time.Second)) {
		return store.Tenant{}, httpx.NewError(409, CodeDeletionScheduled, "this project is being removed right now; it cannot be restored")
	}
	out, err := s.q.CancelTenantDeletion(ctx, row.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		return store.Tenant{}, errProjectNotFound
	}
	if err != nil {
		return store.Tenant{}, fmt.Errorf("console: restore: %w", err)
	}
	s.audit(ctx, p.Account.ID, "project_restored_by_admin", strp(row.Slug), c)
	if owner != nil {
		s.audit(ctx, owner.ID, "project_restored", strp(row.Slug), c)
	}
	return out, nil
}
