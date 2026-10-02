// Package authtest provides a fake OIDC issuer for tests: an HTTP JWKS
// endpoint plus helpers that mint Keycloak-shaped access tokens.
//
// It must only be imported from tests.
package authtest

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"

	"github.com/vortech/backend/internal/auth"
)

// Defaults matching the development realm.
const (
	Audience = "vortech-api"
	ClientID = "vortech-web"
)

var (
	keysOnce sync.Once
	keyPool  []*rsa.PrivateKey
)

// Key returns a cached 2048-bit RSA test key (i in 0..2); generating keys
// per test would dominate test runtime.
func Key(t testing.TB, i int) *rsa.PrivateKey {
	t.Helper()
	keysOnce.Do(func() {
		for range 3 {
			k, err := rsa.GenerateKey(rand.Reader, 2048)
			if err != nil {
				panic(err)
			}
			keyPool = append(keyPool, k)
		}
	})
	return keyPool[i]
}

// Issuer is a fake Keycloak realm.
type Issuer struct {
	Server   *httptest.Server
	URL      string // issuer (iss) value
	Requests atomic.Int32

	mu   sync.Mutex
	jwks []jose.JSONWebKey
	key  *rsa.PrivateKey
	kid  string
}

// NewIssuer starts a JWKS server publishing one signing key.
func NewIssuer(t testing.TB) *Issuer {
	t.Helper()
	i := &Issuer{key: Key(t, 0), kid: "kid-0"}
	i.jwks = []jose.JSONWebKey{PublicJWK(i.key, i.kid)}
	i.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		i.Requests.Add(1)
		i.mu.Lock()
		set := jose.JSONWebKeySet{Keys: i.jwks}
		i.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(set)
	}))
	t.Cleanup(i.Server.Close)
	i.URL = i.Server.URL + "/realms/test"
	return i
}

// JWKSURL is the key-set endpoint.
func (i *Issuer) JWKSURL() string { return i.Server.URL + "/certs" }

// VerifierConfig returns expectations matching tokens minted by i.
func (i *Issuer) VerifierConfig() auth.VerifierConfig {
	return auth.VerifierConfig{Issuer: i.URL, Audience: Audience, ClientID: ClientID, Leeway: 30 * time.Second}
}

// PublicJWK returns the published form of key.
func PublicJWK(key *rsa.PrivateKey, kid string) jose.JSONWebKey {
	return jose.JSONWebKey{Key: &key.PublicKey, KeyID: kid, Algorithm: string(jose.RS256), Use: "sig"}
}

// SetKeys replaces the published key set.
func (i *Issuer) SetKeys(keys ...jose.JSONWebKey) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.jwks = keys
}

// UseSigningKey switches the key used by Sign (without publishing it).
func (i *Issuer) UseSigningKey(key *rsa.PrivateKey, kid string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.key, i.kid = key, kid
}

// Claims returns valid access-token claims for subject with realm roles.
func (i *Issuer) Claims(subject string, roles ...string) map[string]any {
	now := time.Now()
	realmRoles := append([]string{"offline_access", "default-roles-test"}, roles...)
	return map[string]any{
		"iss":                i.URL,
		"sub":                subject,
		"aud":                []string{Audience, "account"},
		"azp":                ClientID,
		"typ":                "Bearer",
		"exp":                now.Add(5 * time.Minute).Unix(),
		"iat":                now.Unix(),
		"jti":                "jti-" + subject,
		"sid":                "sid-" + subject,
		"preferred_username": subject,
		"email":              subject + "@vortech.test",
		"email_verified":     true,
		"name":               "Test " + subject,
		"scope":              "openid profile email",
		"realm_access":       map[string]any{"roles": realmRoles},
	}
}

// Sign signs claims with the current signing key.
func (i *Issuer) Sign(t testing.TB, claims map[string]any) string {
	t.Helper()
	i.mu.Lock()
	key, kid := i.key, i.kid
	i.mu.Unlock()
	return SignWith(t, jose.RS256, key, kid, claims)
}

// Token mints a valid access token for subject with realm roles.
func (i *Issuer) Token(t testing.TB, subject string, roles ...string) string {
	t.Helper()
	return i.Sign(t, i.Claims(subject, roles...))
}

// SignWith signs claims with an arbitrary algorithm, key and kid.
func SignWith(t testing.TB, alg jose.SignatureAlgorithm, key any, kid string, claims map[string]any) string {
	t.Helper()
	opts := (&jose.SignerOptions{}).WithType("JWT")
	if kid != "" {
		opts = opts.WithHeader(jose.HeaderKey("kid"), kid)
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: alg, Key: key}, opts)
	if err != nil {
		t.Fatalf("new signer: %v", err)
	}
	raw, err := jwt.Signed(signer).Claims(claims).Serialize()
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	return raw
}
