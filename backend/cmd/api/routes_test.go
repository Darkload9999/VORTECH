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

	"github.com/Darkload9999/VORTECH/backend/api"
	"github.com/Darkload9999/VORTECH/backend/internal/asset"
	"github.com/Darkload9999/VORTECH/backend/internal/audit"
	"github.com/Darkload9999/VORTECH/backend/internal/auth"
	"github.com/Darkload9999/VORTECH/backend/internal/auth/authtest"
	"github.com/Darkload9999/VORTECH/backend/internal/career"
	"github.com/Darkload9999/VORTECH/backend/internal/company"
	"github.com/Darkload9999/VORTECH/backend/internal/config"
	"github.com/Darkload9999/VORTECH/backend/internal/cyberrange"
	"github.com/Darkload9999/VORTECH/backend/internal/employee"
	"github.com/Darkload9999/VORTECH/backend/internal/health"
	"github.com/Darkload9999/VORTECH/backend/internal/interaction"
	"github.com/Darkload9999/VORTECH/backend/internal/notification"
	"github.com/Darkload9999/VORTECH/backend/internal/player"
	"github.com/Darkload9999/VORTECH/backend/internal/requestid"
	"github.com/Darkload9999/VORTECH/backend/internal/websocket"
	"github.com/Darkload9999/VORTECH/backend/internal/world"
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
		// World handlers are only reached with a valid token; route-level
		// tests never get that far, so they need no database.
		companies:     company.NewHandler(nil, nil, log),
		employees:     employee.NewHandler(nil, nil, log),
		assets:        asset.NewHandler(nil, nil, log),
		world:         world.NewHandler(nil, nil, nil, log),
		progress:      career.NewHandler(nil, nil, nil, log),
		interactions:  interaction.NewHandler(nil, nil, log),
		notifications: notification.NewHandler(nil, log),
		announcer:     world.NewAnnouncer(nil, nil, nil, log),
		gateway:       testGateway(),
		ranges:        cyberrange.NewHandler(nil, log),
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
	{http.MethodGet, "/api/v1/me/progress"},
	{http.MethodPost, "/api/v1/interactions"},
	{http.MethodPost, "/api/v1/ws/tickets"},
	{http.MethodGet, "/api/v1/notifications"},
	{http.MethodPost, "/api/v1/notifications/{id}/read"},
	{http.MethodPost, "/api/v1/notifications/read-all"},
	{http.MethodPost, "/api/v1/admin/announcements"},
	{http.MethodGet, "/api/v1/company"},
	{http.MethodGet, "/api/v1/company/departments"},
	{http.MethodGet, "/api/v1/employees"},
	{http.MethodGet, "/api/v1/employees/{id}"},
	{http.MethodGet, "/api/v1/assets"},
	{http.MethodGet, "/api/v1/assets/graph"},
	{http.MethodGet, "/api/v1/assets/{id}"},
	{http.MethodGet, "/api/v1/world"},
	{http.MethodGet, "/api/v1/world/zones/{id}"},
	{http.MethodGet, "/api/v1/world/objects/{key}"},
	{http.MethodGet, "/api/v1/range-templates"},
	{http.MethodPost, "/api/v1/ranges"},
	{http.MethodGet, "/api/v1/ranges"},
	{http.MethodGet, "/api/v1/ranges/{id}"},
	{http.MethodDelete, "/api/v1/ranges/{id}"},
}

// concrete fills path wildcards with plausible values for requests.
func concrete(path string) string {
	return strings.NewReplacer("{id}", "01a0fc08-fb28-7d11-b6a7-419822df652e", "{key}", "finance_pc_04").Replace(path)
}

func TestEveryRouteRequiresAuthentication(t *testing.T) {
	h, _ := newTestHandler(t, nil)
	for _, rt := range protectedRoutes {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(rt.method, concrete(rt.path), nil))
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
	{http.MethodGet, "/ws"}, // authenticated by a one-time ticket instead
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
		cfg:           testConfig(t),
		log:           log,
		health:        health.New(log, time.Second, 0),
		authn:         newTestAuthenticator(t, iss),
		players:       player.NewHandler(nil, log),
		companies:     company.NewHandler(nil, nil, log),
		employees:     employee.NewHandler(nil, nil, log),
		assets:        asset.NewHandler(nil, nil, log),
		world:         world.NewHandler(nil, nil, nil, log),
		progress:      career.NewHandler(nil, nil, nil, log),
		interactions:  interaction.NewHandler(nil, nil, log),
		notifications: notification.NewHandler(nil, log),
		announcer:     world.NewAnnouncer(nil, nil, nil, log),
		gateway:       testGateway(),
		ranges:        cyberrange.NewHandler(nil, log),
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

func testGateway() *websocket.Gateway {
	cfg := config.RealtimeConfig{MaxConnections: 10, MaxConnectionsPerUser: 2, MaxConnectionAge: time.Hour, PingInterval: time.Minute, TicketTTL: time.Minute}
	return websocket.NewGateway(cfg, websocket.NewTicketStore(cfg.TicketTTL), websocket.WorldAuthorizer{}, nil, slog.New(slog.DiscardHandler))
}

func TestWebSocketRequiresTicket(t *testing.T) {
	h, _ := newTestHandler(t, nil)
	for _, target := range []string{"/ws", "/ws?ticket=forged", "/ws?access_token=x"} {
		r := httptest.NewRequest(http.MethodGet, target, nil)
		r.Header.Set("Connection", "Upgrade")
		r.Header.Set("Upgrade", "websocket")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), `"code":"INVALID_TICKET"`) {
			t.Errorf("%s: got %d %s", target, rec.Code, rec.Body.String())
		}
	}
}
