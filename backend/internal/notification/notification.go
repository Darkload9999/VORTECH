package notification

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Darkload9999/VORTECH/backend/internal/auth"
	"github.com/Darkload9999/VORTECH/backend/internal/database/db"
	"github.com/Darkload9999/VORTECH/backend/internal/event"
	"github.com/Darkload9999/VORTECH/backend/internal/httpx"
)

// Notification types.
const (
	TypePromoted     = "career.promoted"
	TypeZoneUnlocked = "zone.unlocked"
)

// Notification is a durable message for one user.
type Notification struct {
	ID        uuid.UUID       `json:"id"`
	Type      string          `json:"type"`
	Title     string          `json:"title"`
	Body      string          `json:"body"`
	Data      json.RawMessage `json:"data"`
	ReadAt    *time.Time      `json:"read_at"`
	CreatedAt time.Time       `json:"created_at"`
}

// Create stores a notification with q (usually bound to the transaction
// that caused it) and returns the event to publish once that transaction
// commits. topic is the recipient's private topic, or "" for users
// without a player profile (they read notifications over REST).
func Create(ctx context.Context, q *db.Queries, userID uuid.UUID, topic, typ, title, body string, data any) (Notification, event.Event, error) {
	if data == nil {
		data = map[string]any{}
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return Notification{}, event.Event{}, fmt.Errorf("encode notification data: %w", err)
	}
	row, err := q.InsertNotification(ctx, db.InsertNotificationParams{UserID: userID, Type: typ, Title: title, Body: body, Data: raw})
	if err != nil {
		return Notification{}, event.Event{}, fmt.Errorf("insert notification: %w", err)
	}
	n := Notification{ID: row.ID, Type: row.Type, Title: row.Title, Body: row.Body, Data: row.Data, ReadAt: row.ReadAt, CreatedAt: row.CreatedAt.UTC()}
	e, err := event.New(event.NotificationNew, topic, n)
	if err != nil {
		return Notification{}, event.Event{}, err
	}
	return n, e, nil
}

// Handler serves the caller's notifications.
type Handler struct {
	q   *db.Queries
	log *slog.Logger
}

// NewHandler returns a Handler.
func NewHandler(q *db.Queries, log *slog.Logger) *Handler {
	return &Handler{q: q, log: log}
}

// ListMeta extends page metadata with the unread count.
type ListMeta struct {
	httpx.PageMeta
	Unread int32 `json:"unread"`
}

// List serves GET /api/v1/notifications?unread=true&limit=&cursor=.
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	page, err := httpx.ParsePage(r, 50, 100)
	if err != nil {
		httpx.Fail(w, r, h.log, err)
		return
	}
	params := db.ListNotificationsParams{UserID: p.UserID, UnreadOnly: r.URL.Query().Get("unread") == "true", PageSize: page.FetchLimit()}
	if page.After != "" {
		id, err := uuid.Parse(page.After)
		if err != nil {
			httpx.WriteError(w, r, http.StatusBadRequest, httpx.CodeInvalidQuery, "cursor is invalid.")
			return
		}
		params.BeforeID = &id
	}
	rows, err := h.q.ListNotifications(r.Context(), params)
	if err != nil {
		httpx.Fail(w, r, h.log, err)
		return
	}
	unread, err := h.q.CountUnreadNotifications(r.Context(), p.UserID)
	if err != nil {
		httpx.Fail(w, r, h.log, err)
		return
	}
	rows, meta := httpx.Paginate(page, rows, func(n db.ListNotificationsRow) string { return n.ID.String() })
	out := make([]Notification, len(rows))
	for i, n := range rows {
		out[i] = Notification{ID: n.ID, Type: n.Type, Title: n.Title, Body: n.Body, Data: n.Data, ReadAt: n.ReadAt, CreatedAt: n.CreatedAt.UTC()}
	}
	httpx.JSONWithMeta(w, http.StatusOK, out, ListMeta{PageMeta: meta, Unread: unread})
}

// MarkRead serves POST /api/v1/notifications/{id}/read. Unknown IDs and
// other users' notifications are indistinguishable (404).
func (h *Handler) MarkRead(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	id, err := httpx.PathUUID(r, "id")
	if err != nil {
		httpx.Fail(w, r, h.log, err)
		return
	}
	row, err := h.q.MarkNotificationRead(r.Context(), db.MarkNotificationReadParams{ID: id, UserID: p.UserID})
	if errors.Is(err, pgx.ErrNoRows) {
		httpx.WriteError(w, r, http.StatusNotFound, httpx.CodeNotFound, "Notification not found.")
		return
	}
	if err != nil {
		httpx.Fail(w, r, h.log, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"id": row.ID, "read_at": row.ReadAt})
}

// MarkAllRead serves POST /api/v1/notifications/read-all.
func (h *Handler) MarkAllRead(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	n, err := h.q.MarkAllNotificationsRead(r.Context(), p.UserID)
	if err != nil {
		httpx.Fail(w, r, h.log, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"marked": n})
}
