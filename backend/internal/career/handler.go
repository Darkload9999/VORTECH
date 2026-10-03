package career

import (
	"log/slog"
	"net/http"
	"sort"

	"github.com/google/uuid"

	"github.com/Darkload9999/VORTECH/backend/internal/auth"
	"github.com/Darkload9999/VORTECH/backend/internal/company"
	"github.com/Darkload9999/VORTECH/backend/internal/database/db"
	"github.com/Darkload9999/VORTECH/backend/internal/httpx"
)

// CodeNoPlayerProfile is returned to callers without a player profile.
const CodeNoPlayerProfile = "NO_PLAYER_PROFILE"

// Handler serves GET /api/v1/me/progress.
type Handler struct {
	svc *Service
	dir *company.Directory
	q   *db.Queries
	log *slog.Logger
}

// NewHandler returns a Handler.
func NewHandler(svc *Service, dir *company.Directory, q *db.Queries, log *slog.Logger) *Handler {
	return &Handler{svc: svc, dir: dir, q: q, log: log}
}

// ProgressView is the body of GET /api/v1/me/progress.
type ProgressView struct {
	PlayerID      uuid.UUID    `json:"player_id"`
	XP            int32        `json:"xp"`
	Level         Level        `json:"level"`
	NextLevel     *NextLevel   `json:"next_level"`
	CurrentZone   *ZoneRef     `json:"current_zone"`
	UnlockedZones []Unlock     `json:"unlocked_zones"`
	Zones         []ZoneAccess `json:"zones"`
	Discoveries   int32        `json:"discoveries"`
}

// NextLevel is the next rung with the XP still needed.
type NextLevel struct {
	Level
	XPRemaining int32 `json:"xp_remaining"`
}

// ZoneRef references a zone.
type ZoneRef struct {
	ID   uuid.UUID `json:"id"`
	Code string    `json:"code"`
	Name string    `json:"name"`
}

// ZoneAccess is the caller's access to one zone.
type ZoneAccess struct {
	ZoneID       uuid.UUID `json:"zone_id"`
	Code         string    `json:"code"`
	Accessible   bool      `json:"accessible"`
	Reason       string    `json:"reason,omitempty"`
	RequiredRank int32     `json:"required_rank,omitempty"`
}

// Me serves GET /api/v1/me/progress.
func (h *Handler) Me(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.FromContext(r.Context())
	if !ok || p.PlayerID == nil {
		httpx.WriteError(w, r, http.StatusForbidden, CodeNoPlayerProfile, "This account has no player profile.")
		return
	}
	c, err := h.dir.Active(r.Context())
	if err != nil {
		httpx.Fail(w, r, h.log, err)
		return
	}
	st, err := h.svc.Load(r.Context(), h.q, c.ID, *p.PlayerID, false)
	if err != nil {
		httpx.Fail(w, r, h.log, err)
		return
	}
	n, err := h.q.CountDiscoveries(r.Context(), db.CountDiscoveriesParams{PlayerID: *p.PlayerID, CompanyID: c.ID})
	if err != nil {
		httpx.Fail(w, r, h.log, err)
		return
	}
	httpx.JSON(w, http.StatusOK, View(st, n))
}

// View renders a State; shared with the interaction responses.
func View(st State, discoveries int32) ProgressView {
	v := ProgressView{
		PlayerID: st.PlayerID, XP: st.XP, Level: st.Level,
		UnlockedZones: st.Unlocks, Zones: []ZoneAccess{}, Discoveries: discoveries,
	}
	if v.UnlockedZones == nil {
		v.UnlockedZones = []Unlock{}
	}
	if st.Next != nil {
		v.NextLevel = &NextLevel{Level: *st.Next, XPRemaining: st.Next.MinXP - st.XP}
	}
	if st.CurrentZoneID != nil {
		if z, ok := st.Access.Zone(*st.CurrentZoneID); ok {
			v.CurrentZone = &ZoneRef{ID: z.ID, Code: z.Code, Name: z.Name}
		}
	}
	for id, d := range st.Access.All() {
		z, _ := st.Access.Zone(id)
		v.Zones = append(v.Zones, ZoneAccess{ZoneID: id, Code: z.Code, Accessible: d.Accessible, Reason: d.Reason, RequiredRank: d.RequiredRank})
	}
	sort.Slice(v.Zones, func(i, j int) bool { return v.Zones[i].Code < v.Zones[j].Code })
	return v
}
