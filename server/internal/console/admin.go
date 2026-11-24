package console

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/rizkysiregar/marpokat/server/internal/platform/httpx"
	"github.com/rizkysiregar/marpokat/server/internal/store"
)

var errAccountNotFound = httpx.ErrNotFound("account not found")

// AdminAccountView is one account as the platform admin sees it.
type AdminAccountView struct {
	Account  store.Account
	Projects []store.Tenant
	Sessions []store.ConsoleSession
	Audit    []store.ConsoleAudit
}

func (s *Service) AdminAccount(ctx context.Context, p Principal, id uuid.UUID) (AdminAccountView, error) {
	if err := requireAdmin(p); err != nil {
		return AdminAccountView{}, err
	}
	acc, err := s.q.GetAccountByID(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return AdminAccountView{}, errAccountNotFound
	}
	if err != nil {
		return AdminAccountView{}, fmt.Errorf("console: account: %w", err)
	}
	v := AdminAccountView{Account: acc}
	if v.Projects, err = s.q.ListTenantsOfAccount(ctx, uuid.NullUUID{UUID: id, Valid: true}); err != nil {
		return v, fmt.Errorf("console: projects: %w", err)
	}
	if v.Sessions, err = s.q.ListAccountSessions(ctx, id); err != nil {
		return v, fmt.Errorf("console: sessions: %w", err)
	}
	if v.Audit, err = s.q.ListAudit(ctx, store.ListAuditParams{AccountID: id, Limit: 50}); err != nil {
		return v, fmt.Errorf("console: audit: %w", err)
	}
	return v, nil
}

// AdminEndSessions signs an account out everywhere (a suspicious login, a leaked password) without disabling it.
func (s *Service) AdminEndSessions(ctx context.Context, p Principal, id uuid.UUID, c Caller) error {
	if err := requireAdmin(p); err != nil {
		return err
	}
	acc, err := s.q.GetAccountByID(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return errAccountNotFound
	}
	if err != nil {
		return fmt.Errorf("console: account: %w", err)
	}
	if acc.ID == p.Account.ID {
		return httpx.NewError(400, httpx.CodeBadRequest, "sign yourself out from your own account page")
	}
	if err := s.q.DeleteAccountSessions(ctx, id); err != nil {
		return fmt.Errorf("console: end sessions: %w", err)
	}
	s.audit(ctx, p.Account.ID, "account_signed_out_by_admin", strp(acc.Email), c)
	s.audit(ctx, acc.ID, "signed_out_by_admin", nil, c)
	return nil
}

// AdminProjectView is one project with its owner, usage and push status.
type AdminProjectView struct {
	Project    store.Tenant
	OwnerEmail *string
	Usage      store.TenantUsageRow
	Push       PushStatus
	// OwnerIsAdmin: the owner is another platform administrator, out of reach of the admin actions (demote first).
	OwnerIsAdmin bool
}

func (s *Service) AdminProject(ctx context.Context, p Principal, id uuid.UUID) (AdminProjectView, error) {
	if err := requireAdmin(p); err != nil {
		return AdminProjectView{}, err
	}
	row, err := s.q.GetTenantWithOwner(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return AdminProjectView{}, errProjectNotFound
	}
	if err != nil {
		return AdminProjectView{}, fmt.Errorf("console: project: %w", err)
	}
	v := AdminProjectView{Project: row.Tenant, OwnerEmail: row.OwnerEmail}
	if row.Tenant.AccountID.Valid {
		if owner, err := s.q.GetAccountByID(ctx, row.Tenant.AccountID.UUID); err == nil {
			v.OwnerIsAdmin = owner.IsPlatformAdmin && owner.ID != p.Account.ID
		}
	}
	if v.Usage, err = s.q.TenantUsage(ctx, id); err != nil {
		return v, fmt.Errorf("console: usage: %w", err)
	}
	if v.Push, err = s.pushStatus(ctx, row.Tenant); err != nil {
		return v, err
	}
	return v, nil
}

// AuditFilter narrows the platform audit log.
type AuditFilter struct {
	Action    string
	AccountID uuid.UUID // uuid.Nil: every account
	Before    uuid.UUID // uuid.Nil: from the newest
}

func (s *Service) AdminAudit(ctx context.Context, p Principal, f AuditFilter, limit int) ([]store.ListPlatformAuditRow, error) {
	if err := requireAdmin(p); err != nil {
		return nil, err
	}
	params := store.ListPlatformAuditParams{MaxRows: clampRows(limit)}
	if f.Action != "" {
		params.Action = &f.Action
	}
	if f.AccountID != uuid.Nil {
		params.AccountID = uuid.NullUUID{UUID: f.AccountID, Valid: true}
	}
	if f.Before != uuid.Nil {
		params.BeforeID = uuid.NullUUID{UUID: f.Before, Valid: true}
	}
	rows, err := s.q.ListPlatformAudit(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("console: platform audit: %w", err)
	}
	return rows, nil
}

// AdminSeries is what the admin overview charts: one row per day, and the busiest projects of the period.
type AdminSeries struct {
	Days        []store.PlatformDailySeriesRow
	TopProjects []store.TopTenantsByMessagesRow
}

// AdminSeriesDays are the periods the overview offers.
var AdminSeriesDays = map[int]bool{7: true, 30: true, 90: true}

func (s *Service) AdminSeries(ctx context.Context, p Principal, days int) (AdminSeries, error) {
	if err := requireAdmin(p); err != nil {
		return AdminSeries{}, err
	}
	if !AdminSeriesDays[days] {
		return AdminSeries{}, httpx.FieldErrors{"days": "must be 7, 30 or 90"}
	}
	rows, err := s.q.PlatformDailySeries(ctx, int32(days)) //nolint:gosec // 7, 30 or 90, checked above
	if err != nil {
		return AdminSeries{}, fmt.Errorf("console: series: %w", err)
	}
	since := s.now().UTC().Truncate(24*time.Hour).AddDate(0, 0, -(days - 1))
	top, err := s.q.TopTenantsByMessages(ctx, store.TopTenantsByMessagesParams{Since: since, MaxRows: 5})
	if err != nil {
		return AdminSeries{}, fmt.Errorf("console: top projects: %w", err)
	}
	return AdminSeries{Days: rows, TopProjects: top}, nil
}
