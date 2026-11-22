package user

import (
	"errors"
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

// Register adds the routes. user and server wrap a handler with the matching authentication.
func (h *Handler) Register(mux *http.ServeMux, user, server func(http.Handler) http.Handler) {
	mux.Handle("GET /v1/me", user(http.HandlerFunc(h.me)))
	mux.Handle("PUT /v1/server/users/{external_id}", server(http.HandlerFunc(h.upsert)))
	mux.Handle("DELETE /v1/server/users/{external_id}", server(http.HandlerFunc(h.delete)))
}

func (h *Handler) me(w http.ResponseWriter, r *http.Request) {
	p := auth.FromContext(r.Context())
	u, err := h.svc.Get(r.Context(), p.Tenant.ID, p.ExternalID)
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, u)
}

func (h *Handler) upsert(w http.ResponseWriter, r *http.Request) {
	p := auth.FromContext(r.Context())
	var in UpsertInput
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	u, err := h.svc.Upsert(r.Context(), p.Tenant.ID, r.PathValue("external_id"), in)
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, u)
}

func (h *Handler) delete(w http.ResponseWriter, r *http.Request) {
	p := auth.FromContext(r.Context())
	ext := r.PathValue("external_id")
	if err := ValidateExternalID(ext); err != nil {
		httpx.WriteValidation(w, r, httpx.FieldErrors{"external_id": err.Error()})
		return
	}
	if err := h.svc.Delete(r.Context(), p.Tenant.ID, ext); err != nil {
		h.writeErr(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) writeErr(w http.ResponseWriter, r *http.Request, err error) {
	var fields httpx.FieldErrors
	switch {
	case errors.As(err, &fields):
		httpx.WriteValidation(w, r, fields)
	case errors.Is(err, ErrNotFound):
		httpx.WriteError(w, r, http.StatusNotFound, httpx.CodeNotFound, "user not found")
	default:
		h.log.Error("user request failed", "error", err, "request_id", httpx.RequestID(r.Context()))
		httpx.WriteError(w, r, http.StatusInternalServerError, httpx.CodeInternal, "internal error")
	}
}
