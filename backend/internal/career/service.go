package career

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/Darkload9999/VORTECH/backend/internal/auth"
	"github.com/Darkload9999/VORTECH/backend/internal/database/db"
	"github.com/Darkload9999/VORTECH/backend/internal/world"
)

// Unlock is an explicit zone grant.
type Unlock struct {
	ZoneID     uuid.UUID `json:"zone_id"`
	ZoneCode   string    `json:"zone_code"`
	Source     string    `json:"source"`
	UnlockedAt time.Time `json:"unlocked_at"`
}

// State is a player's progression within one company's world.
type State struct {
	PlayerID      uuid.UUID
	XP            int32
	Level         Level
	Next          *Level
	CurrentZoneID *uuid.UUID // nil if outside the world (or in another world)
	Unlocks       []Unlock
	Access        *world.Access
}

// Service owns player progression: XP, career levels and zone access.
type Service struct {
	q *db.Queries

	mu     sync.Mutex
	levels Levels
}

// NewService returns a Service reading through q.
func NewService(q *db.Queries) *Service {
	return &Service{q: q}
}

// Levels returns the career ladder. Levels are platform seed data, so they
// are loaded once per process.
func (s *Service) Levels(ctx context.Context) (Levels, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.levels != nil {
		return s.levels, nil
	}
	rows, err := s.q.ListCareerLevels(ctx)
	if err != nil {
		return nil, fmt.Errorf("load career levels: %w", err)
	}
	ls := make([]Level, len(rows))
	for i, r := range rows {
		ls[i] = Level{Rank: r.Rank, Code: r.Code, Name: r.Name, Description: r.Description, MinXP: r.MinXp}
	}
	if len(ls) == 0 {
		return nil, errors.New("no career levels configured")
	}
	s.levels = NewLevels(ls)
	return s.levels, nil
}

// Load reads a player's state in a company's world, creating the progress
// record on first use. With lock, the progress row is locked FOR UPDATE so
// q must be bound to a transaction; state changes are then serialised per
// player.
func (s *Service) Load(ctx context.Context, q *db.Queries, companyID, playerID uuid.UUID, lock bool) (State, error) {
	levels, err := s.Levels(ctx)
	if err != nil {
		return State{}, err
	}
	if err := q.EnsurePlayerProgress(ctx, playerID); err != nil {
		return State{}, fmt.Errorf("ensure progress: %w", err)
	}
	var row db.GetPlayerProgressRow
	if lock {
		r, err := q.LockPlayerProgress(ctx, playerID)
		if err != nil {
			return State{}, fmt.Errorf("lock progress: %w", err)
		}
		row = db.GetPlayerProgressRow(r)
	} else if row, err = q.GetPlayerProgress(ctx, playerID); err != nil {
		return State{}, fmt.Errorf("load progress: %w", err)
	}

	zones, err := q.ListZoneRules(ctx, companyID)
	if err != nil {
		return State{}, fmt.Errorf("load zones: %w", err)
	}
	rules := make([]world.ZoneRule, len(zones))
	for i, z := range zones {
		rules[i] = world.ZoneRule{ID: z.ID, Code: z.Code, Name: z.Name, ParentID: z.ParentID,
			UnlockedByDefault: z.UnlockedByDefault, RequiredCareerRank: z.RequiredCareerRank}
	}

	unlockRows, err := q.ListZoneUnlocks(ctx, db.ListZoneUnlocksParams{PlayerID: playerID, CompanyID: companyID})
	if err != nil {
		return State{}, fmt.Errorf("load unlocks: %w", err)
	}
	unlocked := make(map[uuid.UUID]bool, len(unlockRows))
	unlocks := make([]Unlock, len(unlockRows))
	for i, u := range unlockRows {
		unlocked[u.ZoneID] = true
		unlocks[i] = Unlock{ZoneID: u.ZoneID, ZoneCode: u.ZoneCode, Source: u.Source, UnlockedAt: u.UnlockedAt.UTC()}
	}

	level, next := levels.ForXP(row.Xp)
	st := State{
		PlayerID: playerID, XP: row.Xp, Level: level, Next: next, Unlocks: unlocks,
		Access: world.NewAccess(rules, unlocked, level.Rank),
	}
	if row.CurrentZoneID != nil {
		if _, ok := st.Access.Zone(*row.CurrentZoneID); ok {
			st.CurrentZoneID = row.CurrentZoneID
		}
	}
	return st, nil
}

// ZoneAccess implements world.AccessResolver: per-zone decisions for the
// caller, or nil for principals without a player profile.
func (s *Service) ZoneAccess(ctx context.Context, companyID uuid.UUID, p *auth.Principal) (map[uuid.UUID]world.Decision, error) {
	if p == nil || p.PlayerID == nil {
		return nil, nil
	}
	st, err := s.Load(ctx, s.q, companyID, *p.PlayerID, false)
	if err != nil {
		return nil, err
	}
	return st.Access.All(), nil
}

// Grant explicitly unlocks a zone for a player (engagement rewards and
// instructor/admin tooling in later phases).
func (s *Service) Grant(ctx context.Context, playerID, zoneID uuid.UUID, source string) error {
	if err := s.q.GrantZoneUnlock(ctx, db.GrantZoneUnlockParams{PlayerID: playerID, ZoneID: zoneID, Source: source}); err != nil {
		return fmt.Errorf("grant zone unlock: %w", err)
	}
	return nil
}
