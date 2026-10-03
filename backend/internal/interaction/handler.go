package interaction

import (
	"log/slog"
	"net/http"

	"github.com/Darkload9999/VORTECH/backend/internal/auth"
	"github.com/Darkload9999/VORTECH/backend/internal/career"
	"github.com/Darkload9999/VORTECH/backend/internal/httpx"
)

// Handler serves POST /api/v1/interactions.
type Handler struct {
	svc     *Service
	limiter *Limiter
	log     *slog.Logger
}

// NewHandler returns a Handler; limiter may be nil to disable rate limiting.
func NewHandler(svc *Service, limiter *Limiter, log *slog.Logger) *Handler {
	return &Handler{svc: svc, limiter: limiter, log: log}
}

// Create serves POST /api/v1/interactions.
func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.FromContext(r.Context())
	if !ok || p.PlayerID == nil {
		httpx.WriteError(w, r, http.StatusForbidden, career.CodeNoPlayerProfile, "This account has no player profile.")
		return
	}
	if h.limiter != nil && !h.limiter.Allow(*p.PlayerID) {
		w.Header().Set("Retry-After", "1")
		httpx.WriteError(w, r, http.StatusTooManyRequests, httpx.CodeRateLimited, "Too many interactions; slow down.")
		return
	}

	var req Request
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Fail(w, r, h.log, err)
		return
	}
	out, err := h.svc.Process(r.Context(), *p.PlayerID, req)
	if err != nil {
		httpx.Fail(w, r, h.log, err)
		return
	}
	httpx.JSON(w, http.StatusOK, out)
}
