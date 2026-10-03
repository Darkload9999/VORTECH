package websocket

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/Darkload9999/VORTECH/backend/internal/auth"
	"github.com/Darkload9999/VORTECH/backend/internal/company"
	"github.com/Darkload9999/VORTECH/backend/internal/database/db"
	"github.com/Darkload9999/VORTECH/backend/internal/event"
	"github.com/Darkload9999/VORTECH/backend/internal/world"
)

// WorldAuthorizer implements Authorizer against the active world: zone
// topics require the zone to exist in that world and either staff
// visibility (world:inspect:locked) or the player's access to the zone.
type WorldAuthorizer struct {
	Dir    *company.Directory
	Q      *db.Queries
	Access world.AccessResolver
}

// WorldTopic implements Authorizer.
func (a WorldAuthorizer) WorldTopic(ctx context.Context) (string, error) {
	c, err := a.Dir.Active(ctx)
	if errors.Is(err, company.ErrNoActiveWorld) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return event.WorldTopic(c.ID), nil
}

// CanSubscribeZone implements Authorizer.
func (a WorldAuthorizer) CanSubscribeZone(ctx context.Context, p *auth.Principal, zoneID uuid.UUID) (bool, error) {
	c, err := a.Dir.Active(ctx)
	if errors.Is(err, company.ErrNoActiveWorld) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	zones, err := a.Q.ListZoneRules(ctx, c.ID)
	if err != nil {
		return false, fmt.Errorf("list zones: %w", err)
	}
	exists := false
	for _, z := range zones {
		if z.ID == zoneID {
			exists = true
			break
		}
	}
	if !exists {
		return false, nil
	}
	if p.Can(auth.PermWorldInspectLocked) {
		return true, nil
	}
	decisions, err := a.Access.ZoneAccess(ctx, c.ID, p)
	if err != nil || decisions == nil {
		return false, err
	}
	return decisions[zoneID].Accessible, nil
}
