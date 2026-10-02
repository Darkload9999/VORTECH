package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vortech/backend/api"
	"github.com/vortech/backend/internal/audit"
	"github.com/vortech/backend/internal/auth"
	"github.com/vortech/backend/internal/auth/authtest"
	"github.com/vortech/backend/internal/config"
	"github.com/vortech/backend/internal/health"
	"github.com/vortech/backend/internal/player"
	"github.com/vortech/backend/internal/requestid"
)

type stubResolver struct{}

func (stubResolver) Resolve(_ context.Context, c *auth.Claims, _ auth.RoleSet) (auth.Identity, error) {
	return auth.Identity{UserID: uuid.NewSHA1(uuid.NameSpaceOID, []byte(c.Subject))}, nil
}

type stubAudit struct{}

func (stubAudit) Record(context.Context, audit.Entry) {}

// newTestAuthenticator returns an authenticator trusting iss.
func newTestAuthenticator(t *testing.T, iss *authtest.Issuer) *auth.Authenticator {
	t.Helper()
	log := slog.New(slog.DiscardHandler)
	ks := auth.NewKeySet(iss.JWKSURL(), log)
	if err := ks.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	return auth.NewAuthenticator(auth.NewVerifier(ks, iss.VerifierConfig()), stubResolver{}, stubAudit{}, log)
}

func testConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := config.Load(func(k string) (string, bool) {
		if k == "DATABASE_URL" {
			return "postgres://u:p@localhost:5432/db", true
		}
		return "", false
	})
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func newTestHandler(t *testing.T, dbErr error) (http.Handler, *health.Service) {
	t.Helper()
	log := slog.New(slog.DiscardHandler)
	hs := health.New(log, time.Second, 0, health.Check{
		Name: "postgres",
		Fn:   func(context.Context) error { return dbErr },
	})
	h := newHandler(deps{
		cfg:     testConfig(t),
		log:     log,
		health:  hs,
		authn:   newTestAuthenticator(t, authtest.NewIssuer(t)),
		players: player.NewHandler(nil, log),
	})
	return h, hs
}

func TestHealthEndpoint(t *testing.T) {
	h, _ := newTestHandler(t, errors.New("db down"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/health", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("liveness must succeed even when the database is down, got %d", rec.Code)
	}
	if strings.TrimSpace(rec.Body.String()) != `{"data":{"status":"ok"}}` {
		t.Fatalf("body = %s", rec.Body.String())
	}
	if !requestid.Valid(rec.Header().Get(requestid.Header)) {
		t.Fatal("response missing X-Request-ID")
	}
}

func TestReadyEndpoint(t *testing.T) {
	t.Run("ready", func(t *testing.T) {
		h, _ := newTestHandler(t, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/ready", nil))
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"postgres":"up"`) {
			t.Fatalf("got %d %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("dependency down", func(t *testing.T) {
		h, _ := newTestHandler(t, errors.New("db down"))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/ready", nil))
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("got %d", rec.Code)
		}
		var env struct {
			Error struct {
				Code      string `json:"code"`
				RequestID string `json:"request_id"`
			} `json:"error"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
			t.Fatal(err)
		}
		if env.Error.Code != health.CodeNotReady || env.Error.RequestID != rec.Header().Get(requestid.Header) {
			t.Fatalf("unexpected error body %s", rec.Body.String())
		}
	})
	t.Run("draining", func(t *testing.T) {
		h, hs := newTestHandler(t, nil)
		hs.SetDraining()
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/ready", nil))
		if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), health.CodeDraining) {
			t.Fatalf("got %d %s", rec.Code, rec.Body.String())
		}
	})
}

func TestUnknownRoutesUseErrorFormat(t *testing.T) {
	h, _ := newTestHandler(t, nil)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/does-not-exist", nil))
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), `"code":"NOT_FOUND"`) {
		t.Fatalf("got %d %s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/health", nil))
	if rec.Code != http.StatusMethodNotAllowed || !strings.Contains(rec.Body.String(), `"code":"METHOD_NOT_ALLOWED"`) {
		t.Fatalf("got %d %s", rec.Code, rec.Body.String())
	}
}

// protectedRoutes lists every non-probe route; each new route must be added
// so the test below proves it is not reachable anonymously.
var protectedRoutes = []struct{ method, path string }{
	{http.MethodGet, "/api/v1/me"},
}

func TestEveryRouteRequiresAuthentication(t *testing.T) {
	h, _ := newTestHandler(t, nil)
	for _, rt := range protectedRoutes {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(rt.method, rt.path, nil))
		if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), `"code":"UNAUTHORIZED"`) {
			t.Errorf("%s %s without token: got %d %s", rt.method, rt.path, rec.Code, rec.Body.String())
		}
	}
}

// publicRoutes are deliberately reachable without a token.
var publicRoutes = []struct{ method, path string }{
	{http.MethodGet, "/api/v1/health"},
	{http.MethodGet, "/api/v1/ready"},
	{http.MethodGet, "/api/v1/openapi.json"},
}

func TestOpenAPIDocumentsEveryRoute(t *testing.T) {
	var doc struct {
		OpenAPI string                               `json:"openapi"`
		Paths   map[string]map[string]map[string]any `json:"paths"`
	}
	if err := json.Unmarshal(api.OpenAPI, &doc); err != nil {
		t.Fatalf("openapi.json is not valid JSON: %v", err)
	}
	if doc.OpenAPI != "3.1.0" {
		t.Fatalf("openapi version = %q", doc.OpenAPI)
	}
	all := append(append([]struct{ method, path string }{}, publicRoutes...), protectedRoutes...)
	for _, rt := range all {
		if _, ok := doc.Paths[rt.path][strings.ToLower(rt.method)]; !ok {
			t.Errorf("%s %s is not documented in api/openapi.json", rt.method, rt.path)
		}
	}
	for path, ops := range doc.Paths {
		for method, op := range ops {
			sec, hasSec := op["security"].([]any)
			public := hasSec && len(sec) == 0
			isPublic := false
			for _, rt := range publicRoutes {
				if rt.path == path && strings.ToLower(rt.method) == method {
					isPublic = true
				}
			}
			if public != isPublic {
				t.Errorf("%s %s: OpenAPI security does not match route protection", method, path)
			}
		}
	}
}

func TestOpenAPIServed(t *testing.T) {
	h, _ := newTestHandler(t, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/openapi.json", nil))
	if rec.Code != http.StatusOK || !json.Valid(rec.Body.Bytes()) {
		t.Fatalf("got %d", rec.Code)
	}
}

func TestMeRequiresAPlatformRole(t *testing.T) {
	iss := authtest.NewIssuer(t)
	log := slog.New(slog.DiscardHandler)
	h := newHandler(deps{
		cfg:     testConfig(t),
		log:     log,
		health:  health.New(log, time.Second, 0),
		authn:   newTestAuthenticator(t, iss),
		players: player.NewHandler(nil, log),
	})

	// A Keycloak account holding none of the platform roles is
	// authenticated but not authorised.
	r := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	r.Header.Set("Authorization", "Bearer "+iss.Token(t, "no-roles"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("got %d %s", rec.Code, rec.Body.String())
	}
}
