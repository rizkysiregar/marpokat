package message

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/google/uuid"

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
	mux.Handle("PATCH /v1/messages/{message_id}", user(http.HandlerFunc(h.edit)))
	mux.Handle("DELETE /v1/messages/{message_id}", user(http.HandlerFunc(h.delete)))
	mux.Handle("PUT /v1/messages/{message_id}/reactions/{emoji}", user(h.react(true)))
	mux.Handle("DELETE /v1/messages/{message_id}/reactions/{emoji}", user(h.react(false)))
	mux.Handle("POST /v1/server/channels/{channel_id}/messages", server(http.HandlerFunc(h.sendSystem)))
}

type sendBody struct {
	ClientMsgID   string          `json:"client_msg_id"`
	Text          string          `json:"text"`
	AttachmentIDs []string        `json:"attachment_ids"`
	ReplyToID     json.RawMessage `json:"reply_to_id"`
}

// ParseSendRefs reads the attachment ids and the reply id of a send request. WebSocket and REST share it.
func ParseSendRefs(attachmentIDs []string, replyToID json.RawMessage) ([]uuid.UUID, uuid.NullUUID, error) {
	errs := httpx.FieldErrors{}
	ids := make([]uuid.UUID, 0, len(attachmentIDs))
	for _, raw := range attachmentIDs {
		id, err := uuid.Parse(raw)
		if err != nil {
			errs["attachment_ids"] = fmt.Sprintf("%q is not an attachment id", raw)
			break
		}
		ids = append(ids, id)
	}
	var reply uuid.NullUUID
	if len(replyToID) > 0 && string(replyToID) != "null" {
		var raw string
		if err := json.Unmarshal(replyToID, &raw); err != nil {
			errs["reply_to_id"] = "must be a message id"
		} else if id, err := uuid.Parse(raw); err != nil {
			errs["reply_to_id"] = "must be a message id"
		} else {
			reply = uuid.NullUUID{UUID: id, Valid: true}
		}
	}
	return ids, reply, errs.OrNil()
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
	attachmentIDs, replyTo, err := ParseSendRefs(body.AttachmentIDs, body.ReplyToID)
	if err != nil {
		httpx.WriteErr(w, r, h.log, err)
		return
	}
	msg, created, err := h.svc.Send(r.Context(), SendInput{
		TenantID: p.Tenant.ID, ChannelID: channelID, SenderID: p.UserID, ClientMsgID: body.ClientMsgID, Text: body.Text,
		AttachmentIDs: attachmentIDs, ReplyToID: replyTo,
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
	if _, err := h.svc.MarkRead(r.Context(), p.Tenant.ID, channelID, p.UserID, *body.Seq, ""); err != nil {
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

func (h *Handler) edit(w http.ResponseWriter, r *http.Request) {
	p := auth.FromContext(r.Context())
	id, err := httpx.PathUUID(r, "message_id")
	if err != nil {
		httpx.WriteErr(w, r, h.log, err)
		return
	}
	var body struct {
		Text string `json:"text"`
	}
	if !httpx.DecodeJSON(w, r, &body) {
		return
	}
	msg, err := h.svc.Edit(r.Context(), p.Tenant.ID, p.UserID, id, body.Text)
	if err != nil {
		httpx.WriteErr(w, r, h.log, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, msg)
}

func (h *Handler) delete(w http.ResponseWriter, r *http.Request) {
	p := auth.FromContext(r.Context())
	id, err := httpx.PathUUID(r, "message_id")
	if err != nil {
		httpx.WriteErr(w, r, h.log, err)
		return
	}
	if err := h.svc.Delete(r.Context(), p.Tenant.ID, p.UserID, id); err != nil {
		httpx.WriteErr(w, r, h.log, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) react(add bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := auth.FromContext(r.Context())
		id, err := httpx.PathUUID(r, "message_id")
		if err != nil {
			httpx.WriteErr(w, r, h.log, err)
			return
		}
		var msg Message
		if add {
			msg, err = h.svc.React(r.Context(), p.Tenant.ID, p.UserID, id, r.PathValue("emoji"))
		} else {
			msg, err = h.svc.Unreact(r.Context(), p.Tenant.ID, p.UserID, id, r.PathValue("emoji"))
		}
		if err != nil {
			httpx.WriteErr(w, r, h.log, err)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, msg)
	})
}
