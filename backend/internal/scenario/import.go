package scenario

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Darkload9999/VORTECH/backend/internal/audit"
	"github.com/Darkload9999/VORTECH/backend/internal/database/db"
)

// ImportOptions controls an import.
type ImportOptions struct {
	// Activate publishes the scenario and makes its world the one served.
	Activate bool
	// Force re-applies content even if its hash is unchanged.
	Force bool
	// Actor identifies who ran the import in the audit log.
	Actor string
}

// ImportResult summarises an import.
type ImportResult struct {
	ScenarioID uuid.UUID
	CompanyID  uuid.UUID
	Outcome    string // created | updated | unchanged
	Activated  bool
	Counts     map[string]int
	Pruned     map[string]int64
}

// Import writes a validated bundle in a single transaction. Entities are
// upserted by natural key (IDs stay stable) and anything removed from the
// files is pruned. Concurrent imports of the same scenario are serialised.
func Import(ctx context.Context, pool *pgxpool.Pool, b *Bundle, opts ImportOptions) (*ImportResult, error) {
	if opts.Actor == "" {
		opts.Actor = "scenario-cli"
	}
	res := &ImportResult{Counts: map[string]int{}, Pruned: map[string]int64{}}
	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		q := db.New(tx)
		if err := q.LockScenarioImport(ctx, b.Scenario.Slug); err != nil {
			return fmt.Errorf("lock scenario: %w", err)
		}

		existing, err := q.GetScenarioBySlug(ctx, b.Scenario.Slug)
		switch {
		case err == nil && existing.ContentHash == b.Hash && !opts.Force:
			res.ScenarioID, res.Outcome = existing.ID, "unchanged"
			if res.CompanyID, err = q.GetCompanyIDByScenario(ctx, existing.ID); err != nil {
				return fmt.Errorf("load company: %w", err)
			}
			if opts.Activate && !existing.IsActive {
				return activate(ctx, q, res, b, opts)
			}
			return nil
		case err != nil && !errors.Is(err, pgx.ErrNoRows):
			return fmt.Errorf("load scenario: %w", err)
		}

		w := &writer{q: q, b: b, res: res}
		if err := w.write(ctx); err != nil {
			return err
		}
		if err := audit.Insert(ctx, q, audit.Entry{
			ActorType:    audit.ActorService,
			ActorID:      opts.Actor,
			Action:       "scenario.imported",
			ResourceType: "scenario",
			ResourceID:   res.ScenarioID.String(),
			Result:       audit.ResultSuccess,
			Metadata: map[string]any{
				"slug": b.Scenario.Slug, "version": b.Scenario.Version,
				"content_hash": b.Hash, "outcome": res.Outcome,
				"counts": res.Counts, "pruned": res.Pruned,
			},
		}); err != nil {
			return err
		}
		if opts.Activate {
			return activate(ctx, q, res, b, opts)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return res, nil
}

func activate(ctx context.Context, q *db.Queries, res *ImportResult, b *Bundle, opts ImportOptions) error {
	if err := q.DeactivateOtherScenarios(ctx, res.ScenarioID); err != nil {
		return fmt.Errorf("deactivate scenarios: %w", err)
	}
	if err := q.PublishAndActivateScenario(ctx, res.ScenarioID); err != nil {
		return fmt.Errorf("activate scenario: %w", err)
	}
	res.Activated = true
	return audit.Insert(ctx, q, audit.Entry{
		ActorType:    audit.ActorService,
		ActorID:      opts.Actor,
		Action:       "scenario.activated",
		ResourceType: "scenario",
		ResourceID:   res.ScenarioID.String(),
		Result:       audit.ResultSuccess,
		Metadata:     map[string]any{"slug": b.Scenario.Slug, "version": b.Scenario.Version},
	})
}

// writer holds the code → ID maps built while writing one bundle.
type writer struct {
	q   *db.Queries
	b   *Bundle
	res *ImportResult

	companyID uuid.UUID
	depts     map[string]uuid.UUID
	zones     map[string]uuid.UUID
	locs      map[string]uuid.UUID
	emps      map[string]uuid.UUID
	idents    map[string]uuid.UUID
	assets    map[string]uuid.UUID
}

func (w *writer) write(ctx context.Context) error {
	b, q := w.b, w.q

	sc, err := q.UpsertScenario(ctx, db.UpsertScenarioParams{
		Slug: b.Scenario.Slug, Name: b.Scenario.Name, Version: b.Scenario.Version,
		Description: b.Scenario.Description, ContentHash: b.Hash,
	})
	if err != nil {
		return fmt.Errorf("upsert scenario: %w", err)
	}
	w.res.ScenarioID = sc.ID
	w.res.Outcome = "updated"
	if sc.Inserted {
		w.res.Outcome = "created"
	}

	w.companyID, err = q.UpsertCompany(ctx, db.UpsertCompanyParams{
		ScenarioID: sc.ID, Code: b.Company.Code, Name: b.Company.Name, LegalName: b.Company.LegalName,
		Industry: b.Company.Industry, Description: b.Company.Description,
		Headquarters: b.Company.Headquarters, FoundedYear: b.Company.FoundedYear,
		EmailDomain: b.Company.EmailDomain,
	})
	if err != nil {
		return fmt.Errorf("upsert company: %w", err)
	}
	w.res.CompanyID = w.companyID

	steps := []struct {
		name string
		fn   func(context.Context) error
	}{
		{"departments", w.departments},
		{"zones", w.worldZones},
		{"locations", w.locations},
		{"employees", w.employees},
		{"department links", w.departmentLinks},
		{"schedules", w.schedules},
		{"identities", w.identities},
		{"assets", w.assetsAndNetworks},
		{"world objects", w.objects},
		{"relationships", w.relationships},
		{"prune", w.prune},
	}
	for _, s := range steps {
		if err := s.fn(ctx); err != nil {
			return fmt.Errorf("import %s: %w", s.name, err)
		}
	}
	return nil
}

func (w *writer) departments(ctx context.Context) error {
	w.depts = map[string]uuid.UUID{}
	for i, d := range w.b.Departments {
		id, err := w.q.UpsertDepartment(ctx, db.UpsertDepartmentParams{
			CompanyID: w.companyID, Code: d.Code, Name: d.Name, Description: d.Description, SortOrder: int32(i),
		})
		if err != nil {
			return fmt.Errorf("%s: %w", d.Code, err)
		}
		w.depts[d.Code] = id
	}
	w.res.Counts["departments"] = len(w.depts)
	return nil
}

func (w *writer) worldZones(ctx context.Context) error {
	w.zones = map[string]uuid.UUID{}
	for i, z := range w.b.World.Zones {
		id, err := w.q.UpsertZone(ctx, db.UpsertZoneParams{
			CompanyID: w.companyID, Code: z.Code, Name: z.Name, Kind: z.Kind, Description: z.Description,
			UnlockedByDefault: z.UnlockedByDefault, RequiredCareerRank: z.RequiredCareerRank, SortOrder: int32(i),
		})
		if err != nil {
			return fmt.Errorf("%s: %w", z.Code, err)
		}
		w.zones[z.Code] = id
	}
	for _, z := range w.b.World.Zones {
		if err := w.q.SetZoneParent(ctx, db.SetZoneParentParams{ID: w.zones[z.Code], ParentID: ref(w.zones, z.Parent)}); err != nil {
			return fmt.Errorf("%s parent: %w", z.Code, err)
		}
	}
	w.res.Counts["zones"] = len(w.zones)
	return nil
}

func (w *writer) locations(ctx context.Context) error {
	w.locs = map[string]uuid.UUID{}
	for _, l := range w.b.World.Locations {
		id, err := w.q.UpsertLocation(ctx, db.UpsertLocationParams{
			CompanyID: w.companyID, ZoneID: w.zones[l.Zone], Code: l.Code, Name: l.Name, Kind: l.Kind,
			DepartmentID: ref(w.depts, l.Department), Description: l.Description,
		})
		if err != nil {
			return fmt.Errorf("%s: %w", l.Code, err)
		}
		w.locs[l.Code] = id
	}
	w.res.Counts["locations"] = len(w.locs)
	return nil
}

func (w *writer) employees(ctx context.Context) error {
	w.emps = map[string]uuid.UUID{}
	for _, e := range w.b.Employees {
		id, err := w.q.UpsertEmployee(ctx, db.UpsertEmployeeParams{
			CompanyID: w.companyID, EmployeeCode: e.Code, FirstName: e.FirstName, LastName: e.LastName,
			DisplayName: e.DisplayName, JobTitle: e.JobTitle, DepartmentID: w.depts[e.Department],
			HomeLocationID: ref(w.locs, e.Location), EmploymentType: e.EmploymentType, Status: e.Status,
			IsNpc: *e.NPC, Persona: e.Persona, Greeting: e.Greeting,
		})
		if err != nil {
			return fmt.Errorf("%s: %w", e.Code, err)
		}
		w.emps[e.Code] = id
	}
	for _, e := range w.b.Employees {
		if err := w.q.SetEmployeeManager(ctx, db.SetEmployeeManagerParams{ID: w.emps[e.Code], ManagerID: ref(w.emps, e.Manager)}); err != nil {
			return fmt.Errorf("%s manager: %w", e.Code, err)
		}
	}
	w.res.Counts["employees"] = len(w.emps)
	return nil
}

func (w *writer) departmentLinks(ctx context.Context) error {
	for _, d := range w.b.Departments {
		err := w.q.SetDepartmentLinks(ctx, db.SetDepartmentLinksParams{
			ID: w.depts[d.Code], ParentID: ref(w.depts, d.Parent), HeadEmployeeID: ref(w.emps, d.Head),
		})
		if err != nil {
			return fmt.Errorf("%s: %w", d.Code, err)
		}
	}
	return nil
}

func (w *writer) schedules(ctx context.Context) error {
	if err := w.q.DeleteCompanySchedules(ctx, w.companyID); err != nil {
		return err
	}
	n := 0
	for _, e := range w.b.Employees {
		for _, s := range e.Schedule {
			loc := ref(w.locs, s.Location)
			if loc == nil {
				loc = ref(w.locs, e.Location) // default: home location
			}
			for _, day := range DaysOf(s.Days) {
				err := w.q.InsertSchedule(ctx, db.InsertScheduleParams{
					EmployeeID: w.emps[e.Code], Weekday: day,
					StartsAt: clock(s.From), EndsAt: clock(s.To), LocationID: loc, Activity: s.Activity,
				})
				if err != nil {
					return fmt.Errorf("%s: %w", e.Code, err)
				}
				n++
			}
		}
	}
	w.res.Counts["schedule_blocks"] = n
	return nil
}

func (w *writer) identities(ctx context.Context) error {
	w.idents = map[string]uuid.UUID{}
	upsert := func(p db.UpsertIdentityParams) error {
		p.CompanyID = w.companyID
		p.Email = p.Username + "@" + w.b.Company.EmailDomain
		id, err := w.q.UpsertIdentity(ctx, p)
		if err != nil {
			return fmt.Errorf("%s: %w", p.Username, err)
		}
		w.idents[p.Username] = id
		return nil
	}
	for _, e := range w.b.Employees {
		id := e.Identity
		if id == nil {
			continue
		}
		if err := upsert(db.UpsertIdentityParams{
			EmployeeID: ref(w.emps, e.Code), Username: id.Username, IdentityType: id.Type,
			Status: id.Status, MfaEnabled: id.MFA, Privileged: id.Privileged, ExpiresAt: id.ExpiresAt,
		}); err != nil {
			return err
		}
	}
	for _, s := range w.b.Services {
		if err := upsert(db.UpsertIdentityParams{
			Username: s.Username, IdentityType: "service", Status: s.Status, Privileged: s.Privileged,
		}); err != nil {
			return err
		}
	}
	w.res.Counts["identities"] = len(w.idents)
	return nil
}

func (w *writer) assetsAndNetworks(ctx context.Context) error {
	if err := w.q.ClearAssetAddresses(ctx, w.companyID); err != nil {
		return err
	}
	w.assets = map[string]uuid.UUID{}
	for _, n := range w.b.Networks {
		pfx, err := netip.ParsePrefix(n.CIDR)
		if err != nil {
			return fmt.Errorf("%s: %w", n.Code, err)
		}
		attrs := map[string]any{}
		if n.VLAN != nil {
			attrs["vlan"] = *n.VLAN
		}
		attrsJSON, _ := json.Marshal(attrs)
		id, err := w.q.UpsertAsset(ctx, db.UpsertAssetParams{
			CompanyID: w.companyID, AssetCode: n.Code, Name: n.Name, AssetType: "network",
			NetworkCIDR: &pfx, Criticality: n.Criticality, Status: "active",
			LocationID: ref(w.locs, n.Location), Description: n.Description, Attributes: attrsJSON,
		})
		if err != nil {
			return fmt.Errorf("%s: %w", n.Code, err)
		}
		w.assets[n.Code] = id
	}
	for _, a := range w.b.Assets {
		p := db.UpsertAssetParams{
			CompanyID: w.companyID, AssetCode: a.Code, Name: a.Name, AssetType: a.Type,
			Hostname: optString(a.Hostname), OperatingSystem: optString(a.OS),
			Criticality: a.Criticality, Status: a.Status, DepartmentID: ref(w.depts, a.Department),
			OwnerEmployeeID: ref(w.emps, a.Owner), LocationID: ref(w.locs, a.Location),
			Description: a.Description,
		}
		if a.IP != "" {
			ip, err := netip.ParseAddr(a.IP)
			if err != nil {
				return fmt.Errorf("%s: %w", a.Code, err)
			}
			p.IPAddress = &ip
		}
		attrs := a.Attributes
		if attrs == nil {
			attrs = map[string]any{}
		}
		var err error
		if p.Attributes, err = json.Marshal(attrs); err != nil {
			return fmt.Errorf("%s attributes: %w", a.Code, err)
		}
		id, err := w.q.UpsertAsset(ctx, p)
		if err != nil {
			return fmt.Errorf("%s: %w", a.Code, err)
		}
		w.assets[a.Code] = id
	}
	w.res.Counts["assets"] = len(w.assets)
	return nil
}

func (w *writer) objects(ctx context.Context) error {
	keep := 0
	for _, o := range w.b.World.Objects {
		attrs := o.Attributes
		if attrs == nil {
			attrs = map[string]any{}
		}
		attrsJSON, err := json.Marshal(attrs)
		if err != nil {
			return fmt.Errorf("%s attributes: %w", o.Key, err)
		}
		interactions := o.Interactions
		if interactions == nil {
			interactions = []string{}
		}
		if _, err := w.q.UpsertWorldObject(ctx, db.UpsertWorldObjectParams{
			CompanyID: w.companyID, LocationID: w.locs[o.Location], ObjectKey: o.Key, Name: o.Name,
			Kind: o.Kind, AssetID: ref(w.assets, o.Asset), LeadsToZoneID: ref(w.zones, o.LeadsTo),
			Interactions: interactions, Description: o.Description, Content: o.Content, Attributes: attrsJSON,
		}); err != nil {
			return fmt.Errorf("%s: %w", o.Key, err)
		}
		keep++
	}
	w.res.Counts["world_objects"] = keep
	return nil
}

// node is one endpoint of a graph edge.
type node struct {
	employee, identity, asset *uuid.UUID
}

func (w *writer) relationships(ctx context.Context) error {
	if err := w.q.DeleteCompanyRelationships(ctx, w.companyID); err != nil {
		return err
	}
	n := 0
	edge := func(typ string, from, to node) error {
		n++
		return w.q.InsertRelationship(ctx, db.InsertRelationshipParams{
			CompanyID: w.companyID, RelationshipType: typ,
			SourceEmployeeID: from.employee, SourceIdentityID: from.identity, SourceAssetID: from.asset,
			TargetEmployeeID: to.employee, TargetIdentityID: to.identity, TargetAssetID: to.asset,
		})
	}
	emp := func(code string) node { return node{employee: ref(w.emps, code)} }
	ident := func(u string) node { return node{identity: ref(w.idents, u)} }
	asset := func(code string) node { return node{asset: ref(w.assets, code)} }
	memberships := func(user string, groups, access []string) error {
		for _, g := range groups {
			if err := edge("MEMBER_OF", ident(user), asset(g)); err != nil {
				return err
			}
		}
		for _, a := range access {
			if err := edge("HAS_ACCESS_TO", ident(user), asset(a)); err != nil {
				return err
			}
		}
		return nil
	}

	// Edges derived from structured fields.
	for _, e := range w.b.Employees {
		if e.Manager != "" {
			if err := edge("MANAGES", emp(e.Manager), emp(e.Code)); err != nil {
				return err
			}
		}
		if id := e.Identity; id != nil {
			if err := edge("HAS_IDENTITY", emp(e.Code), ident(id.Username)); err != nil {
				return err
			}
			if err := memberships(id.Username, id.Groups, id.Access); err != nil {
				return err
			}
		}
	}
	for _, s := range w.b.Services {
		if err := memberships(s.Username, s.Groups, s.Access); err != nil {
			return err
		}
	}
	for _, a := range w.b.Assets {
		if a.Owner != "" {
			if err := edge("OWNS", emp(a.Owner), asset(a.Code)); err != nil {
				return err
			}
		}
		if a.Network != "" {
			if err := edge("CONNECTED_TO", asset(a.Code), asset(a.Network)); err != nil {
				return err
			}
		}
		if a.HostedOn != "" {
			if err := edge("HOSTED_ON", asset(a.Code), asset(a.HostedOn)); err != nil {
				return err
			}
		}
		for _, d := range a.DependsOn {
			if err := edge("DEPENDS_ON", asset(a.Code), asset(d)); err != nil {
				return err
			}
		}
	}

	// Explicit edges.
	parse := func(ref string) node {
		kind, code, _ := strings.Cut(ref, ":")
		switch kind {
		case "employee":
			return emp(code)
		case "identity":
			return ident(code)
		default:
			return asset(code)
		}
	}
	for _, r := range w.b.Relations {
		if err := edge(r.Type, parse(r.From), parse(r.To)); err != nil {
			return fmt.Errorf("%s %s %s: %w", r.From, r.Type, r.To, err)
		}
	}
	w.res.Counts["relationships"] = n
	return nil
}

// prune deletes entities no longer present, children before parents.
func (w *writer) prune(ctx context.Context) error {
	q, c := w.q, w.companyID
	steps := []struct {
		name string
		fn   func() (int64, error)
	}{
		{"world_objects", func() (int64, error) {
			return q.DeleteStaleWorldObjects(ctx, db.DeleteStaleWorldObjectsParams{CompanyID: c, Keep: objectKeys(w.b)})
		}},
		{"assets", func() (int64, error) {
			return q.DeleteStaleAssets(ctx, db.DeleteStaleAssetsParams{CompanyID: c, Keep: keys(w.assets)})
		}},
		{"identities", func() (int64, error) {
			return q.DeleteStaleIdentities(ctx, db.DeleteStaleIdentitiesParams{CompanyID: c, Keep: keys(w.idents)})
		}},
		{"employees", func() (int64, error) {
			return q.DeleteStaleEmployees(ctx, db.DeleteStaleEmployeesParams{CompanyID: c, Keep: keys(w.emps)})
		}},
		{"locations", func() (int64, error) {
			return q.DeleteStaleLocations(ctx, db.DeleteStaleLocationsParams{CompanyID: c, Keep: keys(w.locs)})
		}},
		{"zones", func() (int64, error) {
			return q.DeleteStaleZones(ctx, db.DeleteStaleZonesParams{CompanyID: c, Keep: keys(w.zones)})
		}},
		{"departments", func() (int64, error) {
			return q.DeleteStaleDepartments(ctx, db.DeleteStaleDepartmentsParams{CompanyID: c, Keep: keys(w.depts)})
		}},
	}
	for _, s := range steps {
		n, err := s.fn()
		if err != nil {
			return fmt.Errorf("%s: %w", s.name, err)
		}
		if n > 0 {
			w.res.Pruned[s.name] = n
		}
	}
	return nil
}

func ref(m map[string]uuid.UUID, code string) *uuid.UUID {
	if code == "" {
		return nil
	}
	id, ok := m[code]
	if !ok {
		return nil
	}
	return &id
}

func keys(m map[string]uuid.UUID) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func objectKeys(b *Bundle) []string {
	out := make([]string, len(b.World.Objects))
	for i, o := range b.World.Objects {
		out[i] = o.Key
	}
	return out
}

func optString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// clock converts a validated "HH:MM" to a PostgreSQL time.
func clock(hhmm string) pgtype.Time {
	t, _ := time.Parse("15:04", hhmm)
	us := int64(t.Hour())*int64(time.Hour/time.Microsecond) + int64(t.Minute())*int64(time.Minute/time.Microsecond)
	return pgtype.Time{Microseconds: us, Valid: true}
}
