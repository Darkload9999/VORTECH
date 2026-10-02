// Package auth authenticates callers using Keycloak-issued OIDC access
// tokens and authorises them with platform roles.
//
// Keycloak is the only authority for real platform identities. The browser
// obtains tokens with the Authorization Code flow + PKCE; this package only
// validates them (JWKS signature, issuer, audience, authorised party, token
// type, expiry) and never handles passwords. Verified subjects are mapped
// to internal users by an IdentityResolver (implemented by the player
// module), and handlers declare the Permission they need via
// Authenticator.Protect.
package auth
