// Package interaction validates and applies player interactions with the
// world (ENTER_ZONE, OPEN_DOOR, USE_WORKSTATION, TALK_TO_NPC, ...).
//
// The client only states intent; every decision is made here from
// server-side state: zone access (career rank and explicit unlocks), the
// player's authoritative current zone, the interactions each object allows
// and NPC availability. Each attempt is recorded with its outcome.
package interaction
