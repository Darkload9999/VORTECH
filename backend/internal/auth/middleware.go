package auth

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/Darkload9999/VORTECH/backend/internal/audit"
	"github.com/Darkload9999/VORTECH/backend/internal/httpx"
)

// Error codes returned by the authentication layer.
const (
	CodeInvalidToken     = "INVALID_TOKEN"
	CodeTokenExpired     = "TOKEN_EXPIRED"
	CodeAccountSuspended = "ACCOUNT_SUSPENDED"
)

// Identity is the internal record a token subject maps to.
type Identity struct {
	UserID    uuid.UUID
	PlayerID  *uuid.UUID // nil unless the user holds the PLAYER role
	Suspended bool
}

// IdentityResolver maps verified claims to internal records, creating them
// on first sight. The player module implements it; auth only consumes it.
type IdentityResolver interface {
	Resolve(ctx context.Context, c *Claims, roles RoleSet) (Identity, error)
}

// TokenVerifier validates raw bearer tokens. *Verifier implements it.
type TokenVerifier interface {
	Verify(ctx context.Context, raw string) (*Claims, error)
}

// AuditRecorder records authorization outcomes. *audit.Recorder implements it.
type AuditRecorder interface {
	Record(ctx context.Context, e audit.Entry)
}

// Principal is the authenticated caller, available to handlers via
// FromContext. It is derived only from a verified token and server-side
// records, never from client-supplied headers or body fields.
type Principal struct {
	UserID    uuid.UUID
	PlayerID  *uuid.UUID
	Subject   string
	Username  string
	Email     string
	Roles     RoleSet
	SessionID string
	ExpiresAt time.Time
}

// Can reports whether the principal holds permission p.
func (p *Principal) Can(perm Permission) bool { return p.Roles.Can(perm) }

type principalKey struct{}

// WithPrincipal returns ctx carrying p.
func WithPrincipal(ctx context.Context, p *Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// FromContext returns the authenticated principal, if any.
func FromContext(ctx context.Context) (*Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(*Principal)
	return p, ok && p != nil
}

// Authenticator authenticates bearer tokens and enforces permissions.
type Authenticator struct {
	verifier   TokenVerifier
	identities IdentityResolver
	audit      AuditRecorder
	log        *slog.Logger
	realm      string
}

// NewAuthenticator wires the authentication pipeline.
func NewAuthenticator(v TokenVerifier, ids IdentityResolver, rec AuditRecorder, log *slog.Logger) *Authenticator {
	return &Authenticator{verifier: v, identities: ids, audit: rec, log: log, realm: "vortech"}
}

// Protect authenticates the request and requires perm before calling h.
// Every protected route names the permission it needs, so there is no way
// to register an authenticated-but-unauthorised endpoint by accident.
func (a *Authenticator) Protect(perm Permission, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := a.authenticate(w, r)
		if !ok {
			return
		}
		ctx := WithPrincipal(r.Context(), p)
		if !p.Can(perm) {
			a.deny(ctx, p, perm, r, "missing_permission")
			httpx.WriteError(w, r, http.StatusForbidden, httpx.CodeForbidden,
				"You do not have permission to perform this action.")
			return
		}
		h.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (a *Authenticator) authenticate(w http.ResponseWriter, r *http.Request) (*Principal, bool) {
	ctx := r.Context()
	raw, ok := bearerToken(r)
	if !ok {
		a.challenge(w, "")
		httpx.WriteError(w, r, http.StatusUnauthorized, httpx.CodeUnauthorized, "Authentication is required.")
		return nil, false
	}

	claims, err := a.verifier.Verify(ctx, raw)
	if err != nil {
		// The reason is logged for operators; clients learn only whether to
		// refresh (expired) or re-authenticate (anything else).
		a.log.InfoContext(ctx, "access token rejected", "reason", err.Error(), "path", r.URL.Path)
		if errors.Is(err, ErrTokenExpired) {
			a.challenge(w, "The access token expired")
			httpx.WriteError(w, r, http.StatusUnauthorized, CodeTokenExpired, "The access token has expired.")
			return nil, false
		}
		a.challenge(w, "The access token is invalid")
		httpx.WriteError(w, r, http.StatusUnauthorized, CodeInvalidToken, "The access token is invalid.")
		return nil, false
	}

	roles := ParseRoles(claims.RealmAccess.Roles)
	ident, err := a.identities.Resolve(ctx, claims, roles)
	if err != nil {
		httpx.Fail(w, r, a.log, err)
		return nil, false
	}

	p := &Principal{
		UserID:    ident.UserID,
		PlayerID:  ident.PlayerID,
		Subject:   claims.Subject,
		Username:  claims.PreferredUsername,
		Email:     claims.Email,
		Roles:     roles,
		SessionID: claims.SessionID,
		ExpiresAt: claims.Expiry.Time(),
	}
	if ident.Suspended {
		a.deny(ctx, p, "", r, "account_suspended")
		httpx.WriteError(w, r, http.StatusForbidden, CodeAccountSuspended, "This account is suspended.")
		return nil, false
	}
	return p, true
}

func (a *Authenticator) challenge(w http.ResponseWriter, description string) {
	v := `Bearer realm="` + a.realm + `"`
	if description != "" {
		v += `, error="invalid_token", error_description="` + description + `"`
	}
	w.Header().Set("WWW-Authenticate", v)
}

func (a *Authenticator) deny(ctx context.Context, p *Principal, perm Permission, r *http.Request, reason string) {
	meta := map[string]any{
		"reason": reason,
		"method": r.Method,
		"path":   r.URL.Path,
	}
	if perm != "" {
		meta["permission"] = string(perm)
	}
	a.log.WarnContext(ctx, "authorization denied", "user_id", p.UserID, "reason", reason, "permission", perm, "path", r.URL.Path)
	a.audit.Record(ctx, audit.Entry{
		ActorType:    audit.ActorUser,
		ActorID:      p.UserID.String(),
		Action:       "authz.denied",
		ResourceType: "endpoint",
		ResourceID:   r.Method + " " + r.URL.Path,
		Result:       audit.ResultDenied,
		Metadata:     meta,
	})
}

// bearerToken extracts the token from a single Authorization header.
// Tokens are never accepted from query strings or cookies: URLs leak into
// logs and browser history, and cookies would expose the API to CSRF.
func bearerToken(r *http.Request) (string, bool) {
	values := r.Header.Values("Authorization")
	if len(values) != 1 {
		return "", false
	}
	scheme, token, ok := strings.Cut(values[0], " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	if token == "" || strings.ContainsAny(token, " \t") {
		return "", false
	}
	return token, true
}
