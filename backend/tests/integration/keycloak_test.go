//go:build integration

package integration

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/vortech/backend/internal/auth"
	"github.com/vortech/backend/internal/config"
	"github.com/vortech/backend/internal/devauth"
)

// These tests exercise the real login path: Keycloak's Authorization Code +
// PKCE flow, real signing keys from its JWKS endpoint, and the API's
// verifier, identity mapping and RBAC on a real database.
//
// They need TEST_KEYCLOAK_URL (the dev realm from deployments/local) and are
// skipped when it is unset or Keycloak is not reachable.

const realmFile = "../../deployments/local/keycloak/realm/vortech-realm.json"

func keycloakAuthConfig(t *testing.T) config.AuthConfig {
	t.Helper()
	kc := os.Getenv("TEST_KEYCLOAK_URL")
	if kc == "" {
		t.Skip("TEST_KEYCLOAK_URL not set")
	}
	cfg, err := config.LoadAuth(func(k string) (string, bool) {
		v, ok := map[string]string{
			"KEYCLOAK_URL":       kc,
			"KEYCLOAK_REALM":     "vortech",
			"KEYCLOAK_CLIENT_ID": "vortech-web",
		}[k]
		return v, ok
	}, config.EnvTest)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Get(cfg.JWKSURL())
	if err != nil {
		t.Skipf("Keycloak not reachable at %s: %v (run `make deps-up`)", kc, err)
	}
	resp.Body.Close()
	return cfg
}

func devLogin(t *testing.T, cfg config.AuthConfig, user string) *devauth.Tokens {
	t.Helper()
	pw, err := devauth.DevPassword(realmFile, user)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	tok, err := devauth.Login(ctx, loginConfig(cfg), user, pw)
	if err != nil {
		t.Fatalf("PKCE login as %s: %v", user, err)
	}
	return tok
}

func loginConfig(cfg config.AuthConfig) devauth.Config {
	return devauth.Config{
		KeycloakURL: cfg.KeycloakURL,
		Realm:       cfg.Realm,
		ClientID:    cfg.ClientID,
		RedirectURI: "http://localhost:5173/callback",
	}
}

func realVerifier(t *testing.T, cfg config.AuthConfig) *auth.Verifier {
	t.Helper()
	ks := auth.NewKeySet(cfg.JWKSURL(), testLogger())
	if err := ks.Refresh(context.Background()); err != nil {
		t.Fatalf("load Keycloak JWKS: %v", err)
	}
	return auth.NewVerifier(ks, auth.VerifierConfig{
		Issuer: cfg.Issuer(), Audience: cfg.Audience, ClientID: cfg.ClientID, Leeway: cfg.ClockSkew,
	})
}

func TestKeycloakLoginMapsPlayerAndServesMe(t *testing.T) {
	cfg := keycloakAuthConfig(t)
	s := newAPIStack(t, realVerifier(t, cfg))

	tok := devLogin(t, cfg, "player1")
	me := decodeMe(t, s.get(t, "/api/v1/me", tok.AccessToken))
	if me.User.Username != "player1" || me.User.Email == nil || *me.User.Email != "player1@vortech.test" {
		t.Fatalf("unexpected user %+v", me.User)
	}
	if !slices.Equal(me.Roles, []auth.Role{auth.RolePlayer}) || me.Player == nil {
		t.Fatalf("player1 should be a PLAYER with a profile: %+v", me)
	}
	if !slices.Contains(me.Permissions, auth.PermRangeUseOwn) || slices.Contains(me.Permissions, auth.PermPlatformAdminister) {
		t.Fatalf("unexpected permissions %v", me.Permissions)
	}

	// Logging in again maps to the same internal user.
	again := decodeMe(t, s.get(t, "/api/v1/me", devLogin(t, cfg, "player1").AccessToken))
	if again.User.ID != me.User.ID || again.Player.ID != me.Player.ID {
		t.Fatal("second login mapped to a different user")
	}
}

func TestKeycloakRoleBasedAccess(t *testing.T) {
	cfg := keycloakAuthConfig(t)
	s := newAPIStack(t, realVerifier(t, cfg))

	admin := devLogin(t, cfg, "admin1").AccessToken
	if rec := s.get(t, "/api/v1/admin/ping", admin); rec.Code != http.StatusNoContent {
		t.Fatalf("admin1 on admin endpoint: %d", rec.Code)
	}
	me := decodeMe(t, s.get(t, "/api/v1/me", admin))
	if !slices.Contains(me.Roles, auth.RoleAdmin) {
		t.Fatalf("admin1 roles %v", me.Roles)
	}

	if rec := s.get(t, "/api/v1/admin/ping", devLogin(t, cfg, "instructor1").AccessToken); rec.Code != http.StatusForbidden {
		t.Fatalf("instructor1 on admin endpoint: %d", rec.Code)
	}

	// An account with no platform roles authenticates but is not authorised.
	if rec := s.get(t, "/api/v1/me", devLogin(t, cfg, "outsider1").AccessToken); rec.Code != http.StatusForbidden {
		t.Fatalf("outsider1 on /me: %d %s", rec.Code, rec.Body.String())
	}
}

func TestKeycloakOnlyAccessTokensAccepted(t *testing.T) {
	cfg := keycloakAuthConfig(t)
	s := newAPIStack(t, realVerifier(t, cfg))
	tok := devLogin(t, cfg, "player1")

	// The ID token is signed by the same realm key but is not an access
	// token for this API (typ=ID, aud=vortech-web).
	if rec := s.get(t, "/api/v1/me", tok.IDToken); rec.Code != http.StatusUnauthorized {
		t.Fatalf("ID token accepted: %d", rec.Code)
	}
	// Refresh tokens are HMAC-signed and never valid as bearer tokens.
	if rec := s.get(t, "/api/v1/me", tok.RefreshToken); rec.Code != http.StatusUnauthorized {
		t.Fatalf("refresh token accepted: %d", rec.Code)
	}
	// A verifier expecting a different audience rejects the token.
	other := auth.NewVerifier(realKeySet(t, cfg), auth.VerifierConfig{
		Issuer: cfg.Issuer(), Audience: "some-other-api", ClientID: cfg.ClientID, Leeway: cfg.ClockSkew,
	})
	if _, err := other.Verify(context.Background(), tok.AccessToken); !errors.Is(err, auth.ErrInvalidToken) {
		t.Fatalf("token accepted by a verifier for another audience: %v", err)
	}
}

func realKeySet(t *testing.T, cfg config.AuthConfig) *auth.KeySet {
	t.Helper()
	ks := auth.NewKeySet(cfg.JWKSURL(), testLogger())
	if err := ks.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	return ks
}

func TestKeycloakClientHardening(t *testing.T) {
	cfg := keycloakAuthConfig(t)
	ctx := context.Background()
	lc := loginConfig(cfg)

	// Wrong password is refused; a correct login afterwards resets
	// Keycloak's brute-force counter for this account.
	if _, err := devauth.Login(ctx, lc, "creator1", "definitely-wrong-password"); !errors.Is(err, devauth.ErrLoginRejected) {
		t.Fatalf("wrong password: got %v", err)
	}
	devLogin(t, cfg, "creator1")

	// PKCE must use S256; the client refuses "plain".
	pw, _ := devauth.DevPassword(realmFile, "player1")
	if _, err := devauth.LoginWithChallengeMethod(ctx, lc, "player1", pw, "plain"); err == nil {
		t.Fatal("PKCE plain challenge accepted")
	}

	tokenURL := strings.TrimRight(cfg.KeycloakURL, "/") + "/realms/vortech/protocol/openid-connect/token"
	// The Resource Owner Password grant is disabled: Go and the browser never
	// handle passwords outside Keycloak's own login page.
	resp, err := http.PostForm(tokenURL, url.Values{
		"grant_type": {"password"}, "client_id": {"vortech-web"},
		"username": {"player1"}, "password": {pw},
	})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Fatal("password grant is enabled on vortech-web")
	}

	// Redirects to unregistered URIs are refused (open-redirect / code theft).
	evil := lc
	evil.RedirectURI = "https://attacker.example/callback"
	if _, err := devauth.Login(ctx, evil, "player1", pw); err == nil {
		t.Fatal("authorization to an unregistered redirect URI succeeded")
	}
}
