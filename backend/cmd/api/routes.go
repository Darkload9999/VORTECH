package main

import (
	"log/slog"
	"net/http"

	"github.com/vortech/backend/internal/config"
	"github.com/vortech/backend/internal/health"
	"github.com/vortech/backend/internal/httpx"
)

const (
	pathHealth = "/api/v1/health"
	pathReady  = "/api/v1/ready"
)

// newHandler is the composition root for HTTP routes. Each module will
// expose its own route registration here as it is implemented.
func newHandler(cfg *config.Config, log *slog.Logger, hs *health.Service) http.Handler {
	r := httpx.NewRouter()

	r.HandleFunc("GET "+pathHealth, hs.Live)
	r.HandleFunc("GET "+pathReady, hs.Ready)

	return httpx.Chain(r,
		httpx.RequestID,
		httpx.ClientIPMiddleware(httpx.NewClientIPResolver(cfg.HTTP.TrustedProxies)),
		httpx.AccessLog(log, pathHealth, pathReady),
		httpx.Recover(log),
		httpx.MaxBodyBytes(cfg.HTTP.MaxBodyBytes),
		httpx.Timeout(cfg.HTTP.RequestTimeout),
	)
}
