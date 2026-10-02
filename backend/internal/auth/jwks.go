package auth

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	jose "github.com/go-jose/go-jose/v4"
)

const (
	// minRSABits rejects weak RSA verification keys.
	minRSABits = 2048
	// maxJWKSBytes bounds the JWKS response read from Keycloak.
	maxJWKSBytes = 1 << 20
)

// ErrUnknownKey is returned when no signing key matches a token's kid.
var ErrUnknownKey = errors.New("unknown signing key")

// KeySet caches the realm's public signing keys.
//
// Keys are refreshed periodically by Run and on demand when a token
// references an unknown kid (Keycloak key rotation). On-demand refreshes are
// rate limited so tokens with random kids cannot be used to hammer Keycloak.
// A failed refresh never discards previously loaded keys.
type KeySet struct {
	url        string
	client     *http.Client
	log        *slog.Logger
	minRefresh time.Duration
	now        func() time.Time

	mu          sync.RWMutex
	keys        map[string]jose.JSONWebKey
	lastSuccess time.Time

	refreshMu   sync.Mutex // serialises fetches
	lastAttempt time.Time  // guarded by refreshMu
}

// NewKeySet returns an empty key set that loads keys from jwksURL.
func NewKeySet(jwksURL string, log *slog.Logger) *KeySet {
	return &KeySet{
		url:        jwksURL,
		client:     &http.Client{Timeout: 5 * time.Second},
		log:        log,
		minRefresh: 10 * time.Second,
		now:        time.Now,
		keys:       map[string]jose.JSONWebKey{},
	}
}

// Key returns the verification key for kid, refreshing once (subject to rate
// limiting) if the kid is not cached.
func (s *KeySet) Key(ctx context.Context, kid string) (jose.JSONWebKey, error) {
	if k, ok := s.lookup(kid); ok {
		return k, nil
	}
	if err := s.refresh(ctx, false); err != nil && !errors.Is(err, errRateLimited) {
		s.log.WarnContext(ctx, "jwks refresh for unknown kid failed", "error", err)
	}
	if k, ok := s.lookup(kid); ok {
		return k, nil
	}
	return jose.JSONWebKey{}, ErrUnknownKey
}

func (s *KeySet) lookup(kid string) (jose.JSONWebKey, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	k, ok := s.keys[kid]
	return k, ok
}

// Refresh fetches the key set now, ignoring the on-demand rate limit.
func (s *KeySet) Refresh(ctx context.Context) error {
	return s.refresh(ctx, true)
}

var errRateLimited = errors.New("jwks refresh rate limited")

func (s *KeySet) refresh(ctx context.Context, force bool) error {
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()

	if !force && s.now().Sub(s.lastAttempt) < s.minRefresh {
		return errRateLimited
	}
	s.lastAttempt = s.now()

	keys, err := s.fetch(ctx)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.keys = keys
	s.lastSuccess = s.now()
	s.mu.Unlock()
	s.log.Debug("jwks refreshed", "keys", len(keys))
	return nil
}

func (s *KeySet) fetch(ctx context.Context) (map[string]jose.JSONWebKey, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.url, nil)
	if err != nil {
		return nil, fmt.Errorf("build jwks request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch jwks: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch jwks: unexpected status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxJWKSBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read jwks: %w", err)
	}
	if len(body) > maxJWKSBytes {
		return nil, errors.New("jwks response too large")
	}

	// Decode keys individually: one unsupported key (e.g. an RSA-OAEP
	// encryption key) must not prevent loading the signing keys.
	var raw struct {
		Keys []json.RawMessage `json:"keys"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("decode jwks: %w", err)
	}
	keys := make(map[string]jose.JSONWebKey, len(raw.Keys))
	for _, r := range raw.Keys {
		var k jose.JSONWebKey
		if err := k.UnmarshalJSON(r); err != nil {
			continue
		}
		if usableSigningKey(k) {
			keys[k.KeyID] = k
		}
	}
	if len(keys) == 0 {
		return nil, errors.New("jwks contains no usable signing keys")
	}
	return keys, nil
}

// usableSigningKey accepts only public signature keys of strong types whose
// declared algorithm (if any) is one we verify.
func usableSigningKey(k jose.JSONWebKey) bool {
	if k.KeyID == "" || !k.IsPublic() || (k.Use != "" && k.Use != "sig") {
		return false
	}
	if k.Algorithm != "" && !isAllowedAlg(jose.SignatureAlgorithm(k.Algorithm)) {
		return false
	}
	switch pub := k.Key.(type) {
	case *rsa.PublicKey:
		return pub.N.BitLen() >= minRSABits
	case *ecdsa.PublicKey, ed25519.PublicKey:
		return true
	default:
		return false
	}
}

// Ready reports whether at least one signing key has been loaded.
func (s *KeySet) Ready(context.Context) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(s.keys) == 0 {
		return errors.New("no signing keys loaded from Keycloak yet")
	}
	return nil
}

// Run loads keys immediately and then refreshes them every interval until
// ctx is cancelled. Failures are retried with backoff and never discard
// keys that are already loaded.
func (s *KeySet) Run(ctx context.Context, interval time.Duration) {
	backoff := time.Second
	for {
		wait := interval
		if err := s.Refresh(ctx); err != nil {
			if ctx.Err() != nil {
				return
			}
			s.log.Warn("jwks refresh failed", "url", s.url, "error", err, "retry_in", backoff)
			wait = backoff
			backoff = min(backoff*2, time.Minute)
		} else {
			backoff = time.Second
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}
