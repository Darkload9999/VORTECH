// Package career owns player progression: XP, the career ladder (rank is
// derived from XP), the authoritative current zone and explicit zone
// unlocks, and serves GET /api/v1/me/progress. Progression lives in
// PostgreSQL, never in Keycloak.
package career
