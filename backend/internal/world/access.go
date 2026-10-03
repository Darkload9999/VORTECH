package world

import (
	"github.com/google/uuid"
)

// Denial reasons for zone access.
const (
	ReasonZoneLocked       = "ZONE_LOCKED"
	ReasonInsufficientRank = "INSUFFICIENT_RANK"
	ReasonParentLocked     = "PARENT_ZONE_LOCKED"
	ReasonUnknownZone      = "UNKNOWN_ZONE"
)

// ZoneRule is the progression gate of one zone.
type ZoneRule struct {
	ID                 uuid.UUID
	Code               string
	Name               string
	ParentID           *uuid.UUID
	UnlockedByDefault  bool
	RequiredCareerRank int32
}

// Decision is the outcome of an access check. Reason is empty when allowed.
type Decision struct {
	Accessible   bool   `json:"accessible"`
	Reason       string `json:"reason,omitempty"`
	RequiredRank int32  `json:"required_rank,omitempty"`
}

// Access evaluates zone access for one player. It is pure: callers load the
// zone rules, the player's explicit unlocks and rank, then ask questions.
//
// A zone is accessible when
//
//	(explicitly unlocked
//	  OR (rank >= required_career_rank
//	      AND (unlocked_by_default OR required_career_rank > 0)))
//	AND its parent zone is accessible.
//
// Zones are therefore open by default, rank-gated (opening automatically at
// the required rank) or explicit-only (unlocked by an engagement,
// instructor or admin; explicit unlocks bypass the rank requirement).
type Access struct {
	zones    map[uuid.UUID]ZoneRule
	unlocked map[uuid.UUID]bool
	rank     int32
	memo     map[uuid.UUID]Decision
}

// NewAccess builds an evaluator.
func NewAccess(zones []ZoneRule, unlocked map[uuid.UUID]bool, rank int32) *Access {
	m := make(map[uuid.UUID]ZoneRule, len(zones))
	for _, z := range zones {
		m[z.ID] = z
	}
	return &Access{zones: m, unlocked: unlocked, rank: rank, memo: map[uuid.UUID]Decision{}}
}

// Zone returns the rule for id, if the zone belongs to this world.
func (a *Access) Zone(id uuid.UUID) (ZoneRule, bool) {
	z, ok := a.zones[id]
	return z, ok
}

// Decide reports whether the player may be in zone id.
func (a *Access) Decide(id uuid.UUID) Decision {
	return a.decide(id, map[uuid.UUID]bool{})
}

func (a *Access) decide(id uuid.UUID, visiting map[uuid.UUID]bool) Decision {
	if d, ok := a.memo[id]; ok {
		return d
	}
	z, ok := a.zones[id]
	if !ok || visiting[id] {
		// Unknown zone, or a parent cycle (prevented at import): deny.
		return Decision{Reason: ReasonUnknownZone}
	}
	visiting[id] = true

	d := a.own(z)
	if d.Accessible && z.ParentID != nil {
		if p := a.decide(*z.ParentID, visiting); !p.Accessible {
			d = Decision{Reason: ReasonParentLocked}
		}
	}
	a.memo[id] = d
	return d
}

func (a *Access) own(z ZoneRule) Decision {
	if a.unlocked[z.ID] {
		return Decision{Accessible: true}
	}
	if !z.UnlockedByDefault && z.RequiredCareerRank == 0 {
		return Decision{Reason: ReasonZoneLocked}
	}
	if a.rank < z.RequiredCareerRank {
		return Decision{Reason: ReasonInsufficientRank, RequiredRank: z.RequiredCareerRank}
	}
	return Decision{Accessible: true}
}

// All decides every zone of the world.
func (a *Access) All() map[uuid.UUID]Decision {
	out := make(map[uuid.UUID]Decision, len(a.zones))
	for id := range a.zones {
		out[id] = a.Decide(id)
	}
	return out
}
