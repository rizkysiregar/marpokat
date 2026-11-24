package console

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/rizkysiregar/marpokat/server/internal/platform/httpx"
	"github.com/rizkysiregar/marpokat/server/internal/store"
	"github.com/rizkysiregar/marpokat/server/internal/tenant"
)

// SessionCookie holds the session token (PLAN D70). The __Host- prefix makes the browser refuse it unless it is
// Secure, has Path=/ and no Domain: it belongs to exactly the host that set it, so the portal and the admin app each
// have their own session.
const SessionCookie = "__Host-mp_session"

// Codes of the request guard.
const (
	CodeOriginNotAllowed     = "origin_not_allowed"
	CodeUnsupportedMediaType = "unsupported_media_type"
)

// HandlerOptions says which sites may use the API and whose word to take for the visitor's address.
type HandlerOptions struct {
	// PortalOrigins and AdminOrigins are origins as browsers send them (scheme://host[:port], lower case).
	PortalOrigins []string
	AdminOrigins  []string
	// TrustedProxies are the networks whose CF-Connecting-IP / X-Forwarded-For is believed (the ingress).
	TrustedProxies []netip.Prefix
}

// Handler serves the Console API (spec/console.yaml) to the website's two apps, from the browser.
type Handler struct {
	svc     *Service
	log     *slog.Logger
	portal  map[string]bool
	admin   map[string]bool
	proxies []netip.Prefix
}

func NewHandler(svc *Service, opts HandlerOptions, log *slog.Logger) *Handler {
	h := &Handler{svc: svc, log: log, portal: map[string]bool{}, admin: map[string]bool{}, proxies: opts.TrustedProxies}
	for _, o := range opts.PortalOrigins {
		h.portal[strings.ToLower(o)] = true
	}
	for _, o := range opts.AdminOrigins {
		h.admin[strings.ToLower(o)] = true
	}
	return h
}

type ctxKey struct{}

// Routes returns the API behind the request guard.
func (h *Handler) Routes() http.Handler {
	mux := http.NewServeMux()
	open := func(f http.HandlerFunc) http.Handler { return f }
	authed := func(f func(http.ResponseWriter, *http.Request, Principal)) http.Handler {
		return h.session(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			f(w, r, r.Context().Value(ctxKey{}).(Principal))
		}))
	}
	const v = "/console/v1"

	mux.Handle("GET "+v+"/status", open(h.status))
	mux.Handle("POST "+v+"/auth/register", open(h.register))
	mux.Handle("POST "+v+"/auth/verify-email", open(h.verifyEmail))
	mux.Handle("POST "+v+"/auth/resend-verification", authed(h.resendVerification))
	mux.Handle("POST "+v+"/auth/login", open(h.login))
	mux.Handle("POST "+v+"/auth/logout", authed(h.logout))
	mux.Handle("POST "+v+"/auth/forgot-password", open(h.forgotPassword))
	mux.Handle("POST "+v+"/auth/reset-password", open(h.resetPassword))

	mux.Handle("GET "+v+"/me", authed(h.getMe))
	mux.Handle("PATCH "+v+"/me", authed(h.updateMe))
	mux.Handle("PUT "+v+"/me/password", authed(h.changePassword))
	mux.Handle("GET "+v+"/me/sessions", authed(h.listSessions))
	mux.Handle("DELETE "+v+"/me/sessions/{session_id}", authed(h.endSession))
	mux.Handle("GET "+v+"/me/audit", authed(h.listAudit))

	mux.Handle("GET "+v+"/projects", authed(h.listProjects))
	mux.Handle("POST "+v+"/projects", authed(h.createProject))
	mux.Handle("GET "+v+"/projects/{project_id}", authed(h.getProject))
	mux.Handle("DELETE "+v+"/projects/{project_id}", authed(h.deleteProject))
	mux.Handle("POST "+v+"/projects/{project_id}/restore", authed(h.restoreProject))
	mux.Handle("PATCH "+v+"/projects/{project_id}", authed(h.updateProject))
	mux.Handle("POST "+v+"/projects/{project_id}/rotate-secret", authed(h.rotateSecret))
	mux.Handle("GET "+v+"/projects/{project_id}/push", authed(h.getPush))
	mux.Handle("PUT "+v+"/projects/{project_id}/push", authed(h.setPush))
	mux.Handle("DELETE "+v+"/projects/{project_id}/push", authed(h.removePush))
	mux.Handle("POST "+v+"/projects/{project_id}/disable", authed(h.setProjectDisabled))

	mux.Handle("GET "+v+"/admin/accounts", authed(h.adminAccounts))
	mux.Handle("GET "+v+"/admin/accounts/{account_id}", authed(h.adminAccount))
	mux.Handle("POST "+v+"/admin/accounts/{account_id}/sign-out", authed(h.adminEndSessions))
	mux.Handle("POST "+v+"/admin/accounts/{account_id}/disable", authed(h.adminSetAccountDisabled))
	mux.Handle("GET "+v+"/admin/projects", authed(h.adminProjects))
	mux.Handle("GET "+v+"/admin/projects/{project_id}", authed(h.adminProject))
	mux.Handle("POST "+v+"/admin/projects/{project_id}/suspend", authed(h.adminSuspendProject))
	mux.Handle("POST "+v+"/admin/projects/{project_id}/unsuspend", authed(h.adminUnsuspendProject))
	mux.Handle("POST "+v+"/admin/projects/{project_id}/delete", authed(h.adminDeleteProject))
	mux.Handle("POST "+v+"/admin/projects/{project_id}/restore", authed(h.adminRestoreProject))
	mux.Handle("POST "+v+"/admin/projects/{project_id}/disable", authed(h.adminSetProjectDisabled))
	mux.Handle("GET "+v+"/admin/stats", authed(h.adminStats))
	mux.Handle("GET "+v+"/admin/stats/series", authed(h.adminSeries))
	mux.Handle("GET "+v+"/admin/audit", authed(h.adminAudit))

	return h.guard(mux)
}

// guard runs before anything else. Every answer is no-store (some carry secrets) and nosniff. A request that changes
// something must come from the portal or the admin app (Origin, which a browser always sends on such a request and a
// page cannot fake) and, when it has a body, be JSON, which a form on another site cannot send. Together with the
// SameSite=Strict cookie this shuts out cross-site request forgery (PLAN D70).
func (h *Handler) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		switch r.Method {
		case http.MethodGet, http.MethodHead:
		default:
			if _, ok := h.originScope(r); !ok {
				httpx.WriteError(w, r, http.StatusForbidden, CodeOriginNotAllowed, "this request did not come from the Marpokat website")
				return
			}
			if r.ContentLength != 0 || r.Header.Get("Content-Type") != "" {
				if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
					httpx.WriteError(w, r, http.StatusUnsupportedMediaType, CodeUnsupportedMediaType, "send the body as application/json")
					return
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

// originScope says which app a request comes from, by its Origin header.
func (h *Handler) originScope(r *http.Request) (Scope, bool) {
	o := strings.ToLower(r.Header.Get("Origin"))
	switch {
	case o == "":
		return "", false
	case h.admin[o]:
		return ScopeAdmin, true
	case h.portal[o]:
		return ScopePortal, true
	}
	return "", false
}

func (h *Handler) session(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(SessionCookie)
		if err != nil || c.Value == "" {
			httpx.WriteErr(w, r, h.log, errUnauthenticated)
			return
		}
		p, err := h.svc.Authenticate(r.Context(), c.Value)
		if err != nil {
			if errors.Is(err, errUnauthenticated) { // ended or unknown: the browser can forget it
				clearSessionCookie(w)
			}
			httpx.WriteErr(w, r, h.log, err)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, p)))
	})
}

func setSessionCookie(w http.ResponseWriter, token string, until time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name: SessionCookie, Value: token, Path: "/", Expires: until, MaxAge: max(int(time.Until(until).Seconds()), 1),
		Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode,
	})
}

func clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name: SessionCookie, Value: "", Path: "/", MaxAge: -1, Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode,
	})
}

func (h *Handler) caller(r *http.Request) Caller {
	return Caller{IP: h.clientIP(r), UserAgent: r.UserAgent()}
}

// clientIP is the visitor's address (PLAN D73). The headers that name it (CF-Connecting-IP from Cloudflare, then the
// ingress's) are only believed when the connection comes from a trusted proxy; anyone else could write them to dodge
// the limits or to put a false address in the audit log.
func (h *Handler) clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return host
	}
	addr = addr.Unmap()
	for _, p := range h.proxies {
		if p.Contains(addr) {
			return httpx.ClientIP(r)
		}
	}
	return addr.String()
}

func (h *Handler) fail(w http.ResponseWriter, r *http.Request, err error) {
	httpx.WriteErr(w, r, h.log, err)
}

func noContent(w http.ResponseWriter) { w.WriteHeader(http.StatusNoContent) }

// ---------------------------------------------------------------- JSON shapes

type accountJSON struct {
	ID              uuid.UUID `json:"id"`
	Email           string    `json:"email"`
	Name            string    `json:"name"`
	EmailVerified   bool      `json:"email_verified"`
	IsPlatformAdmin bool      `json:"is_platform_admin"`
	Disabled        bool      `json:"disabled"`
	CreatedAt       time.Time `json:"created_at"`
}

func accountOf(a store.Account) accountJSON {
	return accountJSON{ID: a.ID, Email: a.Email, Name: a.Name, EmailVerified: a.EmailVerifiedAt != nil,
		IsPlatformAdmin: a.IsPlatformAdmin, Disabled: a.DisabledAt != nil, CreatedAt: a.CreatedAt}
}

type projectJSON struct {
	ID                   uuid.UUID  `json:"id"`
	Slug                 string     `json:"slug"`
	Name                 string     `json:"name"`
	APIKey               string     `json:"api_key"`
	ClientCanCreateDM    bool       `json:"client_can_create_dm"`
	ClientCanCreateGroup bool       `json:"client_can_create_group"`
	PushConfigured       bool       `json:"push_configured"`
	Disabled             bool       `json:"disabled"`
	Environment          *string    `json:"environment"`
	Suspended            bool       `json:"suspended"`
	SuspendedReason      *string    `json:"suspended_reason"`
	DeleteAfter          *time.Time `json:"delete_after"`
	CreatedAt            time.Time  `json:"created_at"`
}

func projectOf(t store.Tenant) projectJSON {
	var cfg tenant.Config
	_ = json.Unmarshal([]byte(t.Config), &cfg)
	return projectJSON{ID: t.ID, Slug: t.Slug, Name: t.Name, APIKey: t.ApiKey,
		ClientCanCreateDM: cfg.ClientCanCreateDM, ClientCanCreateGroup: cfg.ClientCanCreateGroup,
		PushConfigured: len(t.FcmCredentialsEnc) > 0, Disabled: t.DisabledAt != nil, Environment: t.Environment, Suspended: t.SuspendedAt != nil, SuspendedReason: t.SuspendedReason, DeleteAfter: t.DeleteAfter, CreatedAt: t.CreatedAt}
}

type pushJSON struct {
	Configured        bool   `json:"configured"`
	FirebaseProjectID string `json:"firebase_project_id,omitempty"`
	ClientEmail       string `json:"client_email,omitempty"`
}

func pushOf(p PushStatus) pushJSON {
	return pushJSON(p)
}

// ---------------------------------------------------------------- auth

func (h *Handler) register(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Email, Password, Name string
		TurnstileToken        string `json:"turnstile_token"`
	}
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	if err := h.svc.Register(r.Context(), in.Email, in.Password, in.Name, in.TurnstileToken, h.caller(r)); err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, map[string]string{"message": "if the address can be used, an e-mail with a link to confirm it is on its way"})
}

func (h *Handler) verifyEmail(w http.ResponseWriter, r *http.Request) {
	var in struct{ Token string }
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	if err := h.svc.VerifyEmail(r.Context(), in.Token); err != nil {
		h.fail(w, r, err)
		return
	}
	noContent(w)
}

func (h *Handler) resendVerification(w http.ResponseWriter, r *http.Request, p Principal) {
	if err := h.svc.ResendVerification(r.Context(), p); err != nil {
		h.fail(w, r, err)
		return
	}
	noContent(w)
}

func (h *Handler) login(w http.ResponseWriter, r *http.Request) {
	var in struct{ Email, Password string }
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	scope, _ := h.originScope(r) // the guard made sure there is one
	res, err := h.svc.Login(r.Context(), in.Email, in.Password, scope, h.caller(r))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	// the token goes into an HttpOnly cookie and nowhere else: no script on the page can read it
	setSessionCookie(w, res.Token, res.AbsoluteExpiresAt)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"expires_at": res.ExpiresAt, "scope": scope, "account": accountOf(res.Account)})
}

func (h *Handler) logout(w http.ResponseWriter, r *http.Request, p Principal) {
	if err := h.svc.Logout(r.Context(), p, h.caller(r)); err != nil {
		h.fail(w, r, err)
		return
	}
	clearSessionCookie(w)
	noContent(w)
}

func (h *Handler) forgotPassword(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Email          string
		TurnstileToken string `json:"turnstile_token"`
	}
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	if err := h.svc.ForgotPassword(r.Context(), in.Email, in.TurnstileToken, h.caller(r)); err != nil {
		h.fail(w, r, err)
		return
	}
	noContent(w)
}

func (h *Handler) resetPassword(w http.ResponseWriter, r *http.Request) {
	var in struct{ Token, Password string }
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	if err := h.svc.ResetPassword(r.Context(), in.Token, in.Password, h.caller(r)); err != nil {
		h.fail(w, r, err)
		return
	}
	noContent(w)
}

// ---------------------------------------------------------------- account

func (h *Handler) getMe(w http.ResponseWriter, _ *http.Request, p Principal) {
	httpx.WriteJSON(w, http.StatusOK, accountOf(p.Account))
}

func (h *Handler) updateMe(w http.ResponseWriter, r *http.Request, p Principal) {
	var in struct{ Name *string }
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	if in.Name == nil {
		httpx.WriteJSON(w, http.StatusOK, accountOf(p.Account))
		return
	}
	acc, err := h.svc.UpdateName(r.Context(), p, *in.Name)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, accountOf(acc))
}

func (h *Handler) changePassword(w http.ResponseWriter, r *http.Request, p Principal) {
	var in struct {
		CurrentPassword string `json:"current_password"`
		NewPassword     string `json:"new_password"`
	}
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	if err := h.svc.ChangePassword(r.Context(), p, in.CurrentPassword, in.NewPassword, h.caller(r)); err != nil {
		h.fail(w, r, err)
		return
	}
	noContent(w)
}

func (h *Handler) listSessions(w http.ResponseWriter, r *http.Request, p Principal) {
	rows, err := h.svc.Sessions(r.Context(), p)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	type item struct {
		ID         uuid.UUID `json:"id"`
		Current    bool      `json:"current"`
		CreatedAt  time.Time `json:"created_at"`
		LastSeenAt time.Time `json:"last_seen_at"`
		UserAgent  string    `json:"user_agent"`
		IP         string    `json:"ip"`
		Scope      string    `json:"scope"`
	}
	out := make([]item, len(rows))
	for i, s := range rows {
		out[i] = item{s.ID, s.ID == p.SessionID, s.CreatedAt, s.LastSeenAt, s.UserAgent, s.Ip, s.Scope}
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"sessions": out})
}

func (h *Handler) listAudit(w http.ResponseWriter, r *http.Request, p Principal) {
	limit, err := httpx.QueryLimit(r, 50, 100)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	rows, err := h.svc.Audit(r.Context(), p, limit)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	type item struct {
		Action string    `json:"action"`
		Target *string   `json:"target"`
		IP     string    `json:"ip"`
		At     time.Time `json:"at"`
	}
	out := make([]item, len(rows))
	for i, e := range rows {
		out[i] = item{e.Action, e.Target, e.Ip, e.CreatedAt}
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"events": out})
}

// ---------------------------------------------------------------- projects

func (h *Handler) listProjects(w http.ResponseWriter, r *http.Request, p Principal) {
	rows, err := h.svc.Projects(r.Context(), p)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	out := make([]projectJSON, len(rows))
	for i, t := range rows {
		out[i] = projectOf(t)
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"projects": out})
}

func (h *Handler) createProject(w http.ResponseWriter, r *http.Request, p Principal) {
	var in struct{ Name, Slug, Environment string }
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	res, err := h.svc.CreateProject(r.Context(), p, in.Name, in.Slug, in.Environment, h.caller(r))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	type withSecret struct {
		projectJSON
		APISecret string `json:"api_secret"`
	}
	httpx.WriteJSON(w, http.StatusCreated, withSecret{projectOf(res.Project), res.Secret})
}

func (h *Handler) getProject(w http.ResponseWriter, r *http.Request, p Principal) {
	id, err := httpx.PathUUID(r, "project_id")
	if err != nil {
		h.fail(w, r, errProjectNotFound)
		return
	}
	d, err := h.svc.Project(r.Context(), p, id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	type usage struct {
		Users    int64 `json:"users"`
		Channels int64 `json:"channels"`
		Messages int64 `json:"messages"`
	}
	httpx.WriteJSON(w, http.StatusOK, struct {
		projectJSON
		Usage usage `json:"usage"`
	}{projectOf(d.Project), usage{d.Users, d.Channels, d.Messages}})
}

func (h *Handler) updateProject(w http.ResponseWriter, r *http.Request, p Principal) {
	id, err := httpx.PathUUID(r, "project_id")
	if err != nil {
		h.fail(w, r, errProjectNotFound)
		return
	}
	var in struct {
		Name                 *string `json:"name"`
		ClientCanCreateDM    *bool   `json:"client_can_create_dm"`
		ClientCanCreateGroup *bool   `json:"client_can_create_group"`
		Environment          *string `json:"environment"`
	}
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	row, err := h.svc.UpdateProject(r.Context(), p, id, ProjectUpdate{in.Name, in.ClientCanCreateDM, in.ClientCanCreateGroup, in.Environment}, h.caller(r))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, projectOf(row))
}

func (h *Handler) rotateSecret(w http.ResponseWriter, r *http.Request, p Principal) {
	id, err := httpx.PathUUID(r, "project_id")
	if err != nil {
		h.fail(w, r, errProjectNotFound)
		return
	}
	var in struct{ Password string }
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	secret, err := h.svc.RotateSecret(r.Context(), p, id, in.Password, h.caller(r))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"api_secret": secret})
}

func (h *Handler) getPush(w http.ResponseWriter, r *http.Request, p Principal) {
	id, err := httpx.PathUUID(r, "project_id")
	if err != nil {
		h.fail(w, r, errProjectNotFound)
		return
	}
	st, err := h.svc.Push(r.Context(), p, id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, pushOf(st))
}

func (h *Handler) setPush(w http.ResponseWriter, r *http.Request, p Principal) {
	id, err := httpx.PathUUID(r, "project_id")
	if err != nil {
		h.fail(w, r, errProjectNotFound)
		return
	}
	var in struct {
		ServiceAccount string `json:"service_account"`
		Password       string `json:"password"`
	}
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	st, err := h.svc.SetPush(r.Context(), p, id, in.ServiceAccount, in.Password, h.caller(r))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, pushOf(st))
}

func (h *Handler) removePush(w http.ResponseWriter, r *http.Request, p Principal) {
	id, err := httpx.PathUUID(r, "project_id")
	if err != nil {
		h.fail(w, r, errProjectNotFound)
		return
	}
	var in struct{ Password string }
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	if err := h.svc.RemovePush(r.Context(), p, id, in.Password, h.caller(r)); err != nil {
		h.fail(w, r, err)
		return
	}
	noContent(w)
}

func (h *Handler) setProjectDisabled(w http.ResponseWriter, r *http.Request, p Principal) {
	id, err := httpx.PathUUID(r, "project_id")
	if err != nil {
		h.fail(w, r, errProjectNotFound)
		return
	}
	var in struct {
		Disabled bool
		Password string
	}
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	row, err := h.svc.SetProjectDisabled(r.Context(), p, id, in.Disabled, in.Password, h.caller(r))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, projectOf(row))
}

// ---------------------------------------------------------------- platform admin

func (h *Handler) adminAccounts(w http.ResponseWriter, r *http.Request, p Principal) {
	limit, err := httpx.QueryLimit(r, 50, 100)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	after := uuid.Nil
	if c := r.URL.Query().Get("cursor"); c != "" {
		if after, err = uuid.Parse(c); err != nil {
			h.fail(w, r, httpx.FieldErrors{"cursor": "is not a cursor this API gave"})
			return
		}
	}
	rows, err := h.svc.AdminAccounts(r.Context(), p, r.URL.Query().Get("q"), after, limit+1)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	var next *string
	if len(rows) > limit {
		rows = rows[:limit]
		c := rows[len(rows)-1].ID.String()
		next = &c
	}
	out := make([]accountJSON, len(rows))
	for i, a := range rows {
		out[i] = accountOf(a)
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"accounts": out, "next_cursor": next})
}

func (h *Handler) adminSetAccountDisabled(w http.ResponseWriter, r *http.Request, p Principal) {
	id, err := httpx.PathUUID(r, "account_id")
	if err != nil {
		h.fail(w, r, httpx.ErrNotFound("account not found"))
		return
	}
	var in struct{ Disabled bool }
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	acc, err := h.svc.AdminSetAccountDisabled(r.Context(), p, id, in.Disabled, h.caller(r))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, accountOf(acc))
}

type adminProjectJSON struct {
	projectJSON
	OwnerEmail *string `json:"owner_email"`
}

func (h *Handler) adminProjects(w http.ResponseWriter, r *http.Request, p Principal) {
	rows, err := h.svc.AdminProjects(r.Context(), p)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	out := make([]adminProjectJSON, len(rows))
	for i, a := range rows {
		out[i] = adminProjectJSON{projectOf(a.Project), a.OwnerEmail}
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"projects": out})
}

func (h *Handler) adminSetProjectDisabled(w http.ResponseWriter, r *http.Request, p Principal) {
	id, err := httpx.PathUUID(r, "project_id")
	if err != nil {
		h.fail(w, r, errProjectNotFound)
		return
	}
	var in struct{ Disabled bool }
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	res, err := h.svc.AdminSetProjectDisabled(r.Context(), p, id, in.Disabled, h.caller(r))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, adminProjectJSON{projectOf(res.Project), res.OwnerEmail})
}

func (h *Handler) adminStats(w http.ResponseWriter, r *http.Request, p Principal) {
	u, err := h.svc.AdminStats(r.Context(), p)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]int64{
		"accounts": u.Accounts, "projects": u.Projects, "active_projects": u.ActiveProjects,
		"users": u.Users, "channels": u.Channels, "messages": u.Messages,
	})
}

// ---------------------------------------------------------------- status, deletion, sessions

func (h *Handler) status(w http.ResponseWriter, _ *http.Request) {
	httpx.WriteJSON(w, http.StatusOK, map[string]bool{"registration_open": h.svc.RegistrationOpen()})
}

func (h *Handler) deleteProject(w http.ResponseWriter, r *http.Request, p Principal) {
	id, err := httpx.PathUUID(r, "project_id")
	if err != nil {
		h.fail(w, r, errProjectNotFound)
		return
	}
	var in struct{ Password string }
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	row, err := h.svc.DeleteProject(r.Context(), p, id, in.Password, h.caller(r))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusAccepted, projectOf(row))
}

func (h *Handler) restoreProject(w http.ResponseWriter, r *http.Request, p Principal) {
	id, err := httpx.PathUUID(r, "project_id")
	if err != nil {
		h.fail(w, r, errProjectNotFound)
		return
	}
	row, err := h.svc.RestoreProject(r.Context(), p, id, h.caller(r))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, projectOf(row))
}

func (h *Handler) endSession(w http.ResponseWriter, r *http.Request, p Principal) {
	id, err := httpx.PathUUID(r, "session_id")
	if err != nil {
		h.fail(w, r, httpx.ErrNotFound("session not found"))
		return
	}
	if err := h.svc.EndSession(r.Context(), p, id, h.caller(r)); err != nil {
		h.fail(w, r, err)
		return
	}
	if id == p.SessionID {
		clearSessionCookie(w)
	}
	noContent(w)
}

// ---------------------------------------------------------------- platform admin: details, audit, charts

type sessionJSON struct {
	ID         uuid.UUID `json:"id"`
	CreatedAt  time.Time `json:"created_at"`
	LastSeenAt time.Time `json:"last_seen_at"`
	UserAgent  string    `json:"user_agent"`
	IP         string    `json:"ip"`
	Scope      string    `json:"scope"`
}

type auditJSON struct {
	ID           uuid.UUID `json:"id"`
	Action       string    `json:"action"`
	Target       *string   `json:"target"`
	IP           string    `json:"ip"`
	At           time.Time `json:"at"`
	AccountID    uuid.UUID `json:"account_id,omitzero"`
	AccountEmail string    `json:"account_email,omitempty"`
}

func (h *Handler) adminAccount(w http.ResponseWriter, r *http.Request, p Principal) {
	id, err := httpx.PathUUID(r, "account_id")
	if err != nil {
		h.fail(w, r, errAccountNotFound)
		return
	}
	v, err := h.svc.AdminAccount(r.Context(), p, id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	projects := make([]projectJSON, len(v.Projects))
	for i, t := range v.Projects {
		projects[i] = projectOf(t)
	}
	sessions := make([]sessionJSON, len(v.Sessions))
	for i, s := range v.Sessions {
		sessions[i] = sessionJSON{s.ID, s.CreatedAt, s.LastSeenAt, s.UserAgent, s.Ip, s.Scope}
	}
	events := make([]auditJSON, len(v.Audit))
	for i, e := range v.Audit {
		events[i] = auditJSON{ID: e.ID, Action: e.Action, Target: e.Target, IP: e.Ip, At: e.CreatedAt}
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"account": accountOf(v.Account), "projects": projects, "sessions": sessions, "events": events,
	})
}

func (h *Handler) adminEndSessions(w http.ResponseWriter, r *http.Request, p Principal) {
	id, err := httpx.PathUUID(r, "account_id")
	if err != nil {
		h.fail(w, r, errAccountNotFound)
		return
	}
	if err := h.svc.AdminEndSessions(r.Context(), p, id, h.caller(r)); err != nil {
		h.fail(w, r, err)
		return
	}
	noContent(w)
}

func (h *Handler) adminProject(w http.ResponseWriter, r *http.Request, p Principal) {
	id, err := httpx.PathUUID(r, "project_id")
	if err != nil {
		h.fail(w, r, errProjectNotFound)
		return
	}
	v, err := h.svc.AdminProject(r.Context(), p, id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"project":        adminProjectJSON{projectOf(v.Project), v.OwnerEmail},
		"owner_id":       v.Project.AccountID,
		"owner_is_admin": v.OwnerIsAdmin,
		"usage":          map[string]int64{"users": v.Usage.Users, "channels": v.Usage.Channels, "messages": v.Usage.Messages},
		"push":           pushOf(v.Push),
	})
}

func (h *Handler) adminAudit(w http.ResponseWriter, r *http.Request, p Principal) {
	limit, err := httpx.QueryLimit(r, 50, 100)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	q := r.URL.Query()
	f := AuditFilter{Action: q.Get("action")}
	for name, dst := range map[string]*uuid.UUID{"account_id": &f.AccountID, "cursor": &f.Before} {
		if raw := q.Get(name); raw != "" {
			if *dst, err = uuid.Parse(raw); err != nil {
				h.fail(w, r, httpx.FieldErrors{name: "is not an id this API gave"})
				return
			}
		}
	}
	rows, err := h.svc.AdminAudit(r.Context(), p, f, limit+1)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	var next *string
	if len(rows) > limit {
		rows = rows[:limit]
		c := rows[len(rows)-1].ConsoleAudit.ID.String()
		next = &c
	}
	out := make([]auditJSON, len(rows))
	for i, row := range rows {
		e := row.ConsoleAudit
		out[i] = auditJSON{e.ID, e.Action, e.Target, e.Ip, e.CreatedAt, e.AccountID, row.AccountEmail}
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"events": out, "next_cursor": next})
}

func (h *Handler) adminSeries(w http.ResponseWriter, r *http.Request, p Principal) {
	days := 30
	if raw := r.URL.Query().Get("days"); raw != "" {
		if _, err := fmt.Sscanf(raw, "%d", &days); err != nil {
			h.fail(w, r, httpx.FieldErrors{"days": "must be 7, 30 or 90"})
			return
		}
	}
	v, err := h.svc.AdminSeries(r.Context(), p, days)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	type day struct {
		Day         string `json:"day"`
		Accounts    int64  `json:"accounts"`
		Projects    int64  `json:"projects"`
		Messages    int64  `json:"messages"`
		ActiveUsers int64  `json:"active_users"`
	}
	type top struct {
		ID       uuid.UUID `json:"id"`
		Name     string    `json:"name"`
		Slug     string    `json:"slug"`
		Messages int64     `json:"messages"`
	}
	ds := make([]day, len(v.Days))
	for i, d := range v.Days {
		ds[i] = day{d.Day, d.Accounts, d.Projects, d.Messages, d.ActiveUsers}
	}
	ts := make([]top, len(v.TopProjects))
	for i, t := range v.TopProjects {
		ts[i] = top{t.ID, t.Name, t.Slug, t.Messages}
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"days": ds, "top_projects": ts})
}

// ---------------------------------------------------------------- platform admin: control over projects (PLAN D79)

func (h *Handler) adminProjectAction(w http.ResponseWriter, r *http.Request, act func(uuid.UUID) (store.Tenant, error)) {
	id, err := httpx.PathUUID(r, "project_id")
	if err != nil {
		h.fail(w, r, errProjectNotFound)
		return
	}
	row, err := act(id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, projectOf(row))
}

func (h *Handler) adminSuspendProject(w http.ResponseWriter, r *http.Request, p Principal) {
	var in struct{ Reason string }
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	h.adminProjectAction(w, r, func(id uuid.UUID) (store.Tenant, error) {
		return h.svc.AdminSuspendProject(r.Context(), p, id, in.Reason, h.caller(r))
	})
}

func (h *Handler) adminUnsuspendProject(w http.ResponseWriter, r *http.Request, p Principal) {
	h.adminProjectAction(w, r, func(id uuid.UUID) (store.Tenant, error) {
		return h.svc.AdminUnsuspendProject(r.Context(), p, id, h.caller(r))
	})
}

func (h *Handler) adminDeleteProject(w http.ResponseWriter, r *http.Request, p Principal) {
	var in struct {
		Confirm  string
		Password string
		Now      bool
	}
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	h.adminProjectAction(w, r, func(id uuid.UUID) (store.Tenant, error) {
		return h.svc.AdminDeleteProject(r.Context(), p, id, in.Confirm, in.Password, in.Now, h.caller(r))
	})
}

func (h *Handler) adminRestoreProject(w http.ResponseWriter, r *http.Request, p Principal) {
	h.adminProjectAction(w, r, func(id uuid.UUID) (store.Tenant, error) {
		return h.svc.AdminRestoreProject(r.Context(), p, id, h.caller(r))
	})
}
