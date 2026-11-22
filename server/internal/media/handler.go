package media

import (
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

func (h *Handler) Register(mux *http.ServeMux, user func(http.Handler) http.Handler) {
	mux.Handle("POST /v1/attachments", user(http.HandlerFunc(h.create)))
	mux.Handle("POST /v1/attachments/{attachment_id}/complete", user(http.HandlerFunc(h.complete)))
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	p := auth.FromContext(r.Context())
	var body struct {
		Mime     string `json:"mime"`
		Size     int64  `json:"size"`
		FileName string `json:"file_name"`
	}
	if !httpx.DecodeJSON(w, r, &body) {
		return
	}
	up, err := h.svc.Create(r.Context(), p.Tenant.ID, p.UserID, body.Mime, body.Size, body.FileName)
	if err != nil {
		httpx.WriteErr(w, r, h.log, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, up)
}

func (h *Handler) complete(w http.ResponseWriter, r *http.Request) {
	p := auth.FromContext(r.Context())
	id, err := uuid.Parse(r.PathValue("attachment_id"))
	if err != nil {
		httpx.WriteErr(w, r, h.log, httpx.ErrNotFound("attachment not found"))
		return
	}
	a, err := h.svc.Complete(r.Context(), p.Tenant.ID, p.UserID, id)
	if err != nil {
		httpx.WriteErr(w, r, h.log, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, a)
}
