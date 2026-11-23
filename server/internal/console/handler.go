package console

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/rizkysiregar/marpokat/server/internal/platform/httpx"
	"github.com/rizkysiregar/marpokat/server/internal/store"
	"github.com/rizkysiregar/marpokat/server/internal/tenant"
)

// HeaderServiceKey carries the key that says "this is the website". HeaderClientIP and HeaderClientUA carry the
// visitor's address and browser, as the website saw them.
const (
	HeaderServiceKey = "X-Console-Key"
	HeaderClientIP   = "X-Client-IP"
	HeaderClientUA   = "X-Client-User-Agent"
)

// Handler serves the Console API (spec/console.yaml). It must only be reachable from the website.
type Handler struct {
	svc        *Service
	log        *slog.Logger
	serviceKey []byte
}

func NewHandler(svc *Service, serviceKey string, log *slog.Logger) *Handler {
	return &Handler{svc: svc, log: log, serviceKey: []byte(serviceKey)}
}

type ctxKey struct{}

// Routes returns the API behind the service key.
func (h *Handler) Routes() http.Handler {
	mux := http.NewServeMux()
	open := func(f http.HandlerFunc) http.Handler { return f }
	authed := func(f func(http.ResponseWriter, *http.Request, Principal)) http.Handler {
		return h.session(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			f(w, r, r.Context().Value(ctxKey{}).(Principal))
		}))
	}
	const v = "/console/v1"

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
	mux.Handle("GET "+v+"/me/audit", authed(h.listAudit))

	mux.Handle("GET "+v+"/projects", authed(h.listProjects))
	mux.Handle("POST "+v+"/projects", authed(h.createProject))
	mux.Handle("GET "+v+"/projects/{project_id}", authed(h.getProject))
	mux.Handle("PATCH "+v+"/projects/{project_id}", authed(h.updateProject))
	mux.Handle("POST "+v+"/projects/{project_id}/rotate-secret", authed(h.rotateSecret))
	mux.Handle("GET "+v+"/projects/{project_id}/push", authed(h.getPush))
	mux.Handle("PUT "+v+"/projects/{project_id}/push", authed(h.setPush))
	mux.Handle("DELETE "+v+"/projects/{project_id}/push", authed(h.removePush))
	mux.Handle("POST "+v+"/projects/{project_id}/disable", authed(h.setProjectDisabled))

	mux.Handle("GET "+v+"/admin/accounts", authed(h.adminAccounts))
	mux.Handle("POST "+v+"/admin/accounts/{account_id}/disable", authed(h.adminSetAccountDisabled))
	mux.Handle("GET "+v+"/admin/projects", authed(h.adminProjects))
	mux.Handle("POST "+v+"/admin/projects/{project_id}/disable", authed(h.adminSetProjectDisabled))
	mux.Handle("GET "+v+"/admin/stats", authed(h.adminStats))

	return h.serviceKeyOnly(mux)
}

// serviceKeyOnly refuses anything that does not carry the website's key, before anything else is looked at.
func (h *Handler) serviceKeyOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get(HeaderServiceKey)), h.serviceKey) != 1 {
			httpx.WriteError(w, r, http.StatusUnauthorized, httpx.CodeUnauthenticated, "missing or wrong service key")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (h *Handler) session(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		token, ok := strings.CutPrefix(auth, "Bearer ")
		if !ok {
			httpx.WriteErr(w, r, h.log, errUnauthenticated)
			return
		}
		p, err := h.svc.Authenticate(r.Context(), strings.TrimSpace(token))
		if err != nil {
			httpx.WriteErr(w, r, h.log, err)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, p)))
	})
}

func caller(r *http.Request) Caller {
	ip := strings.TrimSpace(r.Header.Get(HeaderClientIP))
	if net.ParseIP(ip) == nil { // the website did not say, or said something that is not an address
		ip, _, _ = net.SplitHostPort(r.RemoteAddr)
	}
	ua := r.Header.Get(HeaderClientUA)
	if ua == "" {
		ua = r.UserAgent()
	}
	return Caller{IP: ip, UserAgent: ua}
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
	ID                   uuid.UUID `json:"id"`
	Slug                 string    `json:"slug"`
	Name                 string    `json:"name"`
	APIKey               string    `json:"api_key"`
	ClientCanCreateDM    bool      `json:"client_can_create_dm"`
	ClientCanCreateGroup bool      `json:"client_can_create_group"`
	PushConfigured       bool      `json:"push_configured"`
	Disabled             bool      `json:"disabled"`
	CreatedAt            time.Time `json:"created_at"`
}

func projectOf(t store.Tenant) projectJSON {
	var cfg tenant.Config
	_ = json.Unmarshal([]byte(t.Config), &cfg)
	return projectJSON{ID: t.ID, Slug: t.Slug, Name: t.Name, APIKey: t.ApiKey,
		ClientCanCreateDM: cfg.ClientCanCreateDM, ClientCanCreateGroup: cfg.ClientCanCreateGroup,
		PushConfigured: len(t.FcmCredentialsEnc) > 0, Disabled: t.DisabledAt != nil, CreatedAt: t.CreatedAt}
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
	var in struct{ Email, Password, Name string }
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	if err := h.svc.Register(r.Context(), in.Email, in.Password, in.Name, caller(r)); err != nil {
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
	res, err := h.svc.Login(r.Context(), in.Email, in.Password, caller(r))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"token": res.Token, "expires_at": res.ExpiresAt, "account": accountOf(res.Account)})
}

func (h *Handler) logout(w http.ResponseWriter, r *http.Request, p Principal) {
	if err := h.svc.Logout(r.Context(), p, caller(r)); err != nil {
		h.fail(w, r, err)
		return
	}
	noContent(w)
}

func (h *Handler) forgotPassword(w http.ResponseWriter, r *http.Request) {
	var in struct{ Email string }
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	if err := h.svc.ForgotPassword(r.Context(), in.Email, caller(r)); err != nil {
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
	if err := h.svc.ResetPassword(r.Context(), in.Token, in.Password, caller(r)); err != nil {
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
	if err := h.svc.ChangePassword(r.Context(), p, in.CurrentPassword, in.NewPassword, caller(r)); err != nil {
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
	}
	out := make([]item, len(rows))
	for i, s := range rows {
		out[i] = item{s.ID, s.ID == p.SessionID, s.CreatedAt, s.LastSeenAt, s.UserAgent, s.Ip}
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
	var in struct{ Name, Slug string }
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	res, err := h.svc.CreateProject(r.Context(), p, in.Name, in.Slug, caller(r))
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
	}
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	row, err := h.svc.UpdateProject(r.Context(), p, id, ProjectUpdate{in.Name, in.ClientCanCreateDM, in.ClientCanCreateGroup}, caller(r))
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
	secret, err := h.svc.RotateSecret(r.Context(), p, id, in.Password, caller(r))
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
	st, err := h.svc.SetPush(r.Context(), p, id, in.ServiceAccount, in.Password, caller(r))
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
	if err := h.svc.RemovePush(r.Context(), p, id, in.Password, caller(r)); err != nil {
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
	row, err := h.svc.SetProjectDisabled(r.Context(), p, id, in.Disabled, in.Password, caller(r))
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
	rows, err := h.svc.AdminAccounts(r.Context(), p, after, limit+1)
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
	acc, err := h.svc.AdminSetAccountDisabled(r.Context(), p, id, in.Disabled, caller(r))
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
	res, err := h.svc.AdminSetProjectDisabled(r.Context(), p, id, in.Disabled, caller(r))
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
