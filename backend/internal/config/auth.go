package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"
)

// AuthConfig configures validation of Keycloak-issued access tokens. Only
// the API needs it, so it is loaded separately from the shared Config.
type AuthConfig struct {
	// KeycloakURL is Keycloak's public base URL, including any relative path
	// (e.g. https://vortech.example/auth). It determines the expected
	// token issuer, so it must match what browsers use.
	KeycloakURL string
	// KeycloakInternalURL optionally overrides the base URL used for
	// server-to-server calls (JWKS), e.g. http://keycloak:8080/auth inside
	// the cluster. Defaults to KeycloakURL.
	KeycloakInternalURL string
	Realm               string
	// ClientID is the frontend's public OIDC client. Tokens must have been
	// issued to it (azp claim).
	ClientID string
	// Audience must appear in the token's aud claim; it identifies this API.
	Audience string
	// ClockSkew tolerated when checking exp/nbf/iat.
	ClockSkew time.Duration
	// JWKSRefreshInterval controls proactive signing-key refreshes.
	JWKSRefreshInterval time.Duration
	// IdentityCacheTTL bounds how long a token subject → internal user
	// mapping (and account status) is cached in memory.
	IdentityCacheTTL time.Duration
}

// Issuer returns the expected iss claim.
func (a AuthConfig) Issuer() string {
	return strings.TrimRight(a.KeycloakURL, "/") + "/realms/" + url.PathEscape(a.Realm)
}

// JWKSURL returns the realm's signing-key endpoint on the internal base URL.
func (a AuthConfig) JWKSURL() string {
	base := a.KeycloakInternalURL
	if base == "" {
		base = a.KeycloakURL
	}
	return strings.TrimRight(base, "/") + "/realms/" + url.PathEscape(a.Realm) + "/protocol/openid-connect/certs"
}

// LoadAuth reads and validates the authentication configuration. env is
// the APP_ENV value (production enforces HTTPS for the issuer).
func LoadAuth(lookup LookupFunc, env string) (AuthConfig, error) {
	p := parser{lookup: lookup}
	cfg := AuthConfig{
		KeycloakURL:         p.str("KEYCLOAK_URL", ""),
		KeycloakInternalURL: p.str("KEYCLOAK_INTERNAL_URL", ""),
		Realm:               p.str("KEYCLOAK_REALM", ""),
		ClientID:            p.str("KEYCLOAK_CLIENT_ID", ""),
		Audience:            p.str("KEYCLOAK_AUDIENCE", "vortech-api"),
		ClockSkew:           p.duration("AUTH_CLOCK_SKEW", 30*time.Second),
		JWKSRefreshInterval: p.duration("AUTH_JWKS_REFRESH_INTERVAL", 15*time.Minute),
		IdentityCacheTTL:    p.duration("AUTH_IDENTITY_CACHE_TTL", 2*time.Minute),
	}

	errs := p.errs
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	if cfg.KeycloakURL == "" {
		add("KEYCLOAK_URL: required")
	} else if err := validateBaseURL(cfg.KeycloakURL, strings.EqualFold(env, EnvProduction)); err != nil {
		add("KEYCLOAK_URL: %v", err)
	}
	if cfg.KeycloakInternalURL != "" {
		if err := validateBaseURL(cfg.KeycloakInternalURL, false); err != nil {
			add("KEYCLOAK_INTERNAL_URL: %v", err)
		}
	}
	if cfg.Realm == "" {
		add("KEYCLOAK_REALM: required")
	}
	if cfg.ClientID == "" {
		add("KEYCLOAK_CLIENT_ID: required")
	}
	if cfg.Audience == "" {
		add("KEYCLOAK_AUDIENCE: must not be empty")
	}
	if cfg.ClockSkew < 0 || cfg.ClockSkew > 2*time.Minute {
		add("AUTH_CLOCK_SKEW: must be between 0s and 2m")
	}
	if cfg.JWKSRefreshInterval < time.Minute {
		add("AUTH_JWKS_REFRESH_INTERVAL: must be at least 1m")
	}
	if cfg.IdentityCacheTTL < 0 || cfg.IdentityCacheTTL > 15*time.Minute {
		add("AUTH_IDENTITY_CACHE_TTL: must be between 0s and 15m (it bounds how long a suspension takes to apply)")
	}

	if len(errs) > 0 {
		return AuthConfig{}, fmt.Errorf("invalid auth configuration:\n%w", errors.Join(errs...))
	}
	return cfg, nil
}

func validateBaseURL(raw string, requireHTTPS bool) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return fmt.Errorf("must be an absolute URL (got %q)", raw)
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return fmt.Errorf("scheme must be http or https (got %q)", u.Scheme)
	}
	if requireHTTPS && u.Scheme != "https" {
		return errors.New("must use https in production")
	}
	if u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return errors.New("must not contain credentials, query or fragment")
	}
	return nil
}

// LogValue implements slog.LogValuer.
func (a AuthConfig) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("issuer", a.Issuer()),
		slog.String("jwks_url", a.JWKSURL()),
		slog.String("client_id", a.ClientID),
		slog.String("audience", a.Audience),
		slog.Duration("clock_skew", a.ClockSkew),
		slog.Duration("identity_cache_ttl", a.IdentityCacheTTL),
	)
}
