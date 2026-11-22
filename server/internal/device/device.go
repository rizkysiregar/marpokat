// Package device keeps the push tokens of users: what the push consumer sends notifications to.
package device

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rizkysiregar/marpokat/server/internal/auth"
	"github.com/rizkysiregar/marpokat/server/internal/platform/httpx"
	"github.com/rizkysiregar/marpokat/server/internal/store"
)

const (
	PlatformAndroid = "android"
	PlatformWeb     = "web"

	maxTokenLength = 4096
	// MaxPerUser: the most recently registered tokens are kept, so phones that were replaced without logging out
	// do not keep receiving (and costing) notifications forever.
	MaxPerUser = 10
)

type Service struct {
	pool *pgxpool.Pool
}

func NewService(pool *pgxpool.Pool) *Service { return &Service{pool: pool} }

// Register stores a push token for the user. A token another user of the tenant registered before moves to this
// user: it is the same device, with a different person signed in.
func (s *Service) Register(ctx context.Context, tenantID, userID uuid.UUID, platform, token string) error {
	errs := httpx.FieldErrors{}
	if platform != PlatformAndroid && platform != PlatformWeb {
		errs["platform"] = `must be "android" or "web"`
	}
	token = strings.TrimSpace(token)
	if token == "" || len(token) > maxTokenLength || !utf8.ValidString(token) || strings.IndexFunc(token, func(r rune) bool { return unicode.IsControl(r) || unicode.IsSpace(r) }) >= 0 {
		errs["push_token"] = fmt.Sprintf("is required: 1-%d characters, no spaces", maxTokenLength)
	}
	if err := errs.OrNil(); err != nil {
		return err
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		if err := q.UpsertDevice(ctx, store.UpsertDeviceParams{TenantID: tenantID, PushToken: token, UserID: userID, Platform: platform}); err != nil {
			return err
		}
		return q.TrimDevicesOfUser(ctx, store.TrimDevicesOfUserParams{TenantID: tenantID, UserID: userID, Keep: MaxPerUser})
	})
}

// Unregister removes a token of the user, for example when they sign out. A token that is not theirs or not known
// is left alone and is not an error: signing out must always work.
func (s *Service) Unregister(ctx context.Context, tenantID, userID uuid.UUID, token string) error {
	_, err := store.New(s.pool).DeleteDeviceOfUser(ctx, store.DeleteDeviceOfUserParams{TenantID: tenantID, PushToken: token, UserID: userID})
	return err
}

type Handler struct {
	svc *Service
	log *slog.Logger
}

func NewHandler(svc *Service, log *slog.Logger) *Handler { return &Handler{svc: svc, log: log} }

func (h *Handler) Register(mux *http.ServeMux, user func(http.Handler) http.Handler) {
	mux.Handle("POST /v1/devices", user(http.HandlerFunc(h.register)))
	mux.Handle("DELETE /v1/devices/{push_token}", user(http.HandlerFunc(h.unregister)))
}

func (h *Handler) register(w http.ResponseWriter, r *http.Request) {
	p := auth.FromContext(r.Context())
	var body struct {
		Platform  string `json:"platform"`
		PushToken string `json:"push_token"`
	}
	if !httpx.DecodeJSON(w, r, &body) {
		return
	}
	if err := h.svc.Register(r.Context(), p.Tenant.ID, p.UserID, body.Platform, body.PushToken); err != nil {
		httpx.WriteErr(w, r, h.log, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) unregister(w http.ResponseWriter, r *http.Request) {
	p := auth.FromContext(r.Context())
	if err := h.svc.Unregister(r.Context(), p.Tenant.ID, p.UserID, r.PathValue("push_token")); err != nil {
		httpx.WriteErr(w, r, h.log, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
