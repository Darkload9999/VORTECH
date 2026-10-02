package main

import (
	"log/slog"
	"net/http"

	"github.com/vortech/backend/api"
	"github.com/vortech/backend/internal/auth"
	"github.com/vortech/backend/internal/config"
	"github.com/vortech/backend/internal/health"
	"github.com/vortech/backend/internal/httpx"
	"github.com/vortech/backend/internal/player"
)

const (
	pathHealth = "/api/v1/health"
	pathReady  = "/api/v1/ready"
)

// deps are the constructed modules the HTTP layer routes to.
type deps struct {
	cfg     *config.Config
	log     *slog.Logger
	health  *health.Service
	authn   *auth.Authenticator
	players *player.Handler
}

// newHandler is the composition root for HTTP routes. Every route except the
// probes and the API description goes through authn.Protect with an
// explicit permission; routes_test.go enforces this and the OpenAPI sync.
func newHandler(d deps) http.Handler {
	r := httpx.NewRouter()

	r.HandleFunc("GET "+pathHealth, d.health.Live)
	r.HandleFunc("GET "+pathReady, d.health.Ready)
	r.HandleFunc("GET /api/v1/openapi.json", serveOpenAPI)

	r.Handle("GET /api/v1/me", d.authn.Protect(auth.PermProfileReadOwn, http.HandlerFunc(d.players.Me)))

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
