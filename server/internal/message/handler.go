package message

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/rizkysiregar/marpokat/server/internal/auth"
	"github.com/rizkysiregar/marpokat/server/internal/platform/httpx"
)

type Handler struct {
	svc *Service
	log *slog.Logger
}

func NewHandler(svc *Service, log *slog.Logger) *Handler { return &Handler{svc: svc, log: log} }

func (h *Handler) Register(mux *http.ServeMux, user, server func(http.Handler) http.Handler) {
	mux.Handle("GET /v1/channels/{channel_id}/messages", user(http.HandlerFunc(h.history)))
	mux.Handle("POST /v1/channels/{channel_id}/messages", user(http.HandlerFunc(h.send)))
	mux.Handle("GET /v1/channels/{channel_id}/sync", user(http.HandlerFunc(h.sync)))
	mux.Handle("POST /v1/channels/{channel_id}/read", user(http.HandlerFunc(h.read)))
	mux.Handle("POST /v1/server/channels/{channel_id}/messages", server(http.HandlerFunc(h.sendSystem)))
}

type sendBody struct {
	ClientMsgID   string          `json:"client_msg_id"`
	Text          string          `json:"text"`
	AttachmentIDs []string        `json:"attachment_ids"`
	ReplyToID     json.RawMessage `json:"reply_to_id"`
}

func (h *Handler) send(w http.ResponseWriter, r *http.Request) {
	p := auth.FromContext(r.Context())
	channelID, err := httpx.PathUUID(r, "channel_id")
	if err != nil {
		httpx.WriteErr(w, r, h.log, err)
		return
	}
	var body sendBody
	if !httpx.DecodeJSON(w, r, &body) {
		return
	}
	// attachments and replies arrive with M5; refuse them instead of silently dropping what the client meant to send
	if len(body.AttachmentIDs) > 0 {
		httpx.WriteValidation(w, r, httpx.FieldErrors{"attachment_ids": "attachments are not supported yet"})
		return
	}
	if len(body.ReplyToID) > 0 && string(body.ReplyToID) != "null" {
		httpx.WriteValidation(w, r, httpx.FieldErrors{"reply_to_id": "replies are not supported yet"})
		return
	}
	msg, created, err := h.svc.Send(r.Context(), SendInput{
		TenantID: p.Tenant.ID, ChannelID: channelID, SenderID: p.UserID, ClientMsgID: body.ClientMsgID, Text: body.Text,
	})
	if err != nil {
		httpx.WriteErr(w, r, h.log, err)
		return
	}
	status := http.StatusCreated
	if !created {
		status = http.StatusOK
	}
	httpx.WriteJSON(w, status, msg)
}

func (h *Handler) history(w http.ResponseWriter, r *http.Request) {
	p := auth.FromContext(r.Context())
	channelID, err := httpx.PathUUID(r, "channel_id")
	if err != nil {
		httpx.WriteErr(w, r, h.log, err)
		return
	}
	limit, err := httpx.QueryLimit(r, DefaultPageSize, MaxPageSize)
	if err != nil {
		httpx.WriteErr(w, r, h.log, err)
		return
	}
	before, _, err := httpx.QueryInt64(r, "before_seq", 1)
	if err != nil {
		httpx.WriteErr(w, r, h.log, err)
		return
	}
	msgs, hasMore, err := h.svc.History(r.Context(), p.Tenant.ID, channelID, p.UserID, before, limit)
	if err != nil {
		httpx.WriteErr(w, r, h.log, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"messages": msgs, "has_more": hasMore})
}

func (h *Handler) sync(w http.ResponseWriter, r *http.Request) {
	p := auth.FromContext(r.Context())
	channelID, err := httpx.PathUUID(r, "channel_id")
	if err != nil {
		httpx.WriteErr(w, r, h.log, err)
		return
	}
	limit, err := httpx.QueryLimit(r, DefaultPageSize, MaxPageSize)
	if err != nil {
		httpx.WriteErr(w, r, h.log, err)
		return
	}
	since, ok, err := httpx.QueryInt64(r, "since_rev", 0)
	if err == nil && !ok {
		err = httpx.FieldErrors{"since_rev": "is required"}
	}
	if err != nil {
		httpx.WriteErr(w, r, h.log, err)
		return
	}
	res, err := h.svc.Sync(r.Context(), p.Tenant.ID, channelID, p.UserID, since, limit)
	if err != nil {
		httpx.WriteErr(w, r, h.log, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, res)
}

func (h *Handler) read(w http.ResponseWriter, r *http.Request) {
	p := auth.FromContext(r.Context())
	channelID, err := httpx.PathUUID(r, "channel_id")
	if err != nil {
		httpx.WriteErr(w, r, h.log, err)
		return
	}
	var body struct {
		Seq *int64 `json:"seq"`
	}
	if !httpx.DecodeJSON(w, r, &body) {
		return
	}
	if body.Seq == nil {
		httpx.WriteValidation(w, r, httpx.FieldErrors{"seq": "is required"})
		return
	}
	if _, err := h.svc.MarkRead(r.Context(), p.Tenant.ID, channelID, p.UserID, *body.Seq); err != nil {
		httpx.WriteErr(w, r, h.log, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) sendSystem(w http.ResponseWriter, r *http.Request) {
	p := auth.FromContext(r.Context())
	channelID, err := httpx.PathUUID(r, "channel_id")
	if err != nil {
		httpx.WriteErr(w, r, h.log, err)
		return
	}
	var body struct {
		Text     string         `json:"text"`
		Metadata map[string]any `json:"metadata"`
	}
	if !httpx.DecodeJSON(w, r, &body) {
		return
	}
	msg, err := h.svc.SendSystem(r.Context(), p.Tenant.ID, channelID, body.Text, body.Metadata)
	if err != nil {
		httpx.WriteErr(w, r, h.log, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, msg)
}
