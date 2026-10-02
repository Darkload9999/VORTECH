// Package auth validates Keycloak-issued OIDC access tokens (JWT, JWKS),
// maps the Keycloak sub claim to internal users and enforces platform RBAC
// (PLAYER, INSTRUCTOR, SCENARIO_CREATOR, ADMIN). Passwords are never handled
// by Go.
//
// Implemented in phase 2.
package auth
