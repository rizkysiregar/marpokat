// Package console is the account side of the Marpokat website (PLAN M9): people sign up, verify their e-mail, sign in
// and manage the projects (tenants) they own. The website itself holds no data; everything is here.
package console

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/mail"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/rizkysiregar/marpokat/server/internal/platform/httpx"
	"github.com/rizkysiregar/marpokat/server/internal/store"
	"github.com/rizkysiregar/marpokat/server/internal/tenant"
)

const (
	sessionPrefix = "mpc_" //nolint:gosec // a prefix, not a credential

	verifyTokenTTL  = 24 * time.Hour
	resetTokenTTL   = time.Hour
	sessionIdle     = 12 * time.Hour
	sessionAbsolute = 30 * 24 * time.Hour
	touchEvery      = time.Minute

	minPassword = 10
	maxPassword = 128
)

// Codes of this API on top of the ones in httpx.
//
//nolint:gosec // error codes, not credentials
const (
	CodeInvalidCredentials = "invalid_credentials"
	CodeEmailNotVerified   = "email_not_verified"
	CodeProjectLimit       = "project_limit_reached"
	CodeWrongPassword      = "wrong_password"
	CodeAccountDisabled    = "account_disabled"
	CodeRegistrationClosed = "registration_closed"
	CodeDeletionScheduled  = "deletion_scheduled"
)

// Limiter is ratelimit.Limiter.
type Limiter interface {
	Allow(ctx context.Context, capacity int, perSecond float64, keyParts ...string) (bool, time.Duration)
}

type Options struct {
	// BaseURL is where the website lives; links in e-mails point there.
	BaseURL string
	// MaxProjects is how many projects an account may have.
	MaxProjects int
	// Captcha checks sign up and password reset requests (PLAN D72). Nil: no check.
	Captcha Captcha
	// RegistrationClosed refuses new accounts (a switch for when there is abuse). Existing accounts sign in as usual.
	RegistrationClosed bool
	// DeletionGrace is how long a deleted project can still be restored before the worker removes it (PLAN D75).
	DeletionGrace time.Duration
}

type Service struct {
	q       store.Querier
	tenants *tenant.Service
	mailer  Mailer
	limiter Limiter
	log     *slog.Logger
	opts    Options
	now     func() time.Time
}

func NewService(q store.Querier, tenants *tenant.Service, mailer Mailer, limiter Limiter, log *slog.Logger, opts Options) *Service {
	if opts.MaxProjects <= 0 {
		opts.MaxProjects = 3
	}
	opts.BaseURL = strings.TrimRight(opts.BaseURL, "/")
	if opts.DeletionGrace <= 0 {
		opts.DeletionGrace = 72 * time.Hour
	}
	return &Service{q: q, tenants: tenants, mailer: mailer, limiter: limiter, log: log.With("subsystem", "console"), opts: opts, now: time.Now}
}

// Caller is who a request comes from: the visitor's address and browser.
type Caller struct {
	IP        string
	UserAgent string
}

// Scope is where a session was signed in (PLAN D71). Only an admin session can use the platform admin endpoints.
type Scope string

const (
	ScopePortal Scope = "portal"
	ScopeAdmin  Scope = "admin"
)

// Principal is a signed-in account and the session it signed in with.
type Principal struct {
	Account   store.Account
	SessionID uuid.UUID
	Scope     Scope
}

// ---------------------------------------------------------------- limits and helpers

func (s *Service) within(ctx context.Context, capacity int, perSecond float64, parts ...string) error {
	if s.limiter == nil {
		return nil
	}
	ok, wait := s.limiter.Allow(ctx, capacity, perSecond, append([]string{"console"}, parts...)...)
	if ok {
		return nil
	}
	return httpx.ErrRateLimited(wait, "too many attempts, try again later")
}

func (s *Service) audit(ctx context.Context, accountID uuid.UUID, action string, target *string, c Caller) {
	if err := s.q.InsertAudit(ctx, store.InsertAuditParams{AccountID: accountID, Action: action, Target: target, Ip: c.IP}); err != nil {
		s.log.Warn("audit event not stored", "action", action, "error", err)
	}
}

func strp(s string) *string { return &s }

// human runs the robot check, when there is one.
func (s *Service) human(ctx context.Context, token string, c Caller) error {
	if s.opts.Captcha == nil {
		return nil
	}
	return s.opts.Captcha.Verify(ctx, token, c.IP)
}

// sendMail does not hold up the request: a slow mail server must not make sign up slow. A failure is logged; the
// person can ask for the e-mail again.
func (s *Service) sendMail(m Message) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := s.mailer.Send(ctx, m); err != nil {
			s.log.Error("e-mail not sent", "subject", m.Subject, "error", err)
		}
	}()
}

func randomToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("console: crypto/rand failed: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func hashToken(token string) []byte {
	h := sha256.Sum256([]byte(token))
	return h[:]
}

func normalizeEmail(raw string) (string, error) {
	email := strings.ToLower(strings.TrimSpace(raw))
	addr, err := mail.ParseAddress(email)
	if err != nil || addr.Address != email || len(email) > 254 || strings.ContainsAny(email, "\r\n") || !strings.Contains(email[strings.LastIndex(email, "@"):], ".") {
		return "", httpx.FieldErrors{"email": "must be a valid e-mail address"}
	}
	return email, nil
}

func validatePassword(password, email string) error {
	n := utf8.RuneCountInString(password)
	switch {
	case n < minPassword:
		return httpx.FieldErrors{"password": fmt.Sprintf("must be at least %d characters", minPassword)}
	case n > maxPassword:
		return httpx.FieldErrors{"password": fmt.Sprintf("must be at most %d characters", maxPassword)}
	case strings.EqualFold(password, email):
		return httpx.FieldErrors{"password": "must not be your e-mail address"}
	case strings.ContainsRune(password, 0):
		return httpx.FieldErrors{"password": "contains a character that is not allowed"}
	}
	return nil
}

func validateName(raw string) (string, error) {
	name := strings.TrimSpace(raw)
	if name == "" || utf8.RuneCountInString(name) > 100 || strings.ContainsAny(name, "\r\n\x00") {
		return "", httpx.FieldErrors{"name": "must be 1 to 100 characters"}
	}
	return name, nil
}

func (s *Service) link(path string, token string) string {
	return s.opts.BaseURL + path + "?token=" + url.QueryEscape(token)
}

// ---------------------------------------------------------------- sign up and e-mail

// Register answers the same for a new and a known address, so that nobody can find out who has an account. The robot
// check comes after the cheap checks, so that a typo does not cost the visitor another check, and after the limit, so
// that a flood is not passed on to Cloudflare.
func (s *Service) Register(ctx context.Context, rawEmail, password, rawName, captcha string, c Caller) error {
	if s.opts.RegistrationClosed {
		return httpx.NewError(403, CodeRegistrationClosed, "sign up is closed for now")
	}
	email, err := normalizeEmail(rawEmail)
	if err != nil {
		return err
	}
	name, err := validateName(rawName)
	if err != nil {
		return err
	}
	if err := validatePassword(password, email); err != nil {
		return err
	}
	if err := s.within(ctx, 5, 5.0/3600, "register-ip", c.IP); err != nil {
		return err
	}
	if err := s.human(ctx, captcha, c); err != nil {
		return err
	}
	hash, err := hashPassword(password)
	if err != nil {
		return err
	}
	acc, err := s.q.CreateAccount(ctx, store.CreateAccountParams{ID: uuid.Must(uuid.NewV7()), Email: email, Name: name, PasswordHash: hash})
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" { // known address: tell its owner, not the visitor
		s.sendMail(alreadyRegisteredMessage(email, s.opts.BaseURL+"/login", s.opts.BaseURL+"/forgot-password"))
		return nil
	}
	if err != nil {
		return fmt.Errorf("console: create account: %w", err)
	}
	if err := s.sendVerification(ctx, acc); err != nil {
		return err
	}
	s.audit(ctx, acc.ID, "registered", nil, c)
	return nil
}

func (s *Service) sendVerification(ctx context.Context, acc store.Account) error {
	token := randomToken()
	if err := s.q.DeleteAccountTokens(ctx, store.DeleteAccountTokensParams{AccountID: acc.ID, Kind: "verify_email"}); err != nil {
		return fmt.Errorf("console: clear tokens: %w", err)
	}
	if err := s.q.InsertAccountToken(ctx, store.InsertAccountTokenParams{
		AccountID: acc.ID, Kind: "verify_email", TokenHash: hashToken(token), ExpiresAt: s.now().Add(verifyTokenTTL),
	}); err != nil {
		return fmt.Errorf("console: store token: %w", err)
	}
	s.sendMail(verifyEmailMessage(acc.Email, acc.Name, s.link("/verify-email", token)))
	return nil
}

func (s *Service) VerifyEmail(ctx context.Context, token string) error {
	id, err := s.q.ConsumeAccountToken(ctx, store.ConsumeAccountTokenParams{TokenHash: hashToken(token), Kind: "verify_email"})
	if errors.Is(err, pgx.ErrNoRows) {
		return httpx.NewError(400, httpx.CodeBadRequest, "this link is not valid or has expired")
	}
	if err != nil {
		return fmt.Errorf("console: verify: %w", err)
	}
	if err := s.q.SetAccountEmailVerified(ctx, id); err != nil {
		return fmt.Errorf("console: verify: %w", err)
	}
	s.audit(ctx, id, "email_verified", nil, Caller{})
	return nil
}

func (s *Service) ResendVerification(ctx context.Context, p Principal) error {
	if p.Account.EmailVerifiedAt != nil {
		return nil
	}
	if err := s.within(ctx, 3, 3.0/3600, "resend", p.Account.ID.String()); err != nil {
		return err
	}
	return s.sendVerification(ctx, p.Account)
}

// ---------------------------------------------------------------- sign in and sessions

type SessionResult struct {
	Token     string
	ExpiresAt time.Time
	// AbsoluteExpiresAt is the latest the session can live: the cookie is kept that long, the server ends it sooner
	// when it is idle.
	AbsoluteExpiresAt time.Time
	Account           store.Account
}

// Login starts a session. A sign in on the admin app (scope admin) is only for platform administrators.
func (s *Service) Login(ctx context.Context, rawEmail, password string, scope Scope, c Caller) (SessionResult, error) {
	if scope != ScopeAdmin {
		scope = ScopePortal
	}
	email := strings.ToLower(strings.TrimSpace(rawEmail))
	if email == "" || len(email) > 254 || password == "" || len(password) > 1024 {
		return SessionResult{}, httpx.NewError(401, CodeInvalidCredentials, "wrong e-mail or password")
	}
	// 5 tries per e-mail, then one more every 3 minutes; and a wider limit per address for someone trying many e-mails
	if err := s.within(ctx, 5, 1.0/180, "login-email", email); err != nil {
		return SessionResult{}, err
	}
	if err := s.within(ctx, 30, 30.0/900, "login-ip", c.IP); err != nil {
		return SessionResult{}, err
	}
	acc, err := s.q.GetAccountByEmail(ctx, email)
	if errors.Is(err, pgx.ErrNoRows) {
		verifyPassword(password, dummyHash) // an unknown e-mail takes as long as a wrong password
		return SessionResult{}, httpx.NewError(401, CodeInvalidCredentials, "wrong e-mail or password")
	}
	if err != nil {
		return SessionResult{}, fmt.Errorf("console: login: %w", err)
	}
	if !verifyPassword(password, acc.PasswordHash) {
		s.audit(ctx, acc.ID, "login_failed", nil, c)
		return SessionResult{}, httpx.NewError(401, CodeInvalidCredentials, "wrong e-mail or password")
	}
	if acc.DisabledAt != nil {
		msg := "this account is suspended"
		if acc.SuspendedReason != nil {
			msg += ": " + *acc.SuspendedReason
		}
		return SessionResult{}, httpx.NewError(403, CodeAccountDisabled, "%s", msg)
	}
	if scope == ScopeAdmin && !acc.IsPlatformAdmin {
		s.audit(ctx, acc.ID, "admin_login_refused", nil, c)
		return SessionResult{}, errAdminOnly
	}
	token := sessionPrefix + randomToken()
	now := s.now()
	expires := now.Add(sessionIdle)
	if err := s.q.InsertSession(ctx, store.InsertSessionParams{
		ID: uuid.Must(uuid.NewV7()), AccountID: acc.ID, TokenHash: hashToken(token), ExpiresAt: expires,
		AbsoluteExpiresAt: now.Add(sessionAbsolute), UserAgent: truncate(c.UserAgent, 300), Ip: c.IP, Scope: string(scope),
	}); err != nil {
		return SessionResult{}, fmt.Errorf("console: start session: %w", err)
	}
	action := "login"
	if scope == ScopeAdmin {
		action = "admin_login"
	}
	s.audit(ctx, acc.ID, action, nil, c)
	return SessionResult{Token: token, ExpiresAt: expires, AbsoluteExpiresAt: now.Add(sessionAbsolute), Account: acc}, nil
}

func truncate(s string, n int) string {
	if len(s) > n {
		return strings.ToValidUTF8(s[:n], "")
	}
	return s
}

var errUnauthenticated = httpx.NewError(401, httpx.CodeUnauthenticated, "sign in first")

// Authenticate resolves a session token. Using a session pushes its idle expiry forward, at most once a minute.
func (s *Service) Authenticate(ctx context.Context, token string) (Principal, error) {
	if !strings.HasPrefix(token, sessionPrefix) {
		return Principal{}, errUnauthenticated
	}
	sess, err := s.q.GetSessionByHash(ctx, hashToken(token))
	if errors.Is(err, pgx.ErrNoRows) {
		return Principal{}, errUnauthenticated
	}
	if err != nil {
		return Principal{}, fmt.Errorf("console: session: %w", err)
	}
	acc, err := s.q.GetAccountByID(ctx, sess.AccountID)
	if err != nil || acc.DisabledAt != nil {
		return Principal{}, errUnauthenticated
	}
	if now := s.now(); now.Sub(sess.LastSeenAt) > touchEvery {
		next := now.Add(sessionIdle)
		if next.After(sess.AbsoluteExpiresAt) {
			next = sess.AbsoluteExpiresAt
		}
		if err := s.q.TouchSession(ctx, store.TouchSessionParams{ID: sess.ID, ExpiresAt: next}); err != nil {
			s.log.Warn("session not extended", "error", err)
		}
	}
	return Principal{Account: acc, SessionID: sess.ID, Scope: Scope(sess.Scope)}, nil
}

func (s *Service) Logout(ctx context.Context, p Principal, c Caller) error {
	if err := s.q.DeleteSession(ctx, p.SessionID); err != nil {
		return fmt.Errorf("console: logout: %w", err)
	}
	s.audit(ctx, p.Account.ID, "logout", nil, c)
	return nil
}

func (s *Service) Sessions(ctx context.Context, p Principal) ([]store.ConsoleSession, error) {
	rows, err := s.q.ListAccountSessions(ctx, p.Account.ID)
	if err != nil {
		return nil, fmt.Errorf("console: sessions: %w", err)
	}
	return rows, nil
}

func (s *Service) Audit(ctx context.Context, p Principal, limit int) ([]store.ConsoleAudit, error) {
	rows, err := s.q.ListAudit(ctx, store.ListAuditParams{AccountID: p.Account.ID, Limit: clampRows(limit)})
	if err != nil {
		return nil, fmt.Errorf("console: audit: %w", err)
	}
	return rows, nil
}

// ---------------------------------------------------------------- password

// ForgotPassword always succeeds; a known address gets the link. Asking again replaces the earlier link.
func (s *Service) ForgotPassword(ctx context.Context, rawEmail, captcha string, c Caller) error {
	email := strings.ToLower(strings.TrimSpace(rawEmail))
	if email == "" || len(email) > 254 {
		return nil
	}
	if err := s.within(ctx, 5, 5.0/3600, "forgot-ip", c.IP); err != nil {
		return err
	}
	if err := s.within(ctx, 3, 3.0/3600, "forgot-email", email); err != nil {
		return err
	}
	if err := s.human(ctx, captcha, c); err != nil {
		return err
	}
	acc, err := s.q.GetAccountByEmail(ctx, email)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("console: forgot password: %w", err)
	}
	if acc.DisabledAt != nil {
		return nil
	}
	token := randomToken()
	if err := s.q.DeleteAccountTokens(ctx, store.DeleteAccountTokensParams{AccountID: acc.ID, Kind: "reset_password"}); err != nil {
		return fmt.Errorf("console: clear tokens: %w", err)
	}
	if err := s.q.InsertAccountToken(ctx, store.InsertAccountTokenParams{
		AccountID: acc.ID, Kind: "reset_password", TokenHash: hashToken(token), ExpiresAt: s.now().Add(resetTokenTTL),
	}); err != nil {
		return fmt.Errorf("console: store token: %w", err)
	}
	s.sendMail(resetPasswordMessage(acc.Email, acc.Name, s.link("/reset-password", token)))
	s.audit(ctx, acc.ID, "password_reset_requested", nil, c)
	return nil
}

func (s *Service) ResetPassword(ctx context.Context, token, password string, c Caller) error {
	// the new password is judged first, so that a typo does not spend the single use link
	hash, err := s.preparePassword(password, "")
	if err != nil {
		return err
	}
	id, err := s.q.ConsumeAccountToken(ctx, store.ConsumeAccountTokenParams{TokenHash: hashToken(token), Kind: "reset_password"})
	if errors.Is(err, pgx.ErrNoRows) {
		return httpx.NewError(400, httpx.CodeBadRequest, "this link is not valid or has expired")
	}
	if err != nil {
		return fmt.Errorf("console: reset: %w", err)
	}
	acc, err := s.q.GetAccountByID(ctx, id)
	if err != nil {
		return fmt.Errorf("console: reset: %w", err)
	}
	if strings.EqualFold(password, acc.Email) {
		return httpx.FieldErrors{"password": "must not be your e-mail address"}
	}
	if err := s.q.UpdateAccountPassword(ctx, store.UpdateAccountPasswordParams{ID: id, PasswordHash: hash}); err != nil {
		return fmt.Errorf("console: reset: %w", err)
	}
	// a link in an inbox proves the e-mail too, and whoever had the old password is signed out
	_ = s.q.SetAccountEmailVerified(ctx, id)
	_ = s.q.DeleteAccountSessions(ctx, id)
	s.audit(ctx, id, "password_reset", nil, c)
	s.sendMail(passwordChangedMessage(acc.Email, acc.Name))
	return nil
}

func (s *Service) preparePassword(password, email string) (string, error) {
	if err := validatePassword(password, email); err != nil {
		return "", err
	}
	return hashPassword(password)
}

func (s *Service) ChangePassword(ctx context.Context, p Principal, current, next string, c Caller) error {
	if err := s.confirm(ctx, p, current, c); err != nil {
		return err
	}
	hash, err := s.preparePassword(next, p.Account.Email)
	if err != nil {
		return err
	}
	if err := s.q.UpdateAccountPassword(ctx, store.UpdateAccountPasswordParams{ID: p.Account.ID, PasswordHash: hash}); err != nil {
		return fmt.Errorf("console: change password: %w", err)
	}
	_ = s.q.DeleteOtherSessions(ctx, store.DeleteOtherSessionsParams{AccountID: p.Account.ID, ID: p.SessionID})
	s.audit(ctx, p.Account.ID, "password_changed", nil, c)
	s.sendMail(passwordChangedMessage(p.Account.Email, p.Account.Name))
	return nil
}

// confirm asks the account password again before something that cannot be undone. Every try counts, right or wrong
// (the limiter cannot tell before it is asked): 10 in a row, then one a minute. A person managing a project never
// gets near that; someone with a stolen session guessing the password is held to about 1400 guesses a day.
func (s *Service) confirm(ctx context.Context, p Principal, password string, c Caller) error {
	if err := s.within(ctx, 10, 1.0/60, "confirm", p.Account.ID.String()); err != nil {
		return err
	}
	if !verifyPassword(password, p.Account.PasswordHash) {
		s.audit(ctx, p.Account.ID, "password_confirmation_failed", nil, c)
		return httpx.NewError(403, CodeWrongPassword, "the password is not right")
	}
	return nil
}

func (s *Service) UpdateName(ctx context.Context, p Principal, raw string) (store.Account, error) {
	name, err := validateName(raw)
	if err != nil {
		return store.Account{}, err
	}
	acc, err := s.q.UpdateAccountName(ctx, store.UpdateAccountNameParams{ID: p.Account.ID, Name: name})
	if err != nil {
		return store.Account{}, fmt.Errorf("console: update name: %w", err)
	}
	return acc, nil
}

// ---------------------------------------------------------------- projects

var (
	slugStrip   = regexp.MustCompile(`[^a-z0-9]+`)
	slugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,62}$`)
)

func slugify(name string) string {
	s := strings.Trim(slugStrip.ReplaceAllString(strings.ToLower(name), "-"), "-")
	if len(s) > 50 {
		s = strings.Trim(s[:50], "-")
	}
	if len(s) < 2 {
		s = "project"
	}
	return s
}

type ProjectWithSecret struct {
	Project store.Tenant
	Secret  string
}

func (s *Service) CreateProject(ctx context.Context, p Principal, rawName, rawSlug, rawEnv string, c Caller) (ProjectWithSecret, error) {
	name, err := validateName(rawName)
	if err != nil {
		return ProjectWithSecret{}, err
	}
	env, err := environmentOf(rawEnv)
	if err != nil {
		return ProjectWithSecret{}, err
	}
	explicit := strings.TrimSpace(rawSlug) != ""
	slug := strings.TrimSpace(rawSlug)
	if explicit && !slugPattern.MatchString(slug) {
		return ProjectWithSecret{}, httpx.FieldErrors{"slug": "must be 2 to 63 characters of a-z, 0-9 and '-', starting with a letter or digit"}
	}
	if p.Account.EmailVerifiedAt == nil {
		return ProjectWithSecret{}, httpx.NewError(403, CodeEmailNotVerified, "confirm your e-mail address first")
	}
	if p.Account.DeleteAfter != nil {
		return ProjectWithSecret{}, errAccountDeleting
	}
	if err := s.within(ctx, 5, 5.0/3600, "create-project", p.Account.ID.String()); err != nil {
		return ProjectWithSecret{}, err
	}
	n, err := s.q.CountTenantsOfAccount(ctx, uuid.NullUUID{UUID: p.Account.ID, Valid: true})
	if err != nil {
		return ProjectWithSecret{}, fmt.Errorf("console: count projects: %w", err)
	}
	if int(n) >= s.opts.MaxProjects {
		return ProjectWithSecret{}, httpx.NewError(403, CodeProjectLimit, "an account can have %d projects; ask us if you need more", s.opts.MaxProjects)
	}
	if !explicit {
		slug = slugify(name)
	}
	owner := uuid.NullUUID{UUID: p.Account.ID, Valid: true}
	for attempt := 0; ; attempt++ {
		_, secret, err := s.tenants.CreateOwned(ctx, slug, name, tenant.Config{}, owner)
		if err == nil {
			row, err := s.tenants.GetBySlug(ctx, slug)
			if err != nil {
				return ProjectWithSecret{}, err
			}
			full, err := s.q.GetTenantByID(ctx, row.ID)
			if err != nil {
				return ProjectWithSecret{}, fmt.Errorf("console: load project: %w", err)
			}
			if env != nil {
				if full, err = s.q.SetTenantEnvironment(ctx, store.SetTenantEnvironmentParams{ID: row.ID, Environment: env}); err != nil {
					return ProjectWithSecret{}, fmt.Errorf("console: set label: %w", err)
				}
			}
			s.audit(ctx, p.Account.ID, "project_created", strp(slug), c)
			return ProjectWithSecret{Project: full, Secret: secret}, nil
		}
		if !errors.Is(err, tenant.ErrSlugTaken) {
			return ProjectWithSecret{}, err
		}
		if explicit {
			return ProjectWithSecret{}, httpx.ErrConflict("the slug %q is taken", slug)
		}
		if attempt >= 5 {
			return ProjectWithSecret{}, fmt.Errorf("console: no free slug for %q", name)
		}
		slug = slugify(name) + "-" + randomSuffix()
	}
}

func randomSuffix() string {
	b := make([]byte, 2)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (s *Service) Projects(ctx context.Context, p Principal) ([]store.Tenant, error) {
	rows, err := s.q.ListTenantsOfAccount(ctx, uuid.NullUUID{UUID: p.Account.ID, Valid: true})
	if err != nil {
		return nil, fmt.Errorf("console: projects: %w", err)
	}
	return rows, nil
}

var errProjectNotFound = httpx.ErrNotFound("project not found")

// project loads a project of this account; one of someone else's is "not found", the same as one that does not exist.
func (s *Service) project(ctx context.Context, p Principal, id uuid.UUID) (store.Tenant, error) {
	row, err := s.q.GetTenantOfAccount(ctx, store.GetTenantOfAccountParams{ID: id, AccountID: uuid.NullUUID{UUID: p.Account.ID, Valid: true}})
	if errors.Is(err, pgx.ErrNoRows) {
		return store.Tenant{}, errProjectNotFound
	}
	if err != nil {
		return store.Tenant{}, fmt.Errorf("console: project: %w", err)
	}
	return row, nil
}

type ProjectDetail struct {
	Project  store.Tenant
	Users    int64
	Channels int64
	Messages int64
}

func (s *Service) Project(ctx context.Context, p Principal, id uuid.UUID) (ProjectDetail, error) {
	row, err := s.project(ctx, p, id)
	if err != nil {
		return ProjectDetail{}, err
	}
	u, err := s.q.TenantUsage(ctx, row.ID)
	if err != nil {
		return ProjectDetail{}, fmt.Errorf("console: usage: %w", err)
	}
	return ProjectDetail{Project: row, Users: u.Users, Channels: u.Channels, Messages: u.Messages}, nil
}

type ProjectUpdate struct {
	Name                 *string
	ClientCanCreateDM    *bool
	ClientCanCreateGroup *bool
	// Environment: nil leaves it, "" clears it, otherwise development, staging or production (PLAN D78).
	Environment *string
}

func (s *Service) UpdateProject(ctx context.Context, p Principal, id uuid.UUID, in ProjectUpdate, c Caller) (store.Tenant, error) {
	row, err := s.project(ctx, p, id)
	if err != nil {
		return store.Tenant{}, err
	}
	if in.Name != nil {
		name, err := validateName(*in.Name)
		if err != nil {
			return store.Tenant{}, err
		}
		if err := s.tenants.SetName(ctx, row.Slug, name); err != nil {
			return store.Tenant{}, err
		}
	}
	if in.ClientCanCreateDM != nil || in.ClientCanCreateGroup != nil {
		t, err := s.tenants.GetBySlug(ctx, row.Slug)
		if err != nil {
			return store.Tenant{}, err
		}
		cfg := t.Config
		if in.ClientCanCreateDM != nil {
			cfg.ClientCanCreateDM = *in.ClientCanCreateDM
		}
		if in.ClientCanCreateGroup != nil {
			cfg.ClientCanCreateGroup = *in.ClientCanCreateGroup
		}
		if err := s.tenants.SetConfig(ctx, row.Slug, cfg); err != nil {
			return store.Tenant{}, err
		}
	}
	if in.Environment != nil {
		env, err := environmentOf(*in.Environment)
		if err != nil {
			return store.Tenant{}, err
		}
		if _, err := s.q.SetTenantEnvironment(ctx, store.SetTenantEnvironmentParams{ID: row.ID, Environment: env}); err != nil {
			return store.Tenant{}, fmt.Errorf("console: set label: %w", err)
		}
	}
	s.audit(ctx, p.Account.ID, "project_updated", strp(row.Slug), c)
	return s.q.GetTenantByID(ctx, row.ID)
}

func (s *Service) RotateSecret(ctx context.Context, p Principal, id uuid.UUID, password string, c Caller) (string, error) {
	row, err := s.project(ctx, p, id)
	if err != nil {
		return "", err
	}
	if err := s.confirm(ctx, p, password, c); err != nil {
		return "", err
	}
	secret, err := s.tenants.RotateSecret(ctx, row.Slug)
	if err != nil {
		return "", err
	}
	s.audit(ctx, p.Account.ID, "secret_rotated", strp(row.Slug), c)
	return secret, nil
}

type PushStatus struct {
	Configured        bool
	FirebaseProjectID string
	ClientEmail       string
}

func (s *Service) Push(ctx context.Context, p Principal, id uuid.UUID) (PushStatus, error) {
	row, err := s.project(ctx, p, id)
	if err != nil {
		return PushStatus{}, err
	}
	return s.pushStatus(ctx, row)
}

func (s *Service) pushStatus(ctx context.Context, row store.Tenant) (PushStatus, error) {
	st := PushStatus{Configured: len(row.FcmCredentialsEnc) > 0}
	if !st.Configured {
		return st, nil
	}
	raw, ok, err := s.tenants.FCMCredentials(ctx, row.ID)
	if err != nil || !ok {
		return st, nil // configured, but its details cannot be shown (disabled project)
	}
	st.FirebaseProjectID, st.ClientEmail = serviceAccountInfo(raw)
	return st, nil
}

func (s *Service) SetPush(ctx context.Context, p Principal, id uuid.UUID, serviceAccount, password string, c Caller) (PushStatus, error) {
	row, err := s.project(ctx, p, id)
	if err != nil {
		return PushStatus{}, err
	}
	if len(serviceAccount) > 16<<10 {
		return PushStatus{}, httpx.FieldErrors{"service_account": "is larger than 16 KB, it is not a service account key"}
	}
	if err := s.confirm(ctx, p, password, c); err != nil {
		return PushStatus{}, err
	}
	if err := s.tenants.SetFCMCredentials(ctx, row.Slug, []byte(serviceAccount)); err != nil {
		return PushStatus{}, httpx.FieldErrors{"service_account": strings.TrimPrefix(err.Error(), "tenant: ")}
	}
	s.audit(ctx, p.Account.ID, "push_set", strp(row.Slug), c)
	fresh, err := s.q.GetTenantByID(ctx, row.ID)
	if err != nil {
		return PushStatus{}, err
	}
	return s.pushStatus(ctx, fresh)
}

func (s *Service) RemovePush(ctx context.Context, p Principal, id uuid.UUID, password string, c Caller) error {
	row, err := s.project(ctx, p, id)
	if err != nil {
		return err
	}
	if err := s.confirm(ctx, p, password, c); err != nil {
		return err
	}
	if err := s.tenants.ClearFCMCredentials(ctx, row.Slug); err != nil {
		return err
	}
	s.audit(ctx, p.Account.ID, "push_removed", strp(row.Slug), c)
	return nil
}

func (s *Service) SetProjectDisabled(ctx context.Context, p Principal, id uuid.UUID, disabled bool, password string, c Caller) (store.Tenant, error) {
	row, err := s.project(ctx, p, id)
	if err != nil {
		return store.Tenant{}, err
	}
	if !disabled && row.DeleteAfter != nil {
		return store.Tenant{}, errDeletionScheduled
	}
	if !disabled && row.SuspendedAt != nil {
		return store.Tenant{}, errProjectSuspended
	}
	if err := s.confirm(ctx, p, password, c); err != nil {
		return store.Tenant{}, err
	}
	if err := s.tenants.SetDisabled(ctx, row.Slug, disabled); err != nil {
		return store.Tenant{}, err
	}
	action := "project_enabled"
	if disabled {
		action = "project_disabled"
	}
	s.audit(ctx, p.Account.ID, action, strp(row.Slug), c)
	return s.q.GetTenantByID(ctx, row.ID)
}

// ---------------------------------------------------------------- platform admin

var (
	errForbidden    = httpx.ErrForbidden("this is for platform administrators")
	errAdminOnly    = httpx.ErrForbidden("only platform administrators can sign in here")
	errAdminSession = httpx.ErrForbidden("sign in on the admin site to do this")
)

// requireAdmin lets through a platform administrator who signed in on the admin app (PLAN D71).
func requireAdmin(p Principal) error {
	if !p.Account.IsPlatformAdmin {
		return errForbidden
	}
	if p.Scope != ScopeAdmin {
		return errAdminSession
	}
	return nil
}

// AdminAccounts pages the accounts in id order; search, when given, keeps those whose e-mail or name contains it.
func (s *Service) AdminAccounts(ctx context.Context, p Principal, search string, after uuid.UUID, limit int) ([]store.Account, error) {
	if err := requireAdmin(p); err != nil {
		return nil, err
	}
	rows, err := s.q.ListAccounts(ctx, store.ListAccountsParams{After: after, Pattern: likePattern(search), MaxRows: clampRows(limit)})
	if err != nil {
		return nil, fmt.Errorf("console: accounts: %w", err)
	}
	return rows, nil
}

// AdminSetAccountDisabled is the old switch of the admin app (deprecated): it suspends or lifts, like the new actions.
func (s *Service) AdminSetAccountDisabled(ctx context.Context, p Principal, id uuid.UUID, disabled bool, c Caller) (store.Account, error) {
	if disabled {
		return s.AdminSuspendAccount(ctx, p, id, "Suspended by an administrator.", c)
	}
	return s.AdminUnsuspendAccount(ctx, p, id, c)
}

type AdminProject struct {
	Project    store.Tenant
	OwnerEmail *string
}

func (s *Service) AdminProjects(ctx context.Context, p Principal) ([]AdminProject, error) {
	if err := requireAdmin(p); err != nil {
		return nil, err
	}
	rows, err := s.q.ListAllTenantsWithOwner(ctx)
	if err != nil {
		return nil, fmt.Errorf("console: projects: %w", err)
	}
	out := make([]AdminProject, len(rows))
	for i, r := range rows {
		out[i] = AdminProject{Project: r.Tenant, OwnerEmail: r.OwnerEmail}
	}
	return out, nil
}

func (s *Service) AdminSetProjectDisabled(ctx context.Context, p Principal, id uuid.UUID, disabled bool, c Caller) (AdminProject, error) {
	if err := requireAdmin(p); err != nil {
		return AdminProject{}, err
	}
	row, err := s.q.GetTenantWithOwner(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return AdminProject{}, errProjectNotFound
	}
	if err != nil {
		return AdminProject{}, fmt.Errorf("console: project: %w", err)
	}
	if !disabled && row.Tenant.DeleteAfter != nil {
		return AdminProject{}, errDeletionScheduled
	}
	if err := s.tenants.SetDisabled(ctx, row.Tenant.Slug, disabled); err != nil {
		return AdminProject{}, err
	}
	action := "project_enabled_by_admin"
	if disabled {
		action = "project_disabled_by_admin"
	}
	s.audit(ctx, p.Account.ID, action, strp(row.Tenant.Slug), c)
	fresh, err := s.q.GetTenantWithOwner(ctx, id)
	if err != nil {
		return AdminProject{}, err
	}
	return AdminProject{Project: fresh.Tenant, OwnerEmail: fresh.OwnerEmail}, nil
}

func (s *Service) AdminStats(ctx context.Context, p Principal) (store.PlatformUsageRow, error) {
	if err := requireAdmin(p); err != nil {
		return store.PlatformUsageRow{}, err
	}
	u, err := s.q.PlatformUsage(ctx)
	if err != nil {
		return store.PlatformUsageRow{}, fmt.Errorf("console: stats: %w", err)
	}
	return u, nil
}

// clampRows keeps a page size inside what a query accepts; the handlers already cap it at 100.
func clampRows(n int) int32 {
	if n < 1 {
		return 1
	}
	if n > 1000 {
		return 1000
	}
	return int32(n) //nolint:gosec // between 1 and 1000
}

// CleanSessions removes expired sessions every interval until ctx ends.
func (s *Service) CleanSessions(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if n, err := s.q.DeleteExpiredSessions(ctx); err != nil {
				s.log.Warn("expired sessions not removed", "error", err)
			} else if n > 0 {
				s.log.Info("expired sessions removed", "count", n)
			}
		}
	}
}

// PromoteAdmin makes the account with this e-mail a platform administrator (CLI only: there is no API for it).
// PromoteAdmin makes an account a platform administrator, or takes it away (the CLI, PLAN D66). It is written in the
// account's security log; a session open in the admin app loses its rights at its next request.
func (s *Service) PromoteAdmin(ctx context.Context, email string, admin bool) error {
	id, err := s.q.SetAccountPlatformAdmin(ctx, store.SetAccountPlatformAdminParams{Admin: admin, Email: email})
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("console: no account with e-mail %s", email)
	}
	if err != nil {
		return fmt.Errorf("console: promote: %w", err)
	}
	action := "demoted_from_admin"
	if admin {
		action = "promoted_to_admin"
	}
	s.audit(ctx, id, action, nil, Caller{IP: "cli"})
	return nil
}

// serviceAccountInfo reads which Firebase project and account a stored key belongs to, for display. The key itself
// never leaves.
func serviceAccountInfo(raw []byte) (projectID, clientEmail string) {
	var sa struct {
		ProjectID   string `json:"project_id"`
		ClientEmail string `json:"client_email"`
	}
	if err := json.Unmarshal(raw, &sa); err != nil {
		return "", ""
	}
	return sa.ProjectID, sa.ClientEmail
}
