package channel

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/rizkysiregar/marpokat/server/internal/auth"
	"github.com/rizkysiregar/marpokat/server/internal/platform/httpx"
	"github.com/rizkysiregar/marpokat/server/internal/store"
)

type Handler struct {
	svc *Service
	log *slog.Logger
}

func NewHandler(svc *Service, log *slog.Logger) *Handler { return &Handler{svc: svc, log: log} }

// Register adds the client routes (behind user) and the server routes (behind server).
func (h *Handler) Register(mux *http.ServeMux, user, server func(http.Handler) http.Handler) {
	mux.Handle("GET /v1/channels", user(http.HandlerFunc(h.list)))
	mux.Handle("POST /v1/channels", user(http.HandlerFunc(h.create)))
	mux.Handle("GET /v1/channels/{channel_id}", user(http.HandlerFunc(h.get)))
	mux.Handle("PATCH /v1/channels/{channel_id}", user(http.HandlerFunc(h.update)))
	mux.Handle("GET /v1/channels/{channel_id}/members", user(http.HandlerFunc(h.members)))
	mux.Handle("POST /v1/channels/{channel_id}/members", user(http.HandlerFunc(h.addMembers)))
	mux.Handle("DELETE /v1/channels/{channel_id}/members/{user_id}", user(http.HandlerFunc(h.removeMember)))

	mux.Handle("POST /v1/server/channels/dm", server(http.HandlerFunc(h.serverDM)))
	mux.Handle("POST /v1/server/channels", server(http.HandlerFunc(h.serverCreateGroup)))
	mux.Handle("POST /v1/server/channels/{channel_id}/members", server(http.HandlerFunc(h.serverAddMembers)))
	mux.Handle("DELETE /v1/server/channels/{channel_id}/members/{external_id}", server(http.HandlerFunc(h.serverRemoveMember)))
	mux.Handle("POST /v1/server/channels/{channel_id}/freeze", server(h.serverFreeze(true)))
	mux.Handle("DELETE /v1/server/channels/{channel_id}/freeze", server(h.serverFreeze(false)))
}

func (h *Handler) fail(w http.ResponseWriter, r *http.Request, err error) {
	httpx.WriteErr(w, r, h.log, err)
}

// ---------------------------------------------------------------- client API

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	p := auth.FromContext(r.Context())
	limit, err := httpx.QueryLimit(r, DefaultPageSize, MaxPageSize)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	params := ListParams{Cursor: r.URL.Query().Get("cursor"), Limit: limit}
	if raw := r.URL.Query().Get("updated_since"); raw != "" {
		t, err := time.Parse(time.RFC3339Nano, raw)
		if err != nil {
			h.fail(w, r, httpx.FieldErrors{"updated_since": "must be an RFC 3339 timestamp"})
			return
		}
		params.Since = &t
	}
	res, err := h.svc.ListForUser(r.Context(), p.Tenant.ID, p.UserID, params)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, res)
}

type createBody struct {
	Type      string   `json:"type"`
	UserID    string   `json:"user_id"`
	Name      string   `json:"name"`
	AvatarURL *string  `json:"avatar_url"`
	MemberIDs []string `json:"member_ids"`
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	p := auth.FromContext(r.Context())
	var body createBody
	if !httpx.DecodeJSON(w, r, &body) {
		return
	}
	ctx := r.Context()
	switch body.Type {
	case TypeDM:
		if !p.Tenant.Config.ClientCanCreateDM {
			h.fail(w, r, httpx.ErrForbidden("this app does not let clients create direct messages; its backend creates them"))
			return
		}
		peers, err := h.svc.ResolveUsers(ctx, p.Tenant.ID, []string{body.UserID})
		if err != nil || len(peers) != 1 {
			h.fail(w, r, orFields(err, httpx.FieldErrors{"user_id": "is required"}))
			return
		}
		ch, created, err := h.svc.GetOrCreateDM(ctx, p.Tenant.ID, p.UserID, peers[0].ID)
		if err != nil {
			h.fail(w, r, err)
			return
		}
		h.writeChannel(w, r, p, ch.ID, created)
	case TypeGroup:
		if !p.Tenant.Config.ClientCanCreateGroup {
			h.fail(w, r, httpx.ErrForbidden("this app does not let clients create groups; its backend creates them"))
			return
		}
		members, err := h.svc.ResolveUsers(ctx, p.Tenant.ID, body.MemberIDs)
		if err != nil {
			h.fail(w, r, err)
			return
		}
		ch, err := h.svc.CreateGroup(ctx, p.Tenant.ID, p.UserID, body.Name, body.AvatarURL, userIDs(members))
		if err != nil {
			h.fail(w, r, err)
			return
		}
		h.writeChannel(w, r, p, ch.ID, true)
	default:
		h.fail(w, r, httpx.FieldErrors{"type": `must be "dm" or "group"`})
	}
}

// writeChannel answers with the channel as the caller sees it.
func (h *Handler) writeChannel(w http.ResponseWriter, r *http.Request, p auth.Principal, id uuid.UUID, created bool) {
	ch, err := h.svc.ForUser(r.Context(), p.Tenant.ID, p.UserID, id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	httpx.WriteJSON(w, status, ch)
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	p := auth.FromContext(r.Context())
	id, err := httpx.PathUUID(r, "channel_id")
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.writeChannel(w, r, p, id, false)
}

func (h *Handler) update(w http.ResponseWriter, r *http.Request) {
	p := auth.FromContext(r.Context())
	id, err := httpx.PathUUID(r, "channel_id")
	if err != nil {
		h.fail(w, r, err)
		return
	}
	var body struct {
		Name      *string         `json:"name"`
		AvatarURL json.RawMessage `json:"avatar_url"` // absent: unchanged, null: removed
	}
	if !httpx.DecodeJSON(w, r, &body) {
		return
	}
	in := UpdateInput{Name: body.Name}
	if body.AvatarURL != nil {
		in.SetAvatar = true
		if string(body.AvatarURL) != "null" {
			var s string
			if err := json.Unmarshal(body.AvatarURL, &s); err != nil {
				h.fail(w, r, httpx.FieldErrors{"avatar_url": "must be a URL or null"})
				return
			}
			in.AvatarURL = &s
		}
	}
	if in.Name == nil && !in.SetAvatar {
		h.fail(w, r, httpx.FieldErrors{"name": "send name and/or avatar_url"})
		return
	}
	ch, err := h.svc.Update(r.Context(), p.Tenant.ID, p.UserID, id, in)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, ch)
}

func (h *Handler) members(w http.ResponseWriter, r *http.Request) {
	p := auth.FromContext(r.Context())
	id, err := httpx.PathUUID(r, "channel_id")
	if err != nil {
		h.fail(w, r, err)
		return
	}
	limit, err := httpx.QueryLimit(r, DefaultPageSize, MaxPageSize)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	res, err := h.svc.Members(r.Context(), p.Tenant.ID, p.UserID, id, r.URL.Query().Get("cursor"), limit)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, res)
}

type userIDsBody struct {
	UserIDs []string `json:"user_ids"`
}

func (b userIDsBody) validate() error {
	if len(b.UserIDs) == 0 || len(b.UserIDs) > 50 {
		return httpx.FieldErrors{"user_ids": "must hold 1-50 external user ids"}
	}
	return nil
}

func (h *Handler) addMembers(w http.ResponseWriter, r *http.Request) {
	p := auth.FromContext(r.Context())
	id, err := httpx.PathUUID(r, "channel_id")
	if err != nil {
		h.fail(w, r, err)
		return
	}
	var body userIDsBody
	if !httpx.DecodeJSON(w, r, &body) {
		return
	}
	if err := body.validate(); err != nil {
		h.fail(w, r, err)
		return
	}
	users, err := h.svc.ResolveUsers(r.Context(), p.Tenant.ID, body.UserIDs)
	if err == nil {
		err = h.svc.AddMembers(r.Context(), p.Tenant.ID, UserActor(p.UserID), id, users)
	}
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.writeChannel(w, r, p, id, false)
}

func (h *Handler) removeMember(w http.ResponseWriter, r *http.Request) {
	p := auth.FromContext(r.Context())
	id, err := httpx.PathUUID(r, "channel_id")
	if err != nil {
		h.fail(w, r, err)
		return
	}
	target := p.UserID
	if ext := r.PathValue("user_id"); ext != "me" && ext != p.ExternalID {
		users, err := h.svc.ResolveUsers(r.Context(), p.Tenant.ID, []string{ext})
		if err != nil || len(users) != 1 {
			h.fail(w, r, httpx.ErrNotFound("member not found"))
			return
		}
		target = users[0].ID
	}
	if err := h.svc.RemoveMember(r.Context(), p.Tenant.ID, UserActor(p.UserID), id, target); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------- server API

func (h *Handler) serverDM(w http.ResponseWriter, r *http.Request) {
	p := auth.FromContext(r.Context())
	var body struct {
		UserA string `json:"user_a"`
		UserB string `json:"user_b"`
	}
	if !httpx.DecodeJSON(w, r, &body) {
		return
	}
	if body.UserA == "" || body.UserB == "" {
		h.fail(w, r, httpx.FieldErrors{"user_a": "user_a and user_b are required"})
		return
	}
	if body.UserA == body.UserB {
		h.fail(w, r, httpx.FieldErrors{"user_b": "cannot start a DM with yourself"})
		return
	}
	users, err := h.svc.ResolveUsers(r.Context(), p.Tenant.ID, []string{body.UserA, body.UserB})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	ch, created, err := h.svc.GetOrCreateDM(r.Context(), p.Tenant.ID, users[0].ID, users[1].ID)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.writeServerChannel(w, r, p, ch.ID, created)
}

func (h *Handler) serverCreateGroup(w http.ResponseWriter, r *http.Request) {
	p := auth.FromContext(r.Context())
	var body struct {
		CreatedBy string   `json:"created_by"`
		Name      string   `json:"name"`
		AvatarURL *string  `json:"avatar_url"`
		MemberIDs []string `json:"member_ids"`
	}
	if !httpx.DecodeJSON(w, r, &body) {
		return
	}
	if body.CreatedBy == "" {
		h.fail(w, r, httpx.FieldErrors{"created_by": "is required"})
		return
	}
	users, err := h.svc.ResolveUsers(r.Context(), p.Tenant.ID, append([]string{body.CreatedBy}, body.MemberIDs...))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	ids := userIDs(users) // the creator is first
	ch, err := h.svc.CreateGroup(r.Context(), p.Tenant.ID, ids[0], body.Name, body.AvatarURL, ids[1:])
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.writeServerChannel(w, r, p, ch.ID, true)
}

func (h *Handler) serverAddMembers(w http.ResponseWriter, r *http.Request) {
	p := auth.FromContext(r.Context())
	id, err := httpx.PathUUID(r, "channel_id")
	if err != nil {
		h.fail(w, r, err)
		return
	}
	var body userIDsBody
	if !httpx.DecodeJSON(w, r, &body) {
		return
	}
	if err := body.validate(); err != nil {
		h.fail(w, r, err)
		return
	}
	users, err := h.svc.ResolveUsers(r.Context(), p.Tenant.ID, body.UserIDs)
	if err == nil {
		err = h.svc.AddMembers(r.Context(), p.Tenant.ID, Actor{}, id, users)
	}
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.writeServerChannel(w, r, p, id, false)
}

func (h *Handler) serverRemoveMember(w http.ResponseWriter, r *http.Request) {
	p := auth.FromContext(r.Context())
	id, err := httpx.PathUUID(r, "channel_id")
	if err != nil {
		h.fail(w, r, err)
		return
	}
	users, err := h.svc.ResolveUsers(r.Context(), p.Tenant.ID, []string{r.PathValue("external_id")})
	if err != nil || len(users) != 1 {
		h.fail(w, r, httpx.ErrNotFound("member not found"))
		return
	}
	if err := h.svc.RemoveMember(r.Context(), p.Tenant.ID, Actor{}, id, users[0].ID); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) serverFreeze(frozen bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := auth.FromContext(r.Context())
		id, err := httpx.PathUUID(r, "channel_id")
		if err != nil {
			h.fail(w, r, err)
			return
		}
		if err := h.svc.SetFrozen(r.Context(), p.Tenant.ID, id, frozen); err != nil {
			h.fail(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}

func (h *Handler) writeServerChannel(w http.ResponseWriter, r *http.Request, p auth.Principal, id uuid.UUID, created bool) {
	ch, err := h.svc.ServerView(r.Context(), p.Tenant.ID, id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	httpx.WriteJSON(w, status, ch)
}

func userIDs(users []store.User) []uuid.UUID {
	out := make([]uuid.UUID, 0, len(users))
	for _, u := range users {
		out = append(out, u.ID)
	}
	return out
}

// orFields returns err, or the fallback when err is nil.
func orFields(err error, fallback error) error {
	if err != nil {
		return err
	}
	return fallback
}
