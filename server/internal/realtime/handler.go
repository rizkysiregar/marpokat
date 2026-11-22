package realtime

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/rizkysiregar/marpokat/server/internal/auth"
	"github.com/rizkysiregar/marpokat/server/internal/message"
	"github.com/rizkysiregar/marpokat/server/internal/platform/httpx"
	"github.com/rizkysiregar/marpokat/server/internal/platform/metrics"
	"github.com/rizkysiregar/marpokat/server/internal/store"
	"github.com/rizkysiregar/marpokat/server/internal/user"
)

// Deps is what the WebSocket handler needs.
type Deps struct {
	Hub       *Hub
	Publisher *Publisher
	Presence  *Presence
	Tenants   auth.TenantResolver
	Users     auth.UserLookup
	UserSvc   *user.Service
	Messages  *message.Service
	Queries   store.Querier
	Options   Options
	Log       *slog.Logger
	Metrics   *metrics.Registry // may be nil
}

type Handler struct {
	hub      *Hub
	pub      *Publisher
	presence *Presence
	tenants  auth.TenantResolver
	users    auth.UserLookup
	userSvc  *user.Service
	msgs     *message.Service
	q        store.Querier
	opts     Options
	log      *slog.Logger
	metrics  *metrics.Registry
	now      func() time.Time
}

func NewHandler(d Deps) *Handler {
	return &Handler{
		hub: d.Hub, pub: d.Publisher, presence: d.Presence, tenants: d.Tenants, users: d.Users, userSvc: d.UserSvc,
		msgs: d.Messages, q: d.Queries, opts: d.Options, log: d.Log.With("subsystem", "websocket"),
		metrics: d.Metrics, now: time.Now,
	}
}

// Register adds GET /v1/ws, which authenticates in its first frame, and GET /v1/presence behind the user middleware.
func (h *Handler) Register(mux *http.ServeMux, userAuth func(http.Handler) http.Handler) {
	mux.HandleFunc("GET /v1/ws", h.serveWS)
	mux.Handle("GET /v1/presence", userAuth(http.HandlerFunc(h.presenceOf)))
}

func (h *Handler) serveWS(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// Credentials are sent in the auth frame, never in cookies, so a page of another origin has no ambient
		// authority to abuse: every origin may connect, as with CORS.
		InsecureSkipVerify: true,
		CompressionMode:    websocket.CompressionDisabled,
	})
	if err != nil {
		return // Accept has already answered
	}
	conn.SetReadLimit(h.opts.MaxFrameBytes)
	ctx, cancel := context.WithCancel(r.Context())
	newSession(h, conn, ctx, cancel).run()
}

// presenceOf answers who of the given users is online. Only users who share a channel with the caller are answered:
// presence is not a way to find out who is in an app.
func (h *Handler) presenceOf(w http.ResponseWriter, r *http.Request) {
	p := auth.FromContext(r.Context())
	var ids []string
	for _, id := range strings.Split(r.URL.Query().Get("user_ids"), ",") {
		if id = strings.TrimSpace(id); id != "" {
			if err := user.ValidateExternalID(id); err != nil {
				httpx.WriteValidation(w, r, httpx.FieldErrors{"user_ids": id + ": " + err.Error()})
				return
			}
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 || len(ids) > 100 {
		httpx.WriteValidation(w, r, httpx.FieldErrors{"user_ids": "must hold 1-100 comma separated external user ids"})
		return
	}
	rows, err := h.q.ListCoMembersByExternalIDs(r.Context(), store.ListCoMembersByExternalIDsParams{TenantID: p.Tenant.ID, UserID: p.UserID, Column3: ids})
	if err != nil {
		httpx.WriteErr(w, r, h.log, err)
		return
	}
	uuids := make([]uuid.UUID, 0, len(rows))
	byExt := make(map[string]uuid.UUID, len(rows))
	for _, row := range rows {
		uuids = append(uuids, row.ID)
		byExt[row.ExternalID] = row.ID
	}
	status, err := h.presence.Of(r.Context(), p.Tenant.ID, uuids)
	if err != nil {
		httpx.WriteErr(w, r, h.log, err)
		return
	}
	type entry struct {
		UserID     string     `json:"user_id"`
		Online     bool       `json:"online"`
		LastSeenAt *time.Time `json:"last_seen_at"`
	}
	out := make([]entry, 0, len(ids))
	for _, ext := range ids {
		if id, ok := byExt[ext]; ok {
			st := status[id]
			out = append(out, entry{UserID: ext, Online: st.Online, LastSeenAt: st.LastSeen})
		}
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"users": out})
}
