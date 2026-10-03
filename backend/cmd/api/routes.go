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
	"github.com/Darkload9999/VORTECH/backend/internal/notification"
	"github.com/Darkload9999/VORTECH/backend/internal/player"
	"github.com/Darkload9999/VORTECH/backend/internal/websocket"
	"github.com/Darkload9999/VORTECH/backend/internal/world"
)

const (
	pathHealth    = "/api/v1/health"
	pathReady     = "/api/v1/ready"
	pathWebSocket = "/ws"
)

// deps are the constructed modules the HTTP layer routes to.
type deps struct {
	cfg           *config.Config
	log           *slog.Logger
	health        *health.Service
	authn         *auth.Authenticator
	players       *player.Handler
	companies     *company.Handler
	employees     *employee.Handler
	assets        *asset.Handler
	world         *world.Handler
	progress      *career.Handler
	interactions  *interaction.Handler
	notifications *notification.Handler
	announcer     *world.Announcer
	gateway       *websocket.Gateway
}

// newHandler is the composition root for HTTP routes. Every route except the
// probes, the API description and the ticket-authenticated WebSocket goes
// through authn.Protect with an explicit permission; routes_test.go
// enforces this and the OpenAPI sync.
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

	r.Handle("POST /api/v1/ws/tickets", protect(auth.PermProfileReadOwn, d.gateway.IssueTicket))
	r.Handle("GET /api/v1/notifications", protect(auth.PermProfileReadOwn, d.notifications.List))
	r.Handle("POST /api/v1/notifications/{id}/read", protect(auth.PermProfileReadOwn, d.notifications.MarkRead))
	r.Handle("POST /api/v1/notifications/read-all", protect(auth.PermProfileReadOwn, d.notifications.MarkAllRead))
	r.Handle("POST /api/v1/admin/announcements", protect(auth.PermPlatformAdminister, d.announcer.Create))

	clientIP := httpx.ClientIPMiddleware(httpx.NewClientIPResolver(d.cfg.HTTP.TrustedProxies))
	api := httpx.Chain(r,
		httpx.RequestID,
		clientIP,
		httpx.AccessLog(d.log, pathHealth, pathReady),
		httpx.Recover(d.log),
		httpx.MaxBodyBytes(d.cfg.HTTP.MaxBodyBytes),
		httpx.Timeout(d.cfg.HTTP.RequestTimeout),
	)
	// Long-lived WebSockets bypass the request timeout and body limit.
	ws := httpx.Chain(http.HandlerFunc(d.gateway.Serve),
		httpx.RequestID,
		clientIP,
		httpx.AccessLog(d.log),
		httpx.Recover(d.log),
	)

	top := http.NewServeMux()
	top.Handle("GET "+pathWebSocket, ws)
	top.Handle("/", api)
	return top
}

// serveOpenAPI publishes the API description; it contains no secrets and
// lets the frontend generate typed clients.
func serveOpenAPI(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "public, max-age=300")
	_, _ = w.Write(api.OpenAPI)
}
