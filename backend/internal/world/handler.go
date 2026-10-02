package world

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/netip"
	"regexp"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Darkload9999/VORTECH/backend/internal/asset"
	"github.com/Darkload9999/VORTECH/backend/internal/company"
	"github.com/Darkload9999/VORTECH/backend/internal/database/db"
	"github.com/Darkload9999/VORTECH/backend/internal/httpx"
)

var objectKey = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,127}$`)

// Handler serves the authoritative world model.
type Handler struct {
	dir *company.Directory
	q   *db.Queries
	log *slog.Logger
}

// NewHandler returns a Handler.
func NewHandler(dir *company.Directory, q *db.Queries, log *slog.Logger) *Handler {
	return &Handler{dir: dir, q: q, log: log}
}

// Zone is a navigable area. Progression fields describe the gate; whether a
// given player has passed it is part of the player's progress.
type Zone struct {
	ID                 uuid.UUID  `json:"id"`
	Code               string     `json:"code"`
	Name               string     `json:"name"`
	Kind               string     `json:"kind"`
	Description        string     `json:"description"`
	ParentID           *uuid.UUID `json:"parent_id"`
	ParentCode         *string    `json:"parent_code"`
	UnlockedByDefault  bool       `json:"unlocked_by_default"`
	RequiredCareerRank int32      `json:"required_career_rank"`
}

// WorldView is GET /api/v1/world.
type WorldView struct {
	Company CompanyRef `json:"company"`
	Zones   []Zone     `json:"zones"`
}

// CompanyRef identifies the world's company and scenario.
type CompanyRef struct {
	ID              uuid.UUID `json:"id"`
	Code            string    `json:"code"`
	Name            string    `json:"name"`
	ScenarioSlug    string    `json:"scenario_slug"`
	ScenarioVersion string    `json:"scenario_version"`
}

// World serves GET /api/v1/world: the zone hierarchy as a flat list (use
// parent_id to build the tree).
func (h *Handler) World(w http.ResponseWriter, r *http.Request) {
	c, err := h.dir.Active(r.Context())
	if err != nil {
		httpx.Fail(w, r, h.log, err)
		return
	}
	rows, err := h.q.ListZones(r.Context(), c.ID)
	if err != nil {
		httpx.Fail(w, r, h.log, err)
		return
	}
	zones := make([]Zone, len(rows))
	for i, z := range rows {
		zones[i] = Zone{
			ID: z.ID, Code: z.Code, Name: z.Name, Kind: z.Kind, Description: z.Description,
			ParentID: z.ParentID, ParentCode: z.ParentCode,
			UnlockedByDefault: z.UnlockedByDefault, RequiredCareerRank: z.RequiredCareerRank,
		}
	}
	httpx.JSON(w, http.StatusOK, WorldView{
		Company: CompanyRef{ID: c.ID, Code: c.Code, Name: c.Name, ScenarioSlug: c.ScenarioSlug, ScenarioVersion: c.ScenarioVersion},
		Zones:   zones,
	})
}

// ZoneDetail is GET /api/v1/world/zones/{id}.
type ZoneDetail struct {
	Zone
	Locations []Location `json:"locations"`
	Objects   []Object   `json:"objects"`
	NPCs      []NPC      `json:"npcs"`
}

// Location is a place inside a zone.
type Location struct {
	ID             uuid.UUID `json:"id"`
	Code           string    `json:"code"`
	Name           string    `json:"name"`
	Kind           string    `json:"kind"`
	Description    string    `json:"description"`
	DepartmentCode *string   `json:"department_code"`
}

// Object is an interactive Three.js object.
type Object struct {
	ID           uuid.UUID     `json:"id"`
	Key          string        `json:"key"`
	Name         string        `json:"name"`
	Kind         string        `json:"kind"`
	Description  string        `json:"description"`
	Interactions []string      `json:"interactions"`
	LocationCode string        `json:"location_code"`
	Asset        *AssetSummary `json:"asset"`
	LeadsTo      *ZoneRef      `json:"leads_to"`
}

// AssetSummary is the digital asset behind an object.
type AssetSummary struct {
	ID        uuid.UUID   `json:"id"`
	Code      string      `json:"code"`
	Name      string      `json:"name"`
	Type      string      `json:"type"`
	Hostname  *string     `json:"hostname"`
	IPAddress *netip.Addr `json:"ip_address"`
}

// ZoneRef references a zone.
type ZoneRef struct {
	ID   uuid.UUID `json:"id"`
	Code string    `json:"code"`
}

// NPC is an employee present in the zone.
type NPC struct {
	EmployeeID   uuid.UUID `json:"employee_id"`
	Code         string    `json:"code"`
	DisplayName  string    `json:"display_name"`
	JobTitle     string    `json:"job_title"`
	Greeting     string    `json:"greeting"`
	LocationCode string    `json:"location_code"`
}

// Zone serves GET /api/v1/world/zones/{id}.
func (h *Handler) Zone(w http.ResponseWriter, r *http.Request) {
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
	z, err := h.q.GetZone(ctx, db.GetZoneParams{CompanyID: c.ID, ID: id})
	if errors.Is(err, pgx.ErrNoRows) {
		httpx.WriteError(w, r, http.StatusNotFound, httpx.CodeNotFound, "Zone not found.")
		return
	}
	if err != nil {
		httpx.Fail(w, r, h.log, err)
		return
	}
	d := ZoneDetail{
		Zone: Zone{
			ID: z.ID, Code: z.Code, Name: z.Name, Kind: z.Kind, Description: z.Description,
			ParentID: z.ParentID, ParentCode: z.ParentCode,
			UnlockedByDefault: z.UnlockedByDefault, RequiredCareerRank: z.RequiredCareerRank,
		},
		Locations: []Location{}, Objects: []Object{}, NPCs: []NPC{},
	}

	locs, err := h.q.ListZoneLocations(ctx, db.ListZoneLocationsParams{CompanyID: c.ID, ZoneID: z.ID})
	if err != nil {
		httpx.Fail(w, r, h.log, err)
		return
	}
	for _, l := range locs {
		d.Locations = append(d.Locations, Location{ID: l.ID, Code: l.Code, Name: l.Name, Kind: l.Kind, Description: l.Description, DepartmentCode: l.DepartmentCode})
	}

	objs, err := h.q.ListZoneObjects(ctx, db.ListZoneObjectsParams{CompanyID: c.ID, ZoneID: z.ID})
	if err != nil {
		httpx.Fail(w, r, h.log, err)
		return
	}
	for _, o := range objs {
		obj := Object{
			ID: o.ID, Key: o.ObjectKey, Name: o.Name, Kind: o.Kind, Description: o.Description,
			Interactions: nonNil(o.Interactions), LocationCode: o.LocationCode,
		}
		if o.AssetID != nil {
			obj.Asset = &AssetSummary{ID: *o.AssetID, Code: deref(o.AssetCode), Name: deref(o.AssetName),
				Type: deref(o.AssetType), Hostname: o.Hostname, IPAddress: o.IPAddress}
		}
		if o.LeadsToZoneID != nil {
			obj.LeadsTo = &ZoneRef{ID: *o.LeadsToZoneID, Code: deref(o.LeadsToZoneCode)}
		}
		d.Objects = append(d.Objects, obj)
	}

	npcs, err := h.q.ListZoneEmployees(ctx, db.ListZoneEmployeesParams{CompanyID: c.ID, ZoneID: z.ID})
	if err != nil {
		httpx.Fail(w, r, h.log, err)
		return
	}
	for _, n := range npcs {
		d.NPCs = append(d.NPCs, NPC{EmployeeID: n.ID, Code: n.EmployeeCode, DisplayName: n.DisplayName,
			JobTitle: n.JobTitle, Greeting: n.Greeting, LocationCode: n.LocationCode})
	}
	httpx.JSON(w, http.StatusOK, d)
}

// ObjectDetail is GET /api/v1/world/objects/{key}: the physical → digital
// mapping behind the Digital Twin.
type ObjectDetail struct {
	ID           uuid.UUID       `json:"id"`
	Key          string          `json:"key"`
	Name         string          `json:"name"`
	Kind         string          `json:"kind"`
	Description  string          `json:"description"`
	Interactions []string        `json:"interactions"`
	Attributes   json.RawMessage `json:"attributes"`
	Location     LocationRef     `json:"location"`
	Zone         ZoneRef         `json:"zone"`
	LeadsTo      *ZoneRef        `json:"leads_to"`
	Asset        *ObjectAsset    `json:"asset"`
}

// LocationRef references a location.
type LocationRef struct {
	ID   uuid.UUID `json:"id"`
	Code string    `json:"code"`
	Name string    `json:"name"`
}

// ObjectAsset is the digital asset mapped to an object.
type ObjectAsset struct {
	AssetSummary
	OperatingSystem *string            `json:"operating_system"`
	Criticality     *string            `json:"criticality"`
	DepartmentCode  *string            `json:"department_code"`
	Owner           *OwnerRef          `json:"owner"`
	Networks        []asset.NetworkRef `json:"networks"`
}

// OwnerRef references the owning employee.
type OwnerRef struct {
	Code string `json:"code"`
	Name string `json:"name"`
}

// Object serves GET /api/v1/world/objects/{key}, resolving a Three.js
// object name (e.g. finance_pc_04) to its world placement and asset.
func (h *Handler) Object(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	if !objectKey.MatchString(key) {
		httpx.WriteError(w, r, http.StatusNotFound, httpx.CodeNotFound, "Object not found.")
		return
	}
	c, err := h.dir.Active(r.Context())
	if err != nil {
		httpx.Fail(w, r, h.log, err)
		return
	}
	ctx := r.Context()
	o, err := h.q.GetWorldObjectByKey(ctx, db.GetWorldObjectByKeyParams{CompanyID: c.ID, ObjectKey: key})
	if errors.Is(err, pgx.ErrNoRows) {
		httpx.WriteError(w, r, http.StatusNotFound, httpx.CodeNotFound, "Object not found.")
		return
	}
	if err != nil {
		httpx.Fail(w, r, h.log, err)
		return
	}

	d := ObjectDetail{
		ID: o.ID, Key: o.ObjectKey, Name: o.Name, Kind: o.Kind, Description: o.Description,
		Interactions: nonNil(o.Interactions), Attributes: json.RawMessage(o.Attributes),
		Location: LocationRef{ID: o.LocationID, Code: o.LocationCode, Name: o.LocationName},
		Zone:     ZoneRef{ID: o.ZoneID, Code: o.ZoneCode},
	}
	if o.LeadsToZoneID != nil {
		d.LeadsTo = &ZoneRef{ID: *o.LeadsToZoneID, Code: deref(o.LeadsToZoneCode)}
	}
	if o.AssetID != nil {
		nets, err := asset.Networks(ctx, h.q, c.ID, *o.AssetID)
		if err != nil {
			httpx.Fail(w, r, h.log, err)
			return
		}
		d.Asset = &ObjectAsset{
			AssetSummary: AssetSummary{ID: *o.AssetID, Code: deref(o.AssetCode), Name: deref(o.AssetName),
				Type: deref(o.AssetType), Hostname: o.Hostname, IPAddress: o.IPAddress},
			OperatingSystem: o.OperatingSystem, Criticality: o.Criticality,
			DepartmentCode: o.AssetDepartmentCode, Networks: nets,
		}
		if o.AssetOwnerCode != nil {
			d.Asset.Owner = &OwnerRef{Code: *o.AssetOwnerCode, Name: deref(o.AssetOwnerName)}
		}
	}
	httpx.JSON(w, http.StatusOK, d)
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
