package main

import (
	"log/slog"
	"net/http"

	"github.com/Darkload9999/VORTECH/backend/api"
	"github.com/Darkload9999/VORTECH/backend/internal/asset"
	"github.com/Darkload9999/VORTECH/backend/internal/auth"
	"github.com/Darkload9999/VORTECH/backend/internal/career"
	"github.com/Darkload9999/VORTECH/backend/internal/company"
	"github.com/Darkload9999/VORTECH/backend/internal/config"
	"github.com/Darkload9999/VORTECH/backend/internal/employee"
	"github.com/Darkload9999/VORTECH/backend/internal/health"
	"github.com/Darkload9999/VORTECH/backend/internal/httpx"
	"github.com/Darkload9999/VORTECH/backend/internal/interaction"
	"github.com/Darkload9999/VORTECH/backend/internal/player"
	"github.com/Darkload9999/VORTECH/backend/internal/world"
)

const (
	pathHealth = "/api/v1/health"
	pathReady  = "/api/v1/ready"
)

// deps are the constructed modules the HTTP layer routes to.
type deps struct {
	cfg          *config.Config
	log          *slog.Logger
	health       *health.Service
	authn        *auth.Authenticator
	players      *player.Handler
	companies    *company.Handler
	employees    *employee.Handler
	assets       *asset.Handler
	world        *world.Handler
	progress     *career.Handler
	interactions *interaction.Handler
}

// newHandler is the composition root for HTTP routes. Every route except the
// probes and the API description goes through authn.Protect with an
// explicit permission; routes_test.go enforces this and the OpenAPI sync.
func newHandler(d deps) http.Handler {
	r := httpx.NewRouter()

	r.HandleFunc("GET "+pathHealth, d.health.Live)
	r.HandleFunc("GET "+pathReady, d.health.Ready)
	r.HandleFunc("GET /api/v1/openapi.json", serveOpenAPI)

	protect := func(perm auth.Permission, h http.HandlerFunc) http.Handler { return d.authn.Protect(perm, h) }

	r.Handle("GET /api/v1/me", protect(auth.PermProfileReadOwn, d.players.Me))
	r.Handle("GET /api/v1/me/progress", protect(auth.PermScenarioPlay, d.progress.Me))
	r.Handle("POST /api/v1/interactions", protect(auth.PermScenarioPlay, d.interactions.Create))

	r.Handle("GET /api/v1/company", protect(auth.PermWorldRead, d.companies.Get))
	r.Handle("GET /api/v1/company/departments", protect(auth.PermWorldRead, d.companies.Departments))
	r.Handle("GET /api/v1/employees", protect(auth.PermWorldRead, d.employees.List))
	r.Handle("GET /api/v1/employees/{id}", protect(auth.PermWorldRead, d.employees.Get))
	r.Handle("GET /api/v1/assets", protect(auth.PermWorldRead, d.assets.List))
	r.Handle("GET /api/v1/assets/graph", protect(auth.PermWorldRead, d.assets.Graph))
	r.Handle("GET /api/v1/assets/{id}", protect(auth.PermWorldRead, d.assets.Get))
	r.Handle("GET /api/v1/world", protect(auth.PermWorldRead, d.world.World))
	r.Handle("GET /api/v1/world/zones/{id}", protect(auth.PermWorldRead, d.world.Zone))
	r.Handle("GET /api/v1/world/objects/{key}", protect(auth.PermWorldRead, d.world.Object))

	return httpx.Chain(r,
		httpx.RequestID,
		httpx.ClientIPMiddleware(httpx.NewClientIPResolver(d.cfg.HTTP.TrustedProxies)),
		httpx.AccessLog(d.log, pathHealth, pathReady),
		httpx.Recover(d.log),
		httpx.MaxBodyBytes(d.cfg.HTTP.MaxBodyBytes),
		httpx.Timeout(d.cfg.HTTP.RequestTimeout),
	)
}

// serveOpenAPI publishes the API description; it contains no secrets and
// lets the frontend generate typed clients.
func serveOpenAPI(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "public, max-age=300")
	_, _ = w.Write(api.OpenAPI)
}
