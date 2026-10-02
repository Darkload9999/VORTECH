package auth_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"

	"github.com/Darkload9999/VORTECH/backend/internal/auth"
	"github.com/Darkload9999/VORTECH/backend/internal/auth/authtest"
)

func discard() *slog.Logger { return slog.New(slog.DiscardHandler) }

// setup returns an issuer and a verifier with keys already loaded.
func setup(t *testing.T) (*authtest.Issuer, *auth.KeySet, *auth.Verifier) {
	t.Helper()
	iss := authtest.NewIssuer(t)
	ks := auth.NewKeySet(iss.JWKSURL(), discard())
	if err := ks.Refresh(context.Background()); err != nil {
		t.Fatalf("initial jwks load: %v", err)
	}
	return iss, ks, auth.NewVerifier(ks, iss.VerifierConfig())
}

func TestVerifyValidToken(t *testing.T) {
	iss, _, v := setup(t)
	c, err := v.Verify(context.Background(), iss.Token(t, "user-1", "PLAYER"))
	if err != nil {
		t.Fatalf("valid token rejected: %v", err)
	}
	if c.Subject != "user-1" || c.PreferredUsername != "user-1" || c.AuthorizedParty != authtest.ClientID {
		t.Fatalf("unexpected claims %+v", c)
	}
	if !auth.ParseRoles(c.RealmAccess.Roles).Has(auth.RolePlayer) {
		t.Fatalf("roles not parsed: %v", c.RealmAccess.Roles)
	}
}

func TestVerifyRejectsBadClaims(t *testing.T) {
	iss, _, v := setup(t)
	now := time.Now()

	tests := []struct {
		name    string
		mutate  func(map[string]any)
		wantErr error
	}{
		{"expired", func(c map[string]any) { c["exp"] = now.Add(-time.Minute).Unix() }, auth.ErrTokenExpired},
		{"expired beyond leeway only", func(c map[string]any) { c["exp"] = now.Add(-31 * time.Second).Unix() }, auth.ErrTokenExpired},
		{"missing exp", func(c map[string]any) { delete(c, "exp") }, auth.ErrInvalidToken},
		{"not yet valid", func(c map[string]any) { c["nbf"] = now.Add(5 * time.Minute).Unix() }, auth.ErrInvalidToken},
		{"issued in the future", func(c map[string]any) { c["iat"] = now.Add(5 * time.Minute).Unix() }, auth.ErrInvalidToken},
		{"wrong issuer", func(c map[string]any) { c["iss"] = "https://evil.example/realms/test" }, auth.ErrInvalidToken},
		{"wrong audience", func(c map[string]any) { c["aud"] = []string{"account"} }, auth.ErrInvalidToken},
		{"missing audience", func(c map[string]any) { delete(c, "aud") }, auth.ErrInvalidToken},
		{"issued to another client", func(c map[string]any) { c["azp"] = "some-other-client" }, auth.ErrInvalidToken},
		{"id token", func(c map[string]any) { c["typ"] = "ID" }, auth.ErrInvalidToken},
		{"refresh token", func(c map[string]any) { c["typ"] = "Refresh" }, auth.ErrInvalidToken},
		{"missing typ", func(c map[string]any) { delete(c, "typ") }, auth.ErrInvalidToken},
		{"missing sub", func(c map[string]any) { delete(c, "sub") }, auth.ErrInvalidToken},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := iss.Claims("user-1", "PLAYER")
			tt.mutate(c)
			_, err := v.Verify(context.Background(), iss.Sign(t, c))
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("got %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestVerifyAcceptsWithinClockSkew(t *testing.T) {
	iss, _, v := setup(t)
	c := iss.Claims("user-1")
	c["exp"] = time.Now().Add(-10 * time.Second).Unix()
	if _, err := v.Verify(context.Background(), iss.Sign(t, c)); err != nil {
		t.Fatalf("token within 30s leeway rejected: %v", err)
	}
}

func b64(v any) string {
	b, _ := json.Marshal(v)
	return base64.RawURLEncoding.EncodeToString(b)
}

func TestVerifyRejectsMalformedAndForgedTokens(t *testing.T) {
	iss, _, v := setup(t)
	valid := iss.Token(t, "user-1", "PLAYER")
	parts := strings.Split(valid, ".")

	pubDER, err := x509.MarshalPKIXPublicKey(&authtest.Key(t, 0).PublicKey)
	if err != nil {
		t.Fatal(err)
	}

	cases := map[string]string{
		"empty":              "",
		"garbage":            "not-a-jwt",
		"two segments":       "a.b",
		"invalid base64":     "!!!.@@@.###",
		"oversized":          strings.Repeat("a", 9000),
		"truncated sig":      parts[0] + "." + parts[1] + "." + parts[2][:10],
		"missing sig":        parts[0] + "." + parts[1] + ".",
		"json serialization": `{"payload":"` + parts[1] + `","protected":"` + parts[0] + `","signature":"` + parts[2] + `"}`,
		// alg=none: the classic signature-stripping attack.
		"alg none": b64(map[string]string{"alg": "none", "kid": "kid-0"}) + "." + parts[1] + ".",
		// HS256 keyed with the RSA public key: the algorithm-confusion attack.
		"hs256 with public key": authtest.SignWith(t, jose.HS256, pubDER, "kid-0", iss.Claims("user-1", "ADMIN")),
		// Payload swapped to escalate privileges, original signature kept.
		"tampered payload": parts[0] + "." + b64(iss.Claims("user-1", "ADMIN")) + "." + parts[2],
		// Valid structure signed by a key that is not published.
		"unknown key, same kid": authtest.SignWith(t, jose.RS256, authtest.Key(t, 1), "kid-0", iss.Claims("user-1", "ADMIN")),
		"no kid":                authtest.SignWith(t, jose.RS256, authtest.Key(t, 0), "", iss.Claims("user-1")),
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := v.Verify(context.Background(), raw)
			if !errors.Is(err, auth.ErrInvalidToken) {
				t.Fatalf("got %v, want ErrInvalidToken", err)
			}
		})
	}
}

func TestVerifyRejectsAlgorithmMismatchWithKey(t *testing.T) {
	iss, _, v := setup(t)
	// The published key declares RS256; a PS256 signature by the same key
	// must still be refused.
	raw := authtest.SignWith(t, jose.PS256, authtest.Key(t, 0), "kid-0", iss.Claims("user-1"))
	if _, err := v.Verify(context.Background(), raw); !errors.Is(err, auth.ErrInvalidToken) {
		t.Fatalf("got %v, want ErrInvalidToken", err)
	}
}

func TestKeyRotationRefreshesOnUnknownKid(t *testing.T) {
	iss, ks, v := setup(t)
	ks.SetMinRefresh(0)
	before := iss.Requests.Load()

	// Keycloak publishes a new key and starts signing with it.
	newKey := authtest.Key(t, 1)
	iss.SetKeys(authtest.PublicJWK(authtest.Key(t, 0), "kid-0"), authtest.PublicJWK(newKey, "kid-1"))
	iss.UseSigningKey(newKey, "kid-1")

	if _, err := v.Verify(context.Background(), iss.Token(t, "user-1")); err != nil {
		t.Fatalf("token signed with rotated key rejected: %v", err)
	}
	if got := iss.Requests.Load() - before; got != 1 {
		t.Fatalf("expected exactly one refresh, got %d", got)
	}
}

func TestUnknownKidRefreshIsRateLimited(t *testing.T) {
	iss, _, v := setup(t) // default 10s minimum interval
	before := iss.Requests.Load()
	for i := range 20 {
		raw := authtest.SignWith(t, jose.RS256, authtest.Key(t, 1), "random-kid-"+string(rune('a'+i)), iss.Claims("user-1"))
		if _, err := v.Verify(context.Background(), raw); !errors.Is(err, auth.ErrInvalidToken) {
			t.Fatalf("got %v", err)
		}
	}
	// setup's initial Refresh counts as the last attempt, so no further
	// fetches are allowed within the interval.
	if got := iss.Requests.Load() - before; got != 0 {
		t.Fatalf("unknown kids triggered %d JWKS fetches; refreshes must be rate limited", got)
	}
}

func TestKeySetFiltersUnusableKeys(t *testing.T) {
	weak, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	ec, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	strong := authtest.Key(t, 0)

	keys := []jose.JSONWebKey{
		{Key: &weak.PublicKey, KeyID: "weak", Algorithm: "RS256", Use: "sig"},
		{Key: &strong.PublicKey, KeyID: "enc", Algorithm: "RSA-OAEP", Use: "enc"},
		{Key: &strong.PublicKey, KeyID: "", Algorithm: "RS256", Use: "sig"},
		{Key: strong, KeyID: "private", Algorithm: "RS256", Use: "sig"},
		{Key: &ec.PublicKey, KeyID: "ec", Algorithm: "ES256", Use: "sig"},
		{Key: &strong.PublicKey, KeyID: "good", Algorithm: "RS256", Use: "sig"},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: keys})
	}))
	defer srv.Close()

	ks := auth.NewKeySet(srv.URL, discard())
	if err := ks.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	ks.SetMinRefresh(time.Hour)
	for _, kid := range []string{"weak", "enc", "private"} {
		if _, err := ks.Key(context.Background(), kid); !errors.Is(err, auth.ErrUnknownKey) {
			t.Errorf("key %q should have been filtered out, got err=%v", kid, err)
		}
	}
	for _, kid := range []string{"good", "ec"} {
		if _, err := ks.Key(context.Background(), kid); err != nil {
			t.Errorf("key %q should be usable: %v", kid, err)
		}
	}
}

func TestKeySetFailuresKeepExistingKeys(t *testing.T) {
	status := http.StatusOK
	strong := authtest.Key(t, 0)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{authtest.PublicJWK(strong, "k")}})
	}))
	defer srv.Close()

	ks := auth.NewKeySet(srv.URL, discard())
	if err := ks.Ready(context.Background()); err == nil {
		t.Fatal("empty key set must not be ready")
	}
	if err := ks.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := ks.Ready(context.Background()); err != nil {
		t.Fatalf("loaded key set should be ready: %v", err)
	}

	status = http.StatusServiceUnavailable
	if err := ks.Refresh(context.Background()); err == nil {
		t.Fatal("expected refresh error on 503")
	}
	if _, err := ks.Key(context.Background(), "k"); err != nil {
		t.Fatalf("failed refresh discarded existing keys: %v", err)
	}
}

func TestKeySetRejectsEmptyOrHugeResponses(t *testing.T) {
	body := `{"keys":[]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	ks := auth.NewKeySet(srv.URL, discard())

	if err := ks.Refresh(context.Background()); err == nil || !strings.Contains(err.Error(), "no usable signing keys") {
		t.Fatalf("empty key set: %v", err)
	}
	body = `{"keys":[` + strings.Repeat(" ", 2<<20) + `]}`
	if err := ks.Refresh(context.Background()); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("oversized response: %v", err)
	}
}

func TestKeySetRunLoadsAndStops(t *testing.T) {
	iss := authtest.NewIssuer(t)
	ks := auth.NewKeySet(iss.JWKSURL(), discard())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { ks.Run(ctx, time.Hour); close(done) }()

	deadline := time.Now().Add(2 * time.Second)
	for ks.Ready(ctx) != nil {
		if time.Now().After(deadline) {
			t.Fatal("Run did not load keys")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop on context cancellation")
	}
}
