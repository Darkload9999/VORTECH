package company

import (
	"log/slog"
	"net/http"

	"github.com/google/uuid"

	"github.com/Darkload9999/VORTECH/backend/internal/database/db"
	"github.com/Darkload9999/VORTECH/backend/internal/httpx"
)

// Handler serves the company and its departments.
type Handler struct {
	dir *Directory
	q   *db.Queries
	log *slog.Logger
}

// NewHandler returns a Handler.
func NewHandler(dir *Directory, q *db.Queries, log *slog.Logger) *Handler {
	return &Handler{dir: dir, q: q, log: log}
}

// CompanyView is GET /api/v1/company.
type CompanyView struct {
	ID           uuid.UUID    `json:"id"`
	Code         string       `json:"code"`
	Name         string       `json:"name"`
	LegalName    string       `json:"legal_name"`
	Industry     string       `json:"industry"`
	Description  string       `json:"description"`
	Headquarters string       `json:"headquarters"`
	FoundedYear  *int32       `json:"founded_year"`
	EmailDomain  string       `json:"email_domain"`
	Scenario     ScenarioView `json:"scenario"`
	Stats        StatsView    `json:"stats"`
}

// ScenarioView identifies the scenario the world comes from.
type ScenarioView struct {
	Slug    string `json:"slug"`
	Name    string `json:"name"`
	Version string `json:"version"`
}

// StatsView summarises the company's size.
type StatsView struct {
	Departments int32 `json:"departments"`
	Employees   int32 `json:"employees"`
	Assets      int32 `json:"assets"`
	Zones       int32 `json:"zones"`
}

// Get serves GET /api/v1/company.
func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	c, err := h.dir.Active(r.Context())
	if err != nil {
		httpx.Fail(w, r, h.log, err)
		return
	}
	st, err := h.q.GetCompanyStats(r.Context(), c.ID)
	if err != nil {
		httpx.Fail(w, r, h.log, err)
		return
	}
	httpx.JSON(w, http.StatusOK, CompanyView{
		ID: c.ID, Code: c.Code, Name: c.Name, LegalName: c.LegalName, Industry: c.Industry,
		Description: c.Description, Headquarters: c.Headquarters, FoundedYear: c.FoundedYear,
		EmailDomain: c.EmailDomain,
		Scenario:    ScenarioView{Slug: c.ScenarioSlug, Name: c.ScenarioName, Version: c.ScenarioVersion},
		Stats:       StatsView{Departments: st.Departments, Employees: st.Employees, Assets: st.Assets, Zones: st.Zones},
	})
}

// DepartmentView is one entry of GET /api/v1/company/departments.
type DepartmentView struct {
	ID            uuid.UUID    `json:"id"`
	Code          string       `json:"code"`
	Name          string       `json:"name"`
	Description   string       `json:"description"`
	ParentCode    *string      `json:"parent_code"`
	Head          *EmployeeRef `json:"head"`
	EmployeeCount int32        `json:"employee_count"`
}

// EmployeeRef is a compact reference to an employee.
type EmployeeRef struct {
	ID       uuid.UUID `json:"id"`
	Name     string    `json:"name"`
	JobTitle string    `json:"job_title,omitempty"`
}

// Departments serves GET /api/v1/company/departments.
func (h *Handler) Departments(w http.ResponseWriter, r *http.Request) {
	c, err := h.dir.Active(r.Context())
	if err != nil {
		httpx.Fail(w, r, h.log, err)
		return
	}
	rows, err := h.q.ListDepartments(r.Context(), c.ID)
	if err != nil {
		httpx.Fail(w, r, h.log, err)
		return
	}
	out := make([]DepartmentView, len(rows))
	for i, d := range rows {
		out[i] = DepartmentView{
			ID: d.ID, Code: d.Code, Name: d.Name, Description: d.Description,
			ParentCode: d.ParentCode, EmployeeCount: d.EmployeeCount,
		}
		if d.HeadID != nil {
			out[i].Head = &EmployeeRef{ID: *d.HeadID, Name: deref(d.HeadName), JobTitle: deref(d.HeadTitle)}
		}
	}
	httpx.JSON(w, http.StatusOK, out)
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
