package world

import (
	"log/slog"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/Darkload9999/VORTECH/backend/internal/audit"
	"github.com/Darkload9999/VORTECH/backend/internal/auth"
	"github.com/Darkload9999/VORTECH/backend/internal/company"
	"github.com/Darkload9999/VORTECH/backend/internal/event"
	"github.com/Darkload9999/VORTECH/backend/internal/httpx"
)

// Announcer broadcasts administrator announcements to everyone in the
// active world (the world topic).
type Announcer struct {
	dir   *company.Directory
	pub   event.Publisher
	audit auth.AuditRecorder
	log   *slog.Logger
}

// NewAnnouncer returns an Announcer.
func NewAnnouncer(dir *company.Directory, pub event.Publisher, rec auth.AuditRecorder, log *slog.Logger) *Announcer {
	return &Announcer{dir: dir, pub: pub, audit: rec, log: log}
}

type announcementRequest struct {
	Title string `json:"title"`
	Body  string `json:"body"`
}

// Create serves POST /api/v1/admin/announcements.
func (a *Announcer) Create(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	var req announcementRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Fail(w, r, a.log, err)
		return
	}
	req.Title, req.Body = strings.TrimSpace(req.Title), strings.TrimSpace(req.Body)
	if n := utf8.RuneCountInString(req.Title); n < 1 || n > 200 {
		httpx.WriteError(w, r, http.StatusBadRequest, httpx.CodeBadRequest, "title must be 1–200 characters.")
		return
	}
	if utf8.RuneCountInString(req.Body) > 2000 {
		httpx.WriteError(w, r, http.StatusBadRequest, httpx.CodeBadRequest, "body must not exceed 2000 characters.")
		return
	}
	c, err := a.dir.Active(r.Context())
	if err != nil {
		httpx.Fail(w, r, a.log, err)
		return
	}
	e, err := event.New(event.WorldAnnouncement, event.WorldTopic(c.ID), map[string]any{
		"title": req.Title, "body": req.Body, "from": p.Username,
	})
	if err != nil {
		httpx.Fail(w, r, a.log, err)
		return
	}
	a.pub.Publish(r.Context(), e)
	a.audit.Record(r.Context(), audit.Entry{
		ActorType: audit.ActorUser, ActorID: p.UserID.String(),
		Action: "world.announcement_sent", ResourceType: "company", ResourceID: c.ID.String(),
		Result: audit.ResultSuccess, Metadata: map[string]any{"event_id": e.ID, "title": req.Title},
	})
	httpx.JSON(w, http.StatusAccepted, map[string]any{"event_id": e.ID, "topic": e.Topic})
}
