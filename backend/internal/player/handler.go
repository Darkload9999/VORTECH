package player

import (
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Darkload9999/VORTECH/backend/internal/auth"
	"github.com/Darkload9999/VORTECH/backend/internal/database/db"
	"github.com/Darkload9999/VORTECH/backend/internal/httpx"
)

// Handler serves player-facing profile endpoints.
type Handler struct {
	q   *db.Queries
	log *slog.Logger
}

// NewHandler returns a Handler.
func NewHandler(q *db.Queries, log *slog.Logger) *Handler {
	return &Handler{q: q, log: log}
}

// MeResponse is the body of GET /api/v1/me.
type MeResponse struct {
	User        UserView          `json:"user"`
	Roles       []auth.Role       `json:"roles"`
	Permissions []auth.Permission `json:"permissions"`
	Player      *PlayerView       `json:"player"`
	Session     SessionView       `json:"session"`
}

// UserView is the caller's platform account.
type UserView struct {
	ID            uuid.UUID `json:"id"`
	Username      string    `json:"username"`
	Email         *string   `json:"email"`
	EmailVerified bool      `json:"email_verified"`
	FullName      *string   `json:"full_name"`
	Status        string    `json:"status"`
	CreatedAt     time.Time `json:"created_at"`
}

// PlayerView is the caller's game profile.
type PlayerView struct {
	ID          uuid.UUID `json:"id"`
	DisplayName string    `json:"display_name"`
	CreatedAt   time.Time `json:"created_at"`
}

// SessionView describes the presented access token. Roles and permissions
// are informational for UI gating; the server re-checks every request.
type SessionView struct {
	ExpiresAt time.Time `json:"expires_at"`
}

// Me returns the authenticated caller's account, roles and player profile.
// Timestamps are always rendered in UTC regardless of the host time zone.
func (h *Handler) Me(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.FromContext(r.Context())
	if !ok {
		// Unreachable when mounted behind Authenticator.Protect.
		httpx.WriteError(w, r, http.StatusUnauthorized, httpx.CodeUnauthorized, "Authentication is required.")
		return
	}

	row, err := h.q.GetUserProfile(r.Context(), p.UserID)
	if errors.Is(err, pgx.ErrNoRows) {
		httpx.WriteError(w, r, http.StatusNotFound, httpx.CodeNotFound, "User not found.")
		return
	}
	if err != nil {
		httpx.Fail(w, r, h.log, err)
		return
	}

	resp := MeResponse{
		User: UserView{
			ID:            row.ID,
			Username:      row.Username,
			Email:         row.Email,
			EmailVerified: row.EmailVerified,
			FullName:      row.FullName,
			Status:        row.Status,
			CreatedAt:     row.CreatedAt.UTC(),
		},
		Roles:       nonNil(p.Roles.Roles()),
		Permissions: nonNil(p.Roles.Permissions()),
		Session:     SessionView{ExpiresAt: p.ExpiresAt.UTC()},
	}
	// The player profile is exposed only while the caller holds the PLAYER
	// role, even if a profile was created earlier.
	if p.PlayerID != nil && row.PlayerID != nil {
		resp.Player = &PlayerView{
			ID:          *row.PlayerID,
			DisplayName: deref(row.PlayerDisplayName),
			CreatedAt:   derefTime(row.PlayerCreatedAt).UTC(),
		}
	}
	httpx.JSON(w, http.StatusOK, resp)
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func derefTime(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}
