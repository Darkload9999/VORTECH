// Package player owns platform users mirrored from Keycloak and their game
// profiles (players).
//
// Directory maps a verified token subject to internal records, creating
// them on first sight; Handler serves the caller's own profile. Per-player
// game permissions (e.g. unlocked floors) and progression belong here and
// live in PostgreSQL, never in Keycloak.
package player
