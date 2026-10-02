package employee

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Darkload9999/VORTECH/backend/internal/company"
	"github.com/Darkload9999/VORTECH/backend/internal/database/db"
	"github.com/Darkload9999/VORTECH/backend/internal/httpx"
)

var departmentCode = regexp.MustCompile(`^[A-Z][A-Z0-9_-]{0,31}$`)

// Handler serves the employee directory of the active company.
type Handler struct {
	dir *company.Directory
	q   *db.Queries
	log *slog.Logger
}

// NewHandler returns a Handler.
func NewHandler(dir *company.Directory, q *db.Queries, log *slog.Logger) *Handler {
	return &Handler{dir: dir, q: q, log: log}
}

// Summary is one entry of GET /api/v1/employees.
type Summary struct {
	ID             uuid.UUID `json:"id"`
	Code           string    `json:"code"`
	DisplayName    string    `json:"display_name"`
	JobTitle       string    `json:"job_title"`
	Department     Ref       `json:"department"`
	LocationCode   *string   `json:"location_code"`
	Status         string    `json:"status"`
	EmploymentType string    `json:"employment_type"`
	IsNPC          bool      `json:"is_npc"`
}

// Ref is a compact code/name reference.
type Ref struct {
	Code string `json:"code"`
	Name string `json:"name"`
}

// List serves GET /api/v1/employees?department=&q=&limit=&cursor=.
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	page, err := httpx.ParsePage(r, 50, 200)
	if err != nil {
		httpx.Fail(w, r, h.log, err)
		return
	}
	dept, err := httpx.OptionalMatching(r, "department", departmentCode)
	if err != nil {
		httpx.Fail(w, r, h.log, err)
		return
	}
	search, err := httpx.SearchPattern(r)
	if err != nil {
		httpx.Fail(w, r, h.log, err)
		return
	}
	c, err := h.dir.Active(r.Context())
	if err != nil {
		httpx.Fail(w, r, h.log, err)
		return
	}

	params := db.ListEmployeesParams{CompanyID: c.ID, Department: dept, Search: search, PageSize: page.FetchLimit()}
	if page.After != "" {
		params.After = &page.After
	}
	rows, err := h.q.ListEmployees(r.Context(), params)
	if err != nil {
		httpx.Fail(w, r, h.log, err)
		return
	}
	rows, meta := httpx.Paginate(page, rows, func(e db.ListEmployeesRow) string { return e.EmployeeCode })
	out := make([]Summary, len(rows))
	for i, e := range rows {
		out[i] = Summary{
			ID: e.ID, Code: e.EmployeeCode, DisplayName: e.DisplayName, JobTitle: e.JobTitle,
			Department:   Ref{Code: e.DepartmentCode, Name: e.DepartmentName},
			LocationCode: e.LocationCode, Status: e.Status, EmploymentType: e.EmploymentType, IsNPC: e.IsNpc,
		}
	}
	httpx.JSONWithMeta(w, http.StatusOK, out, meta)
}

// Detail is GET /api/v1/employees/{id}. The NPC persona is an authoring
// note for the dialogue engine and is intentionally not exposed.
type Detail struct {
	ID             uuid.UUID      `json:"id"`
	Code           string         `json:"code"`
	FirstName      string         `json:"first_name"`
	LastName       string         `json:"last_name"`
	DisplayName    string         `json:"display_name"`
	JobTitle       string         `json:"job_title"`
	Status         string         `json:"status"`
	EmploymentType string         `json:"employment_type"`
	IsNPC          bool           `json:"is_npc"`
	Greeting       string         `json:"greeting"`
	Department     IDRef          `json:"department"`
	Manager        *IDRef         `json:"manager"`
	Location       *Location      `json:"location"`
	Identities     []Identity     `json:"identities"`
	Schedule       []ScheduleItem `json:"schedule"`
	DirectReports  []IDRef        `json:"direct_reports"`
	Assets         []AssetRef     `json:"assets"`
}

// IDRef references an entity by ID, code and name.
type IDRef struct {
	ID   uuid.UUID `json:"id"`
	Code string    `json:"code"`
	Name string    `json:"name"`
}

// Location is where an employee normally works.
type Location struct {
	ID       uuid.UUID  `json:"id"`
	Code     string     `json:"code"`
	Name     string     `json:"name"`
	ZoneID   *uuid.UUID `json:"zone_id"`
	ZoneCode *string    `json:"zone_code"`
}

// Identity is a fictional enterprise directory account (never a platform login).
type Identity struct {
	ID         uuid.UUID  `json:"id"`
	Username   string     `json:"username"`
	Email      string     `json:"email"`
	Type       string     `json:"type"`
	Status     string     `json:"status"`
	MFAEnabled bool       `json:"mfa_enabled"`
	Privileged bool       `json:"privileged"`
	ExpiresAt  *time.Time `json:"expires_at"`
	Groups     []string   `json:"groups"`
}

// ScheduleItem is one block of the weekly routine (ISO weekday, local time).
type ScheduleItem struct {
	Weekday      int32   `json:"weekday"`
	From         string  `json:"from"`
	To           string  `json:"to"`
	Activity     string  `json:"activity"`
	LocationCode *string `json:"location_code"`
}

// AssetRef is a compact asset reference.
type AssetRef struct {
	ID       uuid.UUID `json:"id"`
	Code     string    `json:"code"`
	Name     string    `json:"name"`
	Type     string    `json:"type"`
	Hostname *string   `json:"hostname"`
}

// Get serves GET /api/v1/employees/{id}.
func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	id, err := httpx.PathUUID(r, "id")
	if err != nil {
		httpx.Fail(w, r, h.log, err)
		return
	}
	c, err := h.dir.Active(r.Context())
	if err != nil {
		httpx.Fail(w, r, h.log, err)
		return
	}
	ctx := r.Context()
	e, err := h.q.GetEmployee(ctx, db.GetEmployeeParams{CompanyID: c.ID, ID: id})
	if errors.Is(err, pgx.ErrNoRows) {
		httpx.WriteError(w, r, http.StatusNotFound, httpx.CodeNotFound, "Employee not found.")
		return
	}
	if err != nil {
		httpx.Fail(w, r, h.log, err)
		return
	}

	d := Detail{
		ID: e.ID, Code: e.EmployeeCode, FirstName: e.FirstName, LastName: e.LastName,
		DisplayName: e.DisplayName, JobTitle: e.JobTitle, Status: e.Status,
		EmploymentType: e.EmploymentType, IsNPC: e.IsNpc, Greeting: e.Greeting,
		Department:    IDRef{ID: e.DepartmentID, Code: e.DepartmentCode, Name: e.DepartmentName},
		Identities:    []Identity{},
		Schedule:      []ScheduleItem{},
		DirectReports: []IDRef{},
		Assets:        []AssetRef{},
	}
	if e.ManagerID != nil {
		d.Manager = &IDRef{ID: *e.ManagerID, Code: deref(e.ManagerCode), Name: deref(e.ManagerName)}
	}
	if e.LocationID != nil {
		d.Location = &Location{ID: *e.LocationID, Code: deref(e.LocationCode), Name: deref(e.LocationName), ZoneID: e.ZoneID, ZoneCode: e.ZoneCode}
	}

	idents, err := h.q.ListEmployeeIdentities(ctx, db.ListEmployeeIdentitiesParams{CompanyID: c.ID, EmployeeID: &e.ID})
	if err != nil {
		httpx.Fail(w, r, h.log, err)
		return
	}
	for _, i := range idents {
		d.Identities = append(d.Identities, Identity{
			ID: i.ID, Username: i.Username, Email: i.Email, Type: i.IdentityType, Status: i.Status,
			MFAEnabled: i.MfaEnabled, Privileged: i.Privileged, ExpiresAt: i.ExpiresAt, Groups: i.Groups,
		})
	}

	sched, err := h.q.ListEmployeeSchedule(ctx, db.ListEmployeeScheduleParams{CompanyID: c.ID, EmployeeID: e.ID})
	if err != nil {
		httpx.Fail(w, r, h.log, err)
		return
	}
	for _, s := range sched {
		d.Schedule = append(d.Schedule, ScheduleItem{
			Weekday: s.Weekday, From: clock(s.StartsAt), To: clock(s.EndsAt), Activity: s.Activity, LocationCode: s.LocationCode,
		})
	}

	reports, err := h.q.ListDirectReports(ctx, db.ListDirectReportsParams{CompanyID: c.ID, ManagerID: &e.ID})
	if err != nil {
		httpx.Fail(w, r, h.log, err)
		return
	}
	for _, rep := range reports {
		d.DirectReports = append(d.DirectReports, IDRef{ID: rep.ID, Code: rep.EmployeeCode, Name: rep.DisplayName})
	}

	assets, err := h.q.ListEmployeeAssets(ctx, db.ListEmployeeAssetsParams{CompanyID: c.ID, EmployeeID: &e.ID})
	if err != nil {
		httpx.Fail(w, r, h.log, err)
		return
	}
	for _, a := range assets {
		d.Assets = append(d.Assets, AssetRef{ID: a.ID, Code: a.AssetCode, Name: a.Name, Type: a.AssetType, Hostname: a.Hostname})
	}

	httpx.JSON(w, http.StatusOK, d)
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// clock renders a PostgreSQL time as HH:MM.
func clock(t pgtype.Time) string {
	if !t.Valid {
		return ""
	}
	mins := t.Microseconds / int64(time.Minute/time.Microsecond)
	return fmt.Sprintf("%02d:%02d", mins/60, mins%60)
}
