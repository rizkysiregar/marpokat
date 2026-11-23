package auth

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/rizkysiregar/marpokat/server/internal/platform/httpx"
	"github.com/rizkysiregar/marpokat/server/internal/tenant"
)

// ErrUnknownUser is returned by a UserLookup when the token's subject was never upserted (or was deleted).
var ErrUnknownUser = errors.New("unknown user")

// HeaderAPIKey carries the tenant's public api key on every request.
const HeaderAPIKey = "X-Api-Key" //nolint:gosec // G101 false positive: a header name, not a credential

// Principal is who is calling. Server principals have no user.
type Principal struct {
	Tenant     tenant.Tenant
	UserID     uuid.UUID
	ExternalID string
}

func (p Principal) IsServer() bool { return p.UserID == uuid.Nil }

type ctxKey struct{}

// FromContext returns the principal set by the middleware. Handlers behind User or Server can rely on it.
func FromContext(ctx context.Context) Principal {
	p, ok := ctx.Value(ctxKey{}).(Principal)
	if !ok {
		panic("auth: no principal in context; route is not behind auth middleware")
	}
	return p
}

// WithPrincipal is for tests of handlers that sit behind the middleware.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, ctxKey{}, p)
}

// TenantResolver is tenant.Resolver.
type TenantResolver interface {
	Resolve(ctx context.Context, apiKey string) (tenant.Tenant, []byte, error)
}

// UserLookup maps an external id to the internal user id inside a tenant, or returns ErrUnknownUser.
type UserLookup func(ctx context.Context, tenantID uuid.UUID, externalID string) (uuid.UUID, error)

// RequestLimiter is ratelimit.Limiter: a token bucket named by its key parts.
type RequestLimiter interface {
	Allow(ctx context.Context, capacity int, perSecond float64, keyParts ...string) (bool, time.Duration)
}

// What one caller may do over REST, on top of the stricter limits of sending messages and creating uploads. A user
// is one bucket across all their devices; a tenant backend (server token) is one bucket per tenant. Generous: it
// stops a runaway client or a loop, not normal use.
const (
	userRequestBurst        = 60
	userRequestsPerSecond   = 20
	serverRequestBurst      = 200
	serverRequestsPerSecond = 100
)

type Middleware struct {
	tenants TenantResolver
	users   UserLookup
	log     *slog.Logger
	now     func() time.Time
	limiter RequestLimiter
}

// SetRequestLimiter turns on the limit on every authenticated REST request. Without it nothing is limited here.
func (m *Middleware) SetRequestLimiter(l RequestLimiter) { m.limiter = l }

// within takes a token for this caller and answers 429 itself when there is none.
func (m *Middleware) within(w http.ResponseWriter, r *http.Request, burst int, perSecond float64, parts ...string) bool {
	if m.limiter == nil {
		return true
	}
	ok, wait := m.limiter.Allow(r.Context(), burst, perSecond, append([]string{"req"}, parts...)...)
	if ok {
		return true
	}
	httpx.WriteErr(w, r, m.log, httpx.ErrRateLimited(wait, "too many requests, slow down"))
	return false
}

func NewMiddleware(tenants TenantResolver, users UserLookup, log *slog.Logger) *Middleware {
	return &Middleware{tenants: tenants, users: users, log: log, now: time.Now}
}

// User authenticates an end user: X-Api-Key + a user token. The user must exist in the tenant.
func (m *Middleware) User(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t, secret, token, ok := m.credentials(w, r)
		if !ok {
			return
		}
		ext, err := VerifyUserToken(token, secret, m.now())
		if err != nil {
			m.reject(w, r, err)
			return
		}
		userID, err := m.users(r.Context(), t.ID, ext)
		if errors.Is(err, ErrUnknownUser) {
			m.unauthorized(w, r, httpx.CodeUnauthenticated,
				"user "+ext+" does not exist in this app; the app backend must create it with PUT /v1/server/users/{external_id} first")
			return
		}
		if err != nil {
			m.unavailable(w, r, err)
			return
		}
		if !m.within(w, r, userRequestBurst, userRequestsPerSecond, t.ID.String(), userID.String()) {
			return
		}
		ctx := WithPrincipal(r.Context(), Principal{Tenant: t, UserID: userID, ExternalID: ext})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// Server authenticates a tenant backend: X-Api-Key + a server token.
func (m *Middleware) Server(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t, secret, token, ok := m.credentials(w, r)
		if !ok {
			return
		}
		if err := VerifyServerToken(token, secret, m.now()); err != nil {
			m.reject(w, r, err)
			return
		}
		if !m.within(w, r, serverRequestBurst, serverRequestsPerSecond, t.ID.String(), "server") {
			return
		}
		next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), Principal{Tenant: t})))
	})
}

// credentials resolves the api key and extracts the bearer token, writing the error response itself.
func (m *Middleware) credentials(w http.ResponseWriter, r *http.Request) (tenant.Tenant, []byte, string, bool) {
	apiKey := strings.TrimSpace(r.Header.Get(HeaderAPIKey))
	if apiKey == "" {
		m.unauthorized(w, r, httpx.CodeUnauthenticated, "missing "+HeaderAPIKey+" header")
		return tenant.Tenant{}, nil, "", false
	}
	token, ok := bearer(r.Header.Get("Authorization"))
	if !ok {
		m.unauthorized(w, r, httpx.CodeUnauthenticated, "missing Authorization: Bearer <token> header")
		return tenant.Tenant{}, nil, "", false
	}
	t, secret, err := m.tenants.Resolve(r.Context(), apiKey)
	if errors.Is(err, tenant.ErrNotFound) {
		m.unauthorized(w, r, httpx.CodeUnauthenticated, "unknown or disabled api key")
		return tenant.Tenant{}, nil, "", false
	}
	if err != nil {
		m.unavailable(w, r, err)
		return tenant.Tenant{}, nil, "", false
	}
	return t, secret, token, true
}

func (m *Middleware) reject(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, ErrTokenExpired) {
		m.unauthorized(w, r, httpx.CodeTokenExpired, "token expired")
		return
	}
	m.unauthorized(w, r, httpx.CodeUnauthenticated, err.Error())
}

func (m *Middleware) unauthorized(w http.ResponseWriter, r *http.Request, code, msg string) {
	m.log.LogAttrs(r.Context(), slog.LevelDebug, "authentication failed",
		slog.String("code", code), slog.String("reason", msg), slog.String("path", r.URL.Path),
		slog.String("request_id", httpx.RequestID(r.Context())))
	httpx.WriteError(w, r, http.StatusUnauthorized, code, msg)
}

func (m *Middleware) unavailable(w http.ResponseWriter, r *http.Request, err error) {
	m.log.Error("authentication backend failed", "error", err, "request_id", httpx.RequestID(r.Context()))
	httpx.WriteError(w, r, http.StatusServiceUnavailable, httpx.CodeUnavailable, "authentication temporarily unavailable")
}

func bearer(h string) (string, bool) {
	scheme, token, ok := strings.Cut(strings.TrimSpace(h), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	token = strings.TrimSpace(token)
	return token, token != ""
}
