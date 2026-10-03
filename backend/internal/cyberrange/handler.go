package cyberrange

import (
	"log/slog"
	"net/http"
	"regexp"

	"github.com/google/uuid"

	"github.com/Darkload9999/VORTECH/backend/internal/auth"
	"github.com/Darkload9999/VORTECH/backend/internal/career"
	"github.com/Darkload9999/VORTECH/backend/internal/httpx"
)

// Handler serves the range API.
type Handler struct {
	svc *Service
	log *slog.Logger
}

// NewHandler returns a Handler.
func NewHandler(svc *Service, log *slog.Logger) *Handler {
	return &Handler{svc: svc, log: log}
}

var templateSlug = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,40}$`)

// CreateRequest is the body of POST /api/v1/ranges.
type CreateRequest struct {
	Template string `json:"template"`
}

// Templates serves GET /api/v1/range-templates.
func (h *Handler) Templates(w http.ResponseWriter, r *http.Request) {
	out, err := h.svc.Templates(r.Context())
	if err != nil {
		httpx.Fail(w, r, h.log, err)
		return
	}
	httpx.JSON(w, http.StatusOK, out)
}

// Create serves POST /api/v1/ranges. Provisioning is asynchronous: the
// response is 202 with the REQUESTED range; progress arrives as range.*
// events on the player's WebSocket topic and via GET.
func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.FromContext(r.Context())
	if !ok || p.PlayerID == nil {
		httpx.WriteError(w, r, http.StatusForbidden, career.CodeNoPlayerProfile, "This account has no player profile.")
		return
	}
	var req CreateRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Fail(w, r, h.log, err)
		return
	}
	if !templateSlug.MatchString(req.Template) {
		httpx.WriteError(w, r, http.StatusBadRequest, httpx.CodeBadRequest, "template must be a range template slug.")
		return
	}
	v, err := h.svc.Create(r.Context(), p, req.Template)
	if err != nil {
		httpx.Fail(w, r, h.log, err)
		return
	}
	w.Header().Set("Location", "/api/v1/ranges/"+v.ID.String())
	httpx.JSON(w, http.StatusAccepted, v)
}

// List serves GET /api/v1/ranges?limit=&cursor=.
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.FromContext(r.Context())
	if !ok || p.PlayerID == nil {
		httpx.WriteError(w, r, http.StatusForbidden, career.CodeNoPlayerProfile, "This account has no player profile.")
		return
	}
	page, err := httpx.ParsePage(r, 20, 100)
	if err != nil {
		httpx.Fail(w, r, h.log, err)
		return
	}
	var before *uuid.UUID
	if page.After != "" {
		id, err := uuid.Parse(page.After)
		if err != nil {
			httpx.WriteError(w, r, http.StatusBadRequest, httpx.CodeInvalidQuery, "cursor is invalid.")
			return
		}
		before = &id
	}
	rows, err := h.svc.List(r.Context(), *p.PlayerID, before, page.FetchLimit())
	if err != nil {
		httpx.Fail(w, r, h.log, err)
		return
	}
	rows, meta := httpx.Paginate(page, rows, func(v View) string { return v.ID.String() })
	httpx.JSONWithMeta(w, http.StatusOK, rows, meta)
}

// Get serves GET /api/v1/ranges/{id}.
func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	id, err := httpx.PathUUID(r, "id")
	if err != nil {
		httpx.Fail(w, r, h.log, err)
		return
	}
	v, err := h.svc.Get(r.Context(), p, id)
	if err != nil {
		httpx.Fail(w, r, h.log, err)
		return
	}
	httpx.JSON(w, http.StatusOK, v)
}

// Destroy serves DELETE /api/v1/ranges/{id}: 202 while teardown runs.
func (h *Handler) Destroy(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	id, err := httpx.PathUUID(r, "id")
	if err != nil {
		httpx.Fail(w, r, h.log, err)
		return
	}
	v, err := h.svc.Destroy(r.Context(), p, id)
	if err != nil {
		httpx.Fail(w, r, h.log, err)
		return
	}
	httpx.JSON(w, http.StatusAccepted, v)
}
