package asset

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"regexp"
	"strconv"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Darkload9999/VORTECH/backend/internal/company"
	"github.com/Darkload9999/VORTECH/backend/internal/database/db"
	"github.com/Darkload9999/VORTECH/backend/internal/httpx"
)

const (
	defaultGraphDepth = 2
	maxGraphDepth     = 4
	maxGraphNodes     = 2000
)

var (
	departmentCode = regexp.MustCompile(`^[A-Z][A-Z0-9_-]{0,31}$`)
	assetType      = regexp.MustCompile(`^(workstation|laptop|server|virtual_machine|network_device|firewall|network|application|database|identity_group|cloud_resource|printer|iot_device|mobile_device|storage)$`)
)

// Handler serves the asset inventory and the digital-twin graph.
type Handler struct {
	dir *company.Directory
	q   *db.Queries
	log *slog.Logger
}

// NewHandler returns a Handler.
func NewHandler(dir *company.Directory, q *db.Queries, log *slog.Logger) *Handler {
	return &Handler{dir: dir, q: q, log: log}
}

// Summary is one entry of GET /api/v1/assets.
type Summary struct {
	ID             uuid.UUID     `json:"id"`
	Code           string        `json:"code"`
	Name           string        `json:"name"`
	Type           string        `json:"type"`
	Hostname       *string       `json:"hostname"`
	IPAddress      *netip.Addr   `json:"ip_address"`
	NetworkCIDR    *netip.Prefix `json:"network_cidr"`
	Criticality    string        `json:"criticality"`
	Status         string        `json:"status"`
	DepartmentCode *string       `json:"department_code"`
	OwnerCode      *string       `json:"owner_code"`
	LocationCode   *string       `json:"location_code"`
}

// List serves GET /api/v1/assets?type=&department=&q=&limit=&cursor=.
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	page, err := httpx.ParsePage(r, 50, 200)
	if err != nil {
		httpx.Fail(w, r, h.log, err)
		return
	}
	typ, err := httpx.OptionalMatching(r, "type", assetType)
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
	params := db.ListAssetsParams{CompanyID: c.ID, AssetType: typ, Department: dept, Search: search, PageSize: page.FetchLimit()}
	if page.After != "" {
		params.After = &page.After
	}
	rows, err := h.q.ListAssets(r.Context(), params)
	if err != nil {
		httpx.Fail(w, r, h.log, err)
		return
	}
	rows, meta := httpx.Paginate(page, rows, func(a db.ListAssetsRow) string { return a.AssetCode })
	out := make([]Summary, len(rows))
	for i, a := range rows {
		out[i] = Summary{
			ID: a.ID, Code: a.AssetCode, Name: a.Name, Type: a.AssetType, Hostname: a.Hostname,
			IPAddress: a.IPAddress, NetworkCIDR: a.NetworkCIDR, Criticality: a.Criticality, Status: a.Status,
			DepartmentCode: a.DepartmentCode, OwnerCode: a.OwnerCode, LocationCode: a.LocationCode,
		}
	}
	httpx.JSONWithMeta(w, http.StatusOK, out, meta)
}

// Detail is GET /api/v1/assets/{id}.
type Detail struct {
	Summary
	OperatingSystem *string         `json:"operating_system"`
	Description     string          `json:"description"`
	Attributes      json.RawMessage `json:"attributes"`
	Department      *Ref            `json:"department"`
	Owner           *Ref            `json:"owner"`
	Location        *LocationRef    `json:"location"`
	Networks        []NetworkRef    `json:"networks"`
	WorldObjects    []ObjectRef     `json:"world_objects"`
}

// Ref references an entity by ID, code and name.
type Ref struct {
	ID   uuid.UUID `json:"id"`
	Code string    `json:"code"`
	Name string    `json:"name"`
}

// LocationRef is a physical location with its zone.
type LocationRef struct {
	Ref
	ZoneID   *uuid.UUID `json:"zone_id"`
	ZoneCode *string    `json:"zone_code"`
}

// NetworkRef is a network segment the asset is connected to.
type NetworkRef struct {
	Ref
	CIDR *netip.Prefix `json:"cidr"`
}

// ObjectRef is a Three.js object representing the asset.
type ObjectRef struct {
	Key  string `json:"key"`
	Name string `json:"name"`
	Kind string `json:"kind"`
}

// Get serves GET /api/v1/assets/{id}.
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
	a, err := h.q.GetAsset(ctx, db.GetAssetParams{CompanyID: c.ID, ID: id})
	if errors.Is(err, pgx.ErrNoRows) {
		httpx.WriteError(w, r, http.StatusNotFound, httpx.CodeNotFound, "Asset not found.")
		return
	}
	if err != nil {
		httpx.Fail(w, r, h.log, err)
		return
	}

	d := Detail{
		Summary: Summary{
			ID: a.ID, Code: a.AssetCode, Name: a.Name, Type: a.AssetType, Hostname: a.Hostname,
			IPAddress: a.IPAddress, NetworkCIDR: a.NetworkCIDR, Criticality: a.Criticality, Status: a.Status,
			DepartmentCode: a.DepartmentCode, OwnerCode: a.OwnerCode, LocationCode: a.LocationCode,
		},
		OperatingSystem: a.OperatingSystem,
		Description:     a.Description,
		Attributes:      json.RawMessage(a.Attributes),
		Networks:        []NetworkRef{},
		WorldObjects:    []ObjectRef{},
	}
	if a.DepartmentID != nil {
		d.Department = &Ref{ID: *a.DepartmentID, Code: deref(a.DepartmentCode), Name: deref(a.DepartmentName)}
	}
	if a.OwnerID != nil {
		d.Owner = &Ref{ID: *a.OwnerID, Code: deref(a.OwnerCode), Name: deref(a.OwnerName)}
	}
	if a.LocationID != nil {
		d.Location = &LocationRef{Ref: Ref{ID: *a.LocationID, Code: deref(a.LocationCode), Name: deref(a.LocationName)}, ZoneID: a.ZoneID, ZoneCode: a.ZoneCode}
	}

	if d.Networks, err = Networks(ctx, h.q, c.ID, a.ID); err != nil {
		httpx.Fail(w, r, h.log, err)
		return
	}
	objs, err := h.q.ListAssetWorldObjects(ctx, db.ListAssetWorldObjectsParams{CompanyID: c.ID, AssetID: &a.ID})
	if err != nil {
		httpx.Fail(w, r, h.log, err)
		return
	}
	for _, o := range objs {
		d.WorldObjects = append(d.WorldObjects, ObjectRef{Key: o.ObjectKey, Name: o.Name, Kind: o.Kind})
	}
	httpx.JSON(w, http.StatusOK, d)
}

// Networks lists the network segments an asset is connected to. It is
// shared with the world module's object resolution.
func Networks(ctx context.Context, q *db.Queries, companyID, assetID uuid.UUID) ([]NetworkRef, error) {
	rows, err := q.ListAssetNetworks(ctx, db.ListAssetNetworksParams{CompanyID: companyID, AssetID: &assetID})
	if err != nil {
		return nil, fmt.Errorf("list asset networks: %w", err)
	}
	out := make([]NetworkRef, len(rows))
	for i, n := range rows {
		out[i] = NetworkRef{Ref: Ref{ID: n.ID, Code: n.AssetCode, Name: n.Name}, CIDR: n.NetworkCIDR}
	}
	return out, nil
}

// Graph serves GET /api/v1/assets/graph?root=<kind>:<uuid>&depth=N.
// Without root the whole company graph is returned (bounded).
func (h *Handler) Graph(w http.ResponseWriter, r *http.Request) {
	var root *NodeID
	if v := r.URL.Query().Get("root"); v != "" {
		id, ok := ParseNodeID(v)
		if !ok {
			httpx.WriteError(w, r, http.StatusBadRequest, httpx.CodeInvalidQuery,
				"root must be employee:<uuid>, identity:<uuid> or asset:<uuid>.")
			return
		}
		root = &id
	}
	depth := defaultGraphDepth
	if v := r.URL.Query().Get("depth"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxGraphDepth {
			httpx.WriteError(w, r, http.StatusBadRequest, httpx.CodeInvalidQuery,
				fmt.Sprintf("depth must be between 1 and %d.", maxGraphDepth))
			return
		}
		depth = n
	}

	c, err := h.dir.Active(r.Context())
	if err != nil {
		httpx.Fail(w, r, h.log, err)
		return
	}
	nodes, edges, err := h.loadGraph(r.Context(), c.ID)
	if err != nil {
		httpx.Fail(w, r, h.log, err)
		return
	}
	if root != nil {
		if _, ok := nodes[*root]; !ok {
			httpx.WriteError(w, r, http.StatusNotFound, httpx.CodeNotFound, "Graph root not found.")
			return
		}
	}
	httpx.JSON(w, http.StatusOK, BuildGraph(nodes, edges, root, depth, maxGraphNodes))
}

// loadGraph reads all nodes and edges of a company. Scenario companies are
// small (hundreds to low thousands of nodes), so a single pass plus an
// in-memory traversal is cheaper and simpler than recursive SQL.
func (h *Handler) loadGraph(ctx context.Context, companyID uuid.UUID) (map[NodeID]NodeData, []Edge, error) {
	nodes := map[NodeID]NodeData{}

	emps, err := h.q.ListGraphEmployees(ctx, companyID)
	if err != nil {
		return nil, nil, fmt.Errorf("graph employees: %w", err)
	}
	for _, e := range emps {
		id := MakeNodeID("employee", e.ID)
		nodes[id] = NodeData{ID: id, Kind: "employee", Type: "employee", Label: e.DisplayName,
			Code: e.EmployeeCode, JobTitle: e.JobTitle, Department: e.DepartmentCode}
	}
	idents, err := h.q.ListGraphIdentities(ctx, companyID)
	if err != nil {
		return nil, nil, fmt.Errorf("graph identities: %w", err)
	}
	for _, i := range idents {
		id := MakeNodeID("identity", i.ID)
		nodes[id] = NodeData{ID: id, Kind: "identity", Type: i.IdentityType, Label: i.Email,
			Code: i.Username, Privileged: i.Privileged, Status: i.Status}
	}
	assets, err := h.q.ListGraphAssets(ctx, companyID)
	if err != nil {
		return nil, nil, fmt.Errorf("graph assets: %w", err)
	}
	for _, a := range assets {
		id := MakeNodeID("asset", a.ID)
		nodes[id] = NodeData{ID: id, Kind: "asset", Type: a.AssetType, Label: a.Name,
			Code: a.AssetCode, Hostname: deref(a.Hostname), Criticality: a.Criticality}
	}

	rels, err := h.q.ListCompanyRelationships(ctx, companyID)
	if err != nil {
		return nil, nil, fmt.Errorf("graph relationships: %w", err)
	}
	edges := make([]Edge, 0, len(rels))
	for _, r := range rels {
		edges = append(edges, Edge{
			ID:     r.ID.String(),
			Type:   r.RelationshipType,
			Source: endpoint(r.SourceEmployeeID, r.SourceIdentityID, r.SourceAssetID),
			Target: endpoint(r.TargetEmployeeID, r.TargetIdentityID, r.TargetAssetID),
		})
	}
	return nodes, edges, nil
}

// endpoint picks the single non-nil reference (enforced by a CHECK constraint).
func endpoint(employee, identity, asset *uuid.UUID) NodeID {
	switch {
	case employee != nil:
		return MakeNodeID("employee", *employee)
	case identity != nil:
		return MakeNodeID("identity", *identity)
	default:
		return MakeNodeID("asset", *asset)
	}
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
