// Package event provides the in-process event bus (player.*, npc.*, range.*,
// security.*). Its interfaces are designed so NATS can replace the
// in-memory transport later without changing publishers or subscribers.
//
// Implemented in phase 4.
package event
