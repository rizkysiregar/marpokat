package console

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/rizkysiregar/marpokat/server/internal/platform/httpx"
	"github.com/rizkysiregar/marpokat/server/internal/store"
)

// Control over accounts (PLAN D80). Deletion by the owner always has the grace period; the platform admin chooses
// between the grace period and now. A suspended or deleted account takes its projects down with it.

const CodeAccountDeletionScheduled = "account_deletion_scheduled"

var errAccountDeleting = httpx.NewError(409, CodeAccountDeletionScheduled, "this account is being deleted; restore it first")

// syncAccountProjects makes the account's projects follow it: suspended (source "account") while the account is
// suspended or waiting to be deleted, released again when it is neither. A suspension the admin put on a project
// itself is never touched. Every change is announced, so keys and sockets stop or work again at once.
func (s *Service) syncAccountProjects(ctx context.Context, acc store.Account) error {
	if acc.DisabledAt != nil || acc.DeleteAfter != nil {
		reason := "The account is being deleted."
		if acc.DisabledAt != nil {
			reason = "The account is suspended."
			if acc.SuspendedReason != nil {
				reason = "The account is suspended: " + *acc.SuspendedReason
			}
		}
		ids, err := s.q.SuspendTenantsOfAccount(ctx, store.SuspendTenantsOfAccountParams{AccountID: acc.ID, Reason: reason})
		if err != nil {
			return fmt.Errorf("console: suspend the account's projects: %w", err)
		}
		for _, id := range ids {
			s.tenants.Announce(ctx, id, true)
		}
		return nil
	}
	ids, err := s.q.LiftAccountSuspensionsOfTenants(ctx, uuid.NullUUID{UUID: acc.ID, Valid: true})
	if err != nil {
		return fmt.Errorf("console: release the account's projects: %w", err)
	}
	for _, id := range ids {
		s.tenants.Announce(ctx, id, false)
	}
	return nil
}

// adminAccountTarget loads an account for an admin action: not oneself, and not another platform administrator (a
// leaked admin session must not take another administrator down: demote them first with the CLI).
func (s *Service) adminAccountTarget(ctx context.Context, p Principal, id uuid.UUID) (store.Account, error) {
	if err := requireAdmin(p); err != nil {
		return store.Account{}, err
	}
	if id == p.Account.ID {
		return store.Account{}, httpx.NewError(400, httpx.CodeBadRequest, "not on your own account")
	}
	acc, err := s.q.GetAccountByID(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return store.Account{}, errAccountNotFound
	}
	if err != nil {
		return store.Account{}, fmt.Errorf("console: account: %w", err)
	}
	if acc.IsPlatformAdmin {
		return store.Account{}, errTargetIsAdmin
	}
	return acc, nil
}

// AdminSuspendAccount blocks an account: no sign in, every session ended, every project of it suspended. The e-mail
// address stays taken, so the person cannot come back under it.
func (s *Service) AdminSuspendAccount(ctx context.Context, p Principal, id uuid.UUID, rawReason string, c Caller) (store.Account, error) {
	reason, err := validReason(rawReason)
	if err != nil {
		return store.Account{}, err
	}
	if _, err := s.adminAccountTarget(ctx, p, id); err != nil {
		return store.Account{}, err
	}
	acc, err := s.q.SetAccountSuspended(ctx, store.SetAccountSuspendedParams{ID: id, Suspended: true, Reason: &reason})
	if err != nil {
		return store.Account{}, fmt.Errorf("console: suspend account: %w", err)
	}
	if err := s.q.DeleteAccountSessions(ctx, id); err != nil {
		return store.Account{}, fmt.Errorf("console: end sessions: %w", err)
	}
	if err := s.syncAccountProjects(ctx, acc); err != nil {
		return store.Account{}, err
	}
	s.audit(ctx, p.Account.ID, "account_suspended_by_admin", strp(acc.Email), c)
	s.audit(ctx, acc.ID, "account_suspended", nil, c)
	s.sendMail(accountSuspendedMessage(acc.Email, acc.Name, reason))
	return acc, nil
}

// AdminUnsuspendAccount lifts an account's suspension, and the suspensions it caused on its projects.
func (s *Service) AdminUnsuspendAccount(ctx context.Context, p Principal, id uuid.UUID, c Caller) (store.Account, error) {
	acc, err := s.adminAccountTarget(ctx, p, id)
	if err != nil {
		return store.Account{}, err
	}
	if acc.DisabledAt == nil {
		return acc, nil
	}
	if acc, err = s.q.SetAccountSuspended(ctx, store.SetAccountSuspendedParams{ID: id, Suspended: false}); err != nil {
		return store.Account{}, fmt.Errorf("console: lift suspension: %w", err)
	}
	if err := s.syncAccountProjects(ctx, acc); err != nil {
		return store.Account{}, err
	}
	s.audit(ctx, p.Account.ID, "account_unsuspended_by_admin", strp(acc.Email), c)
	s.audit(ctx, acc.ID, "account_unsuspended", nil, c)
	s.sendMail(accountUnsuspendedMessage(acc.Email, acc.Name, s.opts.BaseURL+"/login"))
	return acc, nil
}

// AdminDeleteAccount deletes an account with all its projects, after the grace period or now. The admin types the
// account's e-mail and their own password. Its sessions end at once.
func (s *Service) AdminDeleteAccount(ctx context.Context, p Principal, id uuid.UUID, confirmEmail, password string, now bool, c Caller) (store.Account, error) {
	acc, err := s.adminAccountTarget(ctx, p, id)
	if err != nil {
		return store.Account{}, err
	}
	if !strings.EqualFold(strings.TrimSpace(confirmEmail), acc.Email) {
		return store.Account{}, httpx.FieldErrors{"confirm": "type the account's e-mail exactly"}
	}
	if err := s.confirm(ctx, p, password, c); err != nil {
		return store.Account{}, err
	}
	if now {
		acc, err = s.q.SetAccountDeleteNow(ctx, id)
	} else {
		acc, err = s.q.ScheduleAccountDeletion(ctx, store.ScheduleAccountDeletionParams{ID: id, DeleteAfter: s.now().Add(s.opts.DeletionGrace)})
	}
	if err != nil {
		return store.Account{}, fmt.Errorf("console: delete account: %w", err)
	}
	if err := s.q.DeleteAccountSessions(ctx, id); err != nil {
		return store.Account{}, fmt.Errorf("console: end sessions: %w", err)
	}
	if err := s.syncAccountProjects(ctx, acc); err != nil {
		return store.Account{}, err
	}
	action := "account_deletion_scheduled_by_admin"
	if now {
		action = "account_deleted_now_by_admin"
	}
	s.audit(ctx, p.Account.ID, action, strp(acc.Email), c)
	s.audit(ctx, acc.ID, "account_deleted_by_admin", nil, c)
	s.sendMail(accountDeletedByAdminMessage(acc.Email, acc.Name, deletionWhen(acc.DeleteAfter, now)))
	return acc, nil
}

// AdminRestoreAccount cancels an account's deletion that is still in its grace period.
func (s *Service) AdminRestoreAccount(ctx context.Context, p Principal, id uuid.UUID, c Caller) (store.Account, error) {
	acc, err := s.adminAccountTarget(ctx, p, id)
	if err != nil {
		return store.Account{}, err
	}
	if acc, err = s.restoreAccount(ctx, acc); err != nil {
		return store.Account{}, err
	}
	s.audit(ctx, p.Account.ID, "account_restored_by_admin", strp(acc.Email), c)
	s.audit(ctx, acc.ID, "account_restored", nil, c)
	return acc, nil
}

// DeleteMyAccount is the owner asking for their account to go (with every project): always after the grace period,
// during which they can sign in and restore it. They type their e-mail and their password.
func (s *Service) DeleteMyAccount(ctx context.Context, p Principal, confirmEmail, password string, c Caller) (store.Account, error) {
	if p.Account.DeleteAfter != nil {
		return p.Account, nil
	}
	if !strings.EqualFold(strings.TrimSpace(confirmEmail), p.Account.Email) {
		return store.Account{}, httpx.FieldErrors{"confirm": "type your e-mail exactly"}
	}
	if err := s.confirm(ctx, p, password, c); err != nil {
		return store.Account{}, err
	}
	acc, err := s.q.ScheduleAccountDeletion(ctx, store.ScheduleAccountDeletionParams{ID: p.Account.ID, DeleteAfter: s.now().Add(s.opts.DeletionGrace)})
	if err != nil {
		return store.Account{}, fmt.Errorf("console: delete account: %w", err)
	}
	if err := s.syncAccountProjects(ctx, acc); err != nil {
		return store.Account{}, err
	}
	s.audit(ctx, acc.ID, "account_deletion_scheduled", nil, c)
	s.sendMail(accountDeletionScheduledMessage(acc.Email, acc.Name, deletionWhen(acc.DeleteAfter, false), s.opts.BaseURL+"/login"))
	return acc, nil
}

// RestoreMyAccount cancels the owner's own deletion request.
func (s *Service) RestoreMyAccount(ctx context.Context, p Principal, c Caller) (store.Account, error) {
	acc, err := s.restoreAccount(ctx, p.Account)
	if err != nil {
		return store.Account{}, err
	}
	s.audit(ctx, acc.ID, "account_restored", nil, c)
	return acc, nil
}

func (s *Service) restoreAccount(ctx context.Context, acc store.Account) (store.Account, error) {
	if acc.DeleteAfter == nil {
		return acc, nil
	}
	if !acc.DeleteAfter.After(s.now().Add(5 * time.Second)) {
		return store.Account{}, httpx.NewError(409, CodeAccountDeletionScheduled, "this account is being removed right now; it cannot be restored")
	}
	out, err := s.q.CancelAccountDeletion(ctx, acc.ID)
	if err != nil {
		return store.Account{}, fmt.Errorf("console: restore account: %w", err)
	}
	if err := s.syncAccountProjects(ctx, out); err != nil {
		return store.Account{}, err
	}
	return out, nil
}

func deletionWhen(at *time.Time, now bool) string {
	if now || at == nil {
		return "within a few minutes"
	}
	return "on " + at.UTC().Format("2 January 2006, 15:04 UTC")
}
