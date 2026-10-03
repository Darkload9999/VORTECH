// Package event is the platform's internal event bus.
//
// Modules publish facts ("asset.discovered", "career.level_up") after the
// database transaction that produced them commits; subscribers (the
// WebSocket gateway, later the NPC engine and telemetry) react
// asynchronously. Publishers and subscribers depend only on the Publisher
// and Subscriber interfaces and on JSON-encoded events, so the in-memory
// Bus can be replaced by NATS without changing business logic.
//
// Delivery is at-most-once and in-process. Anything a player must not miss
// is persisted (e.g. notifications) and re-read over REST after reconnect.
package event

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Event types published by the platform.
const (
	PlayerConnected    = "player.connected"
	PlayerDisconnected = "player.disconnected"
	PlayerEnteredZone  = "player.entered_zone"
	PlayerInteracted   = "player.interacted"
	AssetDiscovered    = "asset.discovered"
	CareerLevelUp      = "career.level_up"
	NotificationNew    = "notification.created"
	WorldAnnouncement  = "world.announcement"
)

// Event is one fact. Topic routes it to WebSocket subscribers; an empty
// topic means the event is internal and never leaves the server.
type Event struct {
	ID        uuid.UUID       `json:"id"`
	Type      string          `json:"type"`
	Topic     string          `json:"topic,omitempty"`
	Timestamp time.Time       `json:"timestamp"`
	Data      json.RawMessage `json:"data"`
}

// New builds an event with a time-ordered ID.
func New(typ, topic string, data any) (Event, error) {
	raw, err := json.Marshal(data)
	if err != nil {
		return Event{}, fmt.Errorf("encode %s event: %w", typ, err)
	}
	id, err := uuid.NewV7()
	if err != nil {
		return Event{}, err
	}
	return Event{ID: id, Type: typ, Topic: topic, Timestamp: time.Now().UTC(), Data: raw}, nil
}

// Must is New for statically encodable payloads.
func Must(typ, topic string, data any) Event {
	e, err := New(typ, topic, data)
	if err != nil {
		panic(err)
	}
	return e
}

// Topics. Player topics are private: only that player's connections may
// receive them.
func PlayerTopic(playerID uuid.UUID) string { return "player:" + playerID.String() }

// WorldTopic carries events for everyone in a company's world.
func WorldTopic(companyID uuid.UUID) string { return "world:" + companyID.String() }

// ZoneTopic carries events for players in (or allowed into) a zone.
func ZoneTopic(zoneID uuid.UUID) string { return "zone:" + zoneID.String() }

// Publisher emits events. Publish never blocks on slow subscribers.
type Publisher interface {
	Publish(ctx context.Context, events ...Event)
}

// Handler processes one event. It runs on the subscription's goroutine.
type Handler func(ctx context.Context, e Event)

// Subscriber registers handlers. The returned function unsubscribes.
type Subscriber interface {
	Subscribe(name string, filter func(Event) bool, h Handler) (unsubscribe func())
}
