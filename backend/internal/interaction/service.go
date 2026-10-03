package interaction

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"slices"
	"sort"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Darkload9999/VORTECH/backend/internal/career"
	"github.com/Darkload9999/VORTECH/backend/internal/company"
	"github.com/Darkload9999/VORTECH/backend/internal/database/db"
	"github.com/Darkload9999/VORTECH/backend/internal/httpx"
	"github.com/Darkload9999/VORTECH/backend/internal/requestid"
	"github.com/Darkload9999/VORTECH/backend/internal/world"
)

// DiscoveryXP is awarded the first time a player finds an asset.
const DiscoveryXP = 10

// Denial reasons beyond the world access reasons.
const (
	ReasonNotInZone          = "NOT_IN_ZONE"
	ReasonNotSupported       = "INTERACTION_NOT_SUPPORTED"
	ReasonNPCUnavailable     = "NPC_UNAVAILABLE"
	ReasonRequiresEngagement = "REQUIRES_ENGAGEMENT"
)

var messages = map[string]string{
	world.ReasonZoneLocked:       "That area is locked.",
	world.ReasonInsufficientRank: "Your career rank is too low for that area.",
	world.ReasonParentLocked:     "You cannot reach that area yet.",
	world.ReasonUnknownZone:      "That area does not exist in this world.",
	ReasonNotInZone:              "You need to be in the same area to do that.",
	ReasonNotSupported:           "That object does not support this action.",
	ReasonNPCUnavailable:         "That person is not available right now.",
	// Engagements and cyber ranges arrive with range management; until a
	// player has an active, authorised engagement these are always denied.
	ReasonRequiresEngagement: "No authorised engagement is active for this.",
}

// Outcome is the server's verdict on one interaction.
type Outcome struct {
	InteractionID uuid.UUID `json:"interaction_id"`
	Type          string    `json:"type"`
	Outcome       string    `json:"outcome"` // allowed | denied
	Reason        *string   `json:"reason"`
	Message       string    `json:"message,omitempty"`
	Result        any       `json:"result"`
	XPAwarded     int32     `json:"xp_awarded"`
	Progress      Progress  `json:"progress"`
}

// Progress is the player's state after the interaction.
type Progress struct {
	XP                   int32           `json:"xp"`
	Level                career.Level    `json:"level"`
	LevelUp              bool            `json:"level_up"`
	CurrentZone          *career.ZoneRef `json:"current_zone"`
	NewlyAccessibleZones []string        `json:"newly_accessible_zones"`
}

// Service validates and applies interactions.
type Service struct {
	pool   *pgxpool.Pool
	dir    *company.Directory
	career *career.Service
}

// NewService returns a Service.
func NewService(pool *pgxpool.Pool, dir *company.Directory, cs *career.Service) *Service {
	return &Service{pool: pool, dir: dir, career: cs}
}

// Process decides and applies one interaction for player. Rule denials are
// normal outcomes; returned errors are *httpx.Error (unknown targets) or
// internal failures.
func (s *Service) Process(ctx context.Context, playerID uuid.UUID, req Request) (*Outcome, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	c, err := s.dir.Active(ctx)
	if err != nil {
		return nil, err
	}

	var out *Outcome
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := db.New(tx)
		st, err := s.career.Load(ctx, q, c.ID, playerID, true)
		if err != nil {
			return err
		}
		e := &exec{q: q, st: st, companyID: c.ID, playerID: playerID, req: req, record: recordFor(req)}
		if err := e.run(ctx); err != nil {
			return err
		}
		out, err = e.finish(ctx, s.career)
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// exec carries one interaction through the transaction.
type exec struct {
	q         *db.Queries
	st        career.State
	companyID uuid.UUID
	playerID  uuid.UUID
	req       Request

	record     db.InsertInteractionParams
	reason     string
	result     any
	xp         int32
	newZone    *uuid.UUID // set when the current zone changes
	zoneChange bool
}

func recordFor(req Request) db.InsertInteractionParams {
	return db.InsertInteractionParams{InteractionType: req.Type, ZoneID: req.ZoneID, EmployeeID: req.EmployeeID}
}

func (e *exec) deny(reason string) { e.reason = reason }

func (e *exec) inZone(zoneID uuid.UUID) bool {
	return e.st.CurrentZoneID != nil && *e.st.CurrentZoneID == zoneID
}

func (e *exec) run(ctx context.Context) error {
	switch targets[e.req.Type] {
	case targetZone:
		return e.zone(ctx)
	case targetEmployee:
		return e.npc(ctx)
	default:
		return e.object(ctx)
	}
}

func (e *exec) zone(_ context.Context) error {
	id := *e.req.ZoneID
	z, ok := e.st.Access.Zone(id)
	if !ok {
		return notFound("Zone not found.")
	}
	switch e.req.Type {
	case EnterZone:
		if d := e.st.Access.Decide(id); !d.Accessible {
			e.deny(d.Reason)
			return nil
		}
		e.moveTo(&id)
		e.result = map[string]any{"zone": zoneRef(z)}
	case LeaveZone:
		if !e.inZone(id) {
			e.deny(ReasonNotInZone)
			return nil
		}
		// Leaving a zone puts the player in its parent (or outside the world).
		e.moveTo(z.ParentID)
		var parent *career.ZoneRef
		if z.ParentID != nil {
			if p, ok := e.st.Access.Zone(*z.ParentID); ok {
				parent = zoneRef(p)
			}
		}
		e.result = map[string]any{"zone": parent}
	}
	return nil
}

func (e *exec) moveTo(zone *uuid.UUID) {
	e.newZone, e.zoneChange = zone, true
}

func (e *exec) npc(ctx context.Context) error {
	n, err := e.q.GetInteractionNPC(ctx, db.GetInteractionNPCParams{CompanyID: e.companyID, ID: *e.req.EmployeeID})
	if errors.Is(err, pgx.ErrNoRows) {
		return notFound("Employee not found.")
	}
	if err != nil {
		return fmt.Errorf("load npc: %w", err)
	}
	e.record.ZoneID = n.ZoneID
	switch {
	case !n.IsNpc || n.Status != "active":
		e.deny(ReasonNPCUnavailable)
	case n.ZoneID == nil || !e.inZone(*n.ZoneID):
		e.deny(ReasonNotInZone)
	default:
		e.result = map[string]any{
			"employee": map[string]any{"id": n.ID, "code": n.EmployeeCode, "display_name": n.DisplayName, "job_title": n.JobTitle},
			"greeting": n.Greeting,
		}
	}
	return nil
}

// assetView is the digital asset revealed by inspecting or using an object.
type assetView struct {
	ID              uuid.UUID   `json:"id"`
	Code            string      `json:"code"`
	Name            string      `json:"name"`
	Type            string      `json:"type"`
	Hostname        *string     `json:"hostname"`
	IPAddress       *netip.Addr `json:"ip_address"`
	OperatingSystem *string     `json:"operating_system"`
	Criticality     *string     `json:"criticality"`
	OwnerCode       *string     `json:"owner_code"`
	OwnerName       *string     `json:"owner_name"`
}

func (e *exec) object(ctx context.Context) error {
	o, err := e.q.GetInteractionObject(ctx, db.GetInteractionObjectParams{CompanyID: e.companyID, ObjectKey: e.req.ObjectKey})
	if errors.Is(err, pgx.ErrNoRows) {
		return notFound("Object not found.")
	}
	if err != nil {
		return fmt.Errorf("load object: %w", err)
	}
	e.record.ObjectID, e.record.ZoneID = &o.ID, &o.ZoneID

	switch {
	case !slices.Contains(o.Interactions, e.req.Type):
		e.deny(ReasonNotSupported)
		return nil
	case !e.inZone(o.ZoneID):
		e.deny(ReasonNotInZone)
		return nil
	}
	// Re-check the zone itself: access may have been revoked since entry.
	if d := e.st.Access.Decide(o.ZoneID); !d.Accessible {
		e.deny(d.Reason)
		return nil
	}

	obj := map[string]any{"key": o.ObjectKey, "name": o.Name, "kind": o.Kind, "description": o.Description}
	switch e.req.Type {
	case OpenDoor:
		if o.LeadsToZoneID == nil {
			e.deny(ReasonNotSupported) // not a door (prevented by scenario validation)
			return nil
		}
		target, ok := e.st.Access.Zone(*o.LeadsToZoneID)
		if !ok {
			e.deny(world.ReasonUnknownZone)
			return nil
		}
		if d := e.st.Access.Decide(target.ID); !d.Accessible {
			e.deny(d.Reason)
			return nil
		}
		e.result = map[string]any{"object": obj, "leads_to": zoneRef(target)}
	case CloseDoor:
		e.result = map[string]any{"object": obj}
	case ReadDocument:
		e.result = map[string]any{"object": obj, "content": o.Content}
	case UseWorkstation, InspectObject:
		res := map[string]any{"object": obj, "asset": nil, "discovery": nil}
		if o.AssetID != nil {
			res["asset"] = assetView{
				ID: *o.AssetID, Code: deref(o.AssetCode), Name: deref(o.AssetName), Type: deref(o.AssetType),
				Hostname: o.Hostname, IPAddress: o.IPAddress, OperatingSystem: o.OperatingSystem,
				Criticality: o.Criticality, OwnerCode: o.OwnerCode, OwnerName: o.OwnerName,
			}
			isNew, err := e.discover(ctx, *o.AssetID, o.ID)
			if err != nil {
				return err
			}
			res["discovery"] = map[string]any{"asset_code": deref(o.AssetCode), "new": isNew}
		}
		e.result = res
	case AccessTerminal, StartEngagement:
		e.deny(ReasonRequiresEngagement)
	}
	return nil
}

// discover records the first sighting of an asset and awards XP.
func (e *exec) discover(ctx context.Context, assetID, objectID uuid.UUID) (bool, error) {
	_, err := e.q.InsertDiscovery(ctx, db.InsertDiscoveryParams{
		PlayerID: e.playerID, AssetID: assetID, ObjectID: &objectID, DiscoveredVia: e.req.Type,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("record discovery: %w", err)
	}
	e.xp += DiscoveryXP
	return true, nil
}

// finish applies state changes, records the interaction and builds the outcome.
func (e *exec) finish(ctx context.Context, cs *career.Service) (*Outcome, error) {
	out := &Outcome{Type: e.req.Type, Outcome: "allowed", Result: e.result}
	if e.reason != "" {
		out.Outcome, out.Result, out.Message = "denied", nil, messages[e.reason]
		r := e.reason
		out.Reason = &r
		e.record.Reason = &r
		e.xp, e.zoneChange = 0, false
	}

	current := e.st.CurrentZoneID
	if e.zoneChange {
		if err := e.q.SetCurrentZone(ctx, db.SetCurrentZoneParams{PlayerID: e.playerID, ZoneID: e.newZone}); err != nil {
			return nil, fmt.Errorf("set current zone: %w", err)
		}
		current = e.newZone
	}

	xp := e.st.XP
	if e.xp > 0 {
		newXP, err := e.q.AddPlayerXP(ctx, db.AddPlayerXPParams{PlayerID: e.playerID, Xp: e.xp})
		if err != nil {
			return nil, fmt.Errorf("award xp: %w", err)
		}
		xp = newXP
	}
	out.XPAwarded = e.xp

	levels, err := cs.Levels(ctx)
	if err != nil {
		return nil, err
	}
	level, _ := levels.ForXP(xp)
	out.Progress = Progress{XP: xp, Level: level, LevelUp: level.Rank > e.st.Level.Rank, NewlyAccessibleZones: []string{}}
	if out.Progress.LevelUp {
		out.Progress.NewlyAccessibleZones = e.newlyAccessible(level.Rank)
	}
	if current != nil {
		if z, ok := e.st.Access.Zone(*current); ok {
			out.Progress.CurrentZone = zoneRef(z)
		}
	}

	e.record.PlayerID, e.record.CompanyID = e.playerID, e.companyID
	e.record.Outcome, e.record.XpAwarded = out.Outcome, e.xp
	if id := requestid.From(ctx); id != "" {
		e.record.RequestID = &id
	}
	row, err := e.q.InsertInteraction(ctx, e.record)
	if err != nil {
		return nil, fmt.Errorf("record interaction: %w", err)
	}
	out.InteractionID = row.ID
	return out, nil
}

// newlyAccessible lists zones that open at the new rank.
func (e *exec) newlyAccessible(newRank int32) []string {
	before := e.st.Access.All()
	rules := make([]world.ZoneRule, 0, len(before))
	unlocked := map[uuid.UUID]bool{}
	for id := range before {
		z, _ := e.st.Access.Zone(id)
		rules = append(rules, z)
	}
	for _, u := range e.st.Unlocks {
		unlocked[u.ZoneID] = true
	}
	after := world.NewAccess(rules, unlocked, newRank)
	var out []string
	for id, d := range before {
		if !d.Accessible && after.Decide(id).Accessible {
			z, _ := e.st.Access.Zone(id)
			out = append(out, z.Code)
		}
	}
	sort.Strings(out)
	if out == nil {
		out = []string{}
	}
	return out
}

func zoneRef(z world.ZoneRule) *career.ZoneRef {
	return &career.ZoneRef{ID: z.ID, Code: z.Code, Name: z.Name}
}

func notFound(msg string) error {
	return httpx.NewError(http.StatusNotFound, httpx.CodeNotFound, msg)
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
