package auth_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vortech/backend/internal/audit"
	"github.com/vortech/backend/internal/auth"
	"github.com/vortech/backend/internal/auth/authtest"
)

type fakeResolver struct {
	mu        sync.Mutex
	suspended map[string]bool
	err       error
	calls     int
}

func (f *fakeResolver) Resolve(_ context.Context, c *auth.Claims, roles auth.RoleSet) (auth.Identity, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return auth.Identity{}, f.err
	}
	id := auth.Identity{UserID: uuid.NewSHA1(uuid.NameSpaceOID, []byte(c.Subject)), Suspended: f.suspended[c.Subject]}
	if roles.Has(auth.RolePlayer) {
		pid := uuid.NewSHA1(uuid.NameSpaceURL, []byte(c.Subject))
		id.PlayerID = &pid
	}
	return id, nil
}

type fakeAudit struct {
	mu      sync.Mutex
	entries []audit.Entry
}

func (f *fakeAudit) Record(_ context.Context, e audit.Entry) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.entries = append(f.entries, e)
}

type harness struct {
	iss      *authtest.Issuer
	resolver *fakeResolver
	audit    *fakeAudit
	seen     *auth.Principal
	handler  http.Handler
}

func newHarness(t *testing.T, perm auth.Permission) *harness {
	t.Helper()
	iss, _, v := setup(t)
	h := &harness{iss: iss, resolver: &fakeResolver{suspended: map[string]bool{}}, audit: &fakeAudit{}}
	a := auth.NewAuthenticator(v, h.resolver, h.audit, discard())
	h.handler = a.Protect(perm, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := auth.FromContext(r.Context())
		if !ok {
			t.Error("protected handler ran without a principal")
		}
		h.seen = p
		w.WriteHeader(http.StatusNoContent)
	}))
	return h
}

func (h *harness) do(t *testing.T, mutate func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/thing", nil)
	if mutate != nil {
		mutate(r)
	}
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, r)
	return rec
}

func bearer(tok string) func(*http.Request) {
	return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+tok) }
}

func errCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("not an error envelope: %s", rec.Body.String())
	}
	return env.Error.Code
}

func TestAuthenticationRequired(t *testing.T) {
	h := newHarness(t, auth.PermProfileReadOwn)
	tok := h.iss.Token(t, "user-1", "PLAYER")

	cases := map[string]func(*http.Request){
		"no header":            nil,
		"basic scheme":         func(r *http.Request) { r.Header.Set("Authorization", "Basic dXNlcjpwYXNz") },
		"empty bearer":         func(r *http.Request) { r.Header.Set("Authorization", "Bearer ") },
		"bearer without space": func(r *http.Request) { r.Header.Set("Authorization", "Bearer"+tok) },
		"token in query":       func(r *http.Request) { r.URL.RawQuery = "access_token=" + tok },
		"token in cookie":      func(r *http.Request) { r.AddCookie(&http.Cookie{Name: "access_token", Value: tok}) },
		"two auth headers": func(r *http.Request) {
			r.Header.Add("Authorization", "Bearer "+tok)
			r.Header.Add("Authorization", "Bearer "+tok)
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			rec := h.do(t, mutate)
			if rec.Code != http.StatusUnauthorized || errCode(t, rec) != "UNAUTHORIZED" {
				t.Fatalf("got %d %s", rec.Code, rec.Body.String())
			}
			if !strings.HasPrefix(rec.Header().Get("WWW-Authenticate"), `Bearer realm="vortech"`) {
				t.Fatalf("missing WWW-Authenticate challenge: %v", rec.Header())
			}
		})
	}
	if h.resolver.calls != 0 {
		t.Fatal("identity resolution must not run for unauthenticated requests")
	}
}

func TestInvalidAndExpiredTokens(t *testing.T) {
	h := newHarness(t, auth.PermProfileReadOwn)

	rec := h.do(t, bearer("eyJhbGciOiJub25lIn0.e30."))
	if rec.Code != http.StatusUnauthorized || errCode(t, rec) != auth.CodeInvalidToken {
		t.Fatalf("malformed token: %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Header().Get("WWW-Authenticate"), `error="invalid_token"`) {
		t.Fatalf("challenge missing error: %q", rec.Header().Get("WWW-Authenticate"))
	}

	c := h.iss.Claims("user-1", "PLAYER")
	c["exp"] = time.Now().Add(-time.Hour).Unix()
	rec = h.do(t, bearer(h.iss.Sign(t, c)))
	if rec.Code != http.StatusUnauthorized || errCode(t, rec) != auth.CodeTokenExpired {
		t.Fatalf("expired token: %d %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "exp") && strings.Contains(rec.Body.String(), "go-jose") {
		t.Fatalf("verification internals leaked: %s", rec.Body.String())
	}
}

func TestPermissionGranted(t *testing.T) {
	h := newHarness(t, auth.PermRangeUseOwn)
	rec := h.do(t, bearer(h.iss.Token(t, "user-1", "PLAYER")))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("got %d %s", rec.Code, rec.Body.String())
	}
	if h.seen == nil || h.seen.Subject != "user-1" || h.seen.PlayerID == nil || !h.seen.Roles.Has(auth.RolePlayer) {
		t.Fatalf("unexpected principal %+v", h.seen)
	}
	if len(h.audit.entries) != 0 {
		t.Fatalf("granted request must not be audited as denied: %+v", h.audit.entries)
	}
}

func TestPermissionDeniedIsAudited(t *testing.T) {
	h := newHarness(t, auth.PermPlatformAdminister)
	rec := h.do(t, bearer(h.iss.Token(t, "user-1", "PLAYER")))
	if rec.Code != http.StatusForbidden || errCode(t, rec) != "FORBIDDEN" {
		t.Fatalf("got %d %s", rec.Code, rec.Body.String())
	}
	if len(h.audit.entries) != 1 {
		t.Fatalf("expected one audit entry, got %d", len(h.audit.entries))
	}
	e := h.audit.entries[0]
	if e.Action != "authz.denied" || e.Result != audit.ResultDenied || e.ActorType != audit.ActorUser ||
		e.Metadata["permission"] != string(auth.PermPlatformAdminister) || e.ResourceID != "GET /api/v1/thing" {
		t.Fatalf("unexpected audit entry %+v", e)
	}
}

func TestRoleEscalationRejected(t *testing.T) {
	h := newHarness(t, auth.PermPlatformAdminister)

	cases := map[string]func(*http.Request){
		// Client-supplied hints are ignored: roles come only from the signed token.
		"role header": func(r *http.Request) {
			bearer(h.iss.Token(t, "user-1", "PLAYER"))(r)
			r.Header.Set("X-Roles", "ADMIN")
			r.Header.Set("X-User-Role", "ADMIN")
		},
		// Realm role names are case-sensitive.
		"lowercase admin": bearer(h.iss.Token(t, "user-1", "admin")),
		// A client role named ADMIN on some client is not the realm role.
		"client role": func(r *http.Request) {
			c := h.iss.Claims("user-1", "PLAYER")
			c["resource_access"] = map[string]any{"vortech-web": map[string]any{"roles": []string{"ADMIN"}}}
			bearer(h.iss.Sign(t, c))(r)
		},
		// A top-level roles claim is not where Keycloak puts realm roles.
		"top-level roles claim": func(r *http.Request) {
			c := h.iss.Claims("user-1", "PLAYER")
			c["roles"] = []string{"ADMIN"}
			bearer(h.iss.Sign(t, c))(r)
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			rec := h.do(t, mutate)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("escalation attempt got %d %s", rec.Code, rec.Body.String())
			}
		})
	}

	// Sanity check: a genuine ADMIN token passes.
	if rec := h.do(t, bearer(h.iss.Token(t, "admin-1", "ADMIN"))); rec.Code != http.StatusNoContent {
		t.Fatalf("admin token got %d", rec.Code)
	}
}

func TestSuspendedAccountRejected(t *testing.T) {
	h := newHarness(t, auth.PermProfileReadOwn)
	h.resolver.suspended["user-1"] = true
	rec := h.do(t, bearer(h.iss.Token(t, "user-1", "PLAYER")))
	if rec.Code != http.StatusForbidden || errCode(t, rec) != auth.CodeAccountSuspended {
		t.Fatalf("got %d %s", rec.Code, rec.Body.String())
	}
	if len(h.audit.entries) != 1 || h.audit.entries[0].Metadata["reason"] != "account_suspended" {
		t.Fatalf("suspension denial not audited: %+v", h.audit.entries)
	}
}

func TestIdentityResolverFailureIsOpaque500(t *testing.T) {
	h := newHarness(t, auth.PermProfileReadOwn)
	h.resolver.err = errors.New("pgx: connection refused to 10.0.0.5")
	rec := h.do(t, bearer(h.iss.Token(t, "user-1", "PLAYER")))
	if rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), "10.0.0.5") {
		t.Fatalf("got %d %s", rec.Code, rec.Body.String())
	}
}

func TestSchemeIsCaseInsensitive(t *testing.T) {
	h := newHarness(t, auth.PermProfileReadOwn)
	tok := h.iss.Token(t, "user-1", "PLAYER")
	rec := h.do(t, func(r *http.Request) { r.Header.Set("Authorization", "bearer "+tok) })
	if rec.Code != http.StatusNoContent {
		t.Fatalf("got %d", rec.Code)
	}
}
