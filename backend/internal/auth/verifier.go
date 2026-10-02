package auth

import (
	"context"
	"errors"
	"fmt"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// maxTokenBytes bounds the work done on attacker-supplied input. Keycloak
// access tokens are typically 1–2 KiB.
const maxTokenBytes = 8 << 10

// allowedAlgs lists accepted signature algorithms. Symmetric (HS*) and
// "none" are deliberately absent: accepting them would allow forging tokens
// with the public key or without any key.
var allowedAlgs = []jose.SignatureAlgorithm{
	jose.RS256, jose.RS384, jose.RS512,
	jose.PS256, jose.PS384, jose.PS512,
	jose.ES256, jose.ES384, jose.ES512,
	jose.EdDSA,
}

func isAllowedAlg(a jose.SignatureAlgorithm) bool {
	for _, x := range allowedAlgs {
		if a == x {
			return true
		}
	}
	return false
}

// Token validation errors. ErrTokenExpired is distinguished so clients know
// to refresh; every other failure is ErrInvalidToken.
var (
	ErrInvalidToken = errors.New("invalid token")
	ErrTokenExpired = errors.New("token expired")
)

// Claims are the access-token claims the platform relies on.
type Claims struct {
	jwt.Claims

	// Keycloak sets typ=Bearer on access tokens and ID/Refresh on others.
	Type              string `json:"typ"`
	AuthorizedParty   string `json:"azp"`
	SessionID         string `json:"sid"`
	PreferredUsername string `json:"preferred_username"`
	Email             string `json:"email"`
	EmailVerified     bool   `json:"email_verified"`
	Name              string `json:"name"`
	Scope             string `json:"scope"`
	ACR               string `json:"acr"`
	RealmAccess       struct {
		Roles []string `json:"roles"`
	} `json:"realm_access"`
}

// keySource is satisfied by *KeySet; tests may substitute it.
type keySource interface {
	Key(ctx context.Context, kid string) (jose.JSONWebKey, error)
}

// Verifier validates Keycloak access tokens.
type Verifier struct {
	keys     keySource
	issuer   string
	audience string
	clientID string
	leeway   time.Duration
	now      func() time.Time
}

// VerifierConfig holds the expectations a token must meet.
type VerifierConfig struct {
	Issuer   string
	Audience string
	ClientID string
	Leeway   time.Duration
}

// NewVerifier returns a Verifier that resolves signing keys from keys.
func NewVerifier(keys keySource, cfg VerifierConfig) *Verifier {
	return &Verifier{
		keys:     keys,
		issuer:   cfg.Issuer,
		audience: cfg.Audience,
		clientID: cfg.ClientID,
		leeway:   cfg.Leeway,
		now:      time.Now,
	}
}

// Verify checks the token's signature and claims and returns its claims.
// Returned errors wrap ErrInvalidToken or ErrTokenExpired; the wrapped
// detail is for logs only.
func (v *Verifier) Verify(ctx context.Context, raw string) (*Claims, error) {
	if raw == "" || len(raw) > maxTokenBytes {
		return nil, fmt.Errorf("%w: empty or oversized", ErrInvalidToken)
	}

	// Compact serialization only, restricted to asymmetric algorithms.
	tok, err := jwt.ParseSigned(raw, allowedAlgs)
	if err != nil {
		return nil, fmt.Errorf("%w: parse: %v", ErrInvalidToken, err)
	}
	if len(tok.Headers) != 1 {
		return nil, fmt.Errorf("%w: expected exactly one signature", ErrInvalidToken)
	}
	hdr := tok.Headers[0]
	if hdr.KeyID == "" {
		return nil, fmt.Errorf("%w: missing kid", ErrInvalidToken)
	}
	if len(hdr.ExtraHeaders) > 0 {
		if _, ok := hdr.ExtraHeaders[jose.HeaderKey("crit")]; ok {
			return nil, fmt.Errorf("%w: unsupported critical header", ErrInvalidToken)
		}
	}

	key, err := v.keys.Key(ctx, hdr.KeyID)
	if err != nil {
		return nil, fmt.Errorf("%w: kid %q: %v", ErrInvalidToken, hdr.KeyID, err)
	}
	if key.Algorithm != "" && key.Algorithm != hdr.Algorithm {
		return nil, fmt.Errorf("%w: algorithm %s does not match key algorithm %s", ErrInvalidToken, hdr.Algorithm, key.Algorithm)
	}

	var c Claims
	if err := tok.Claims(key.Key, &c); err != nil {
		return nil, fmt.Errorf("%w: signature: %v", ErrInvalidToken, err)
	}

	// go-jose treats missing exp as "never expires"; we do not.
	if c.Expiry == nil {
		return nil, fmt.Errorf("%w: missing exp", ErrInvalidToken)
	}
	err = c.ValidateWithLeeway(jwt.Expected{
		Issuer:      v.issuer,
		AnyAudience: jwt.Audience{v.audience},
		Time:        v.now(),
	}, v.leeway)
	switch {
	case errors.Is(err, jwt.ErrExpired):
		return nil, fmt.Errorf("%w: exp %s", ErrTokenExpired, c.Expiry.Time().UTC().Format(time.RFC3339))
	case err != nil:
		return nil, fmt.Errorf("%w: %v", ErrInvalidToken, err)
	}

	if c.Subject == "" {
		return nil, fmt.Errorf("%w: missing sub", ErrInvalidToken)
	}
	// Reject ID and refresh tokens presented as access tokens.
	if c.Type != "Bearer" {
		return nil, fmt.Errorf("%w: token type %q is not an access token", ErrInvalidToken, c.Type)
	}
	if c.AuthorizedParty != v.clientID {
		return nil, fmt.Errorf("%w: issued to client %q", ErrInvalidToken, c.AuthorizedParty)
	}
	return &c, nil
}
