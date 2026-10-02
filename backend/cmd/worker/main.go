// Command worker runs asynchronous background jobs (range provisioning,
// reconciliation, cleanup) outside the request path.
//
// Phase 1 provides the process lifecycle only: configuration, logging,
// database connectivity, schema verification, probes and graceful shutdown.
// Job handlers are registered in Phase 5 (range management).
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/Darkload9999/VORTECH/backend/internal/buildinfo"
	"github.com/Darkload9999/VORTECH/backend/internal/config"
	"github.com/Darkload9999/VORTECH/backend/internal/database"
	"github.com/Darkload9999/VORTECH/backend/internal/health"
	"github.com/Darkload9999/VORTECH/backend/internal/httpx"
	"github.com/Darkload9999/VORTECH/backend/internal/logging"
	"github.com/Darkload9999/VORTECH/backend/internal/server"
)

const (
	pathHealth = "/health"
	pathReady  = "/ready"
)

func main() {
	healthcheck := flag.Bool("healthcheck", false, "probe the local liveness endpoint and exit (container HEALTHCHECK)")
	flag.Parse()

	if *healthcheck {
		addr, ok := os.LookupEnv("WORKER_HTTP_ADDR")
		if !ok || addr == "" {
			addr = ":8081"
		}
		if err := server.Probe(context.Background(), addr, pathHealth); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}

	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "worker: fatal:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load(os.LookupEnv)
	if err != nil {
		return err
	}
	log := logging.New(os.Stdout, cfg.Log, "worker")
	log.Info("starting", "commit", buildinfo.Commit, "build_time", buildinfo.BuildTime, "config", cfg)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	context.AfterFunc(ctx, stop)

	pool, err := database.Connect(ctx, cfg.Database, "vortech-worker", log)
	if err != nil {
		return err
	}
	defer func() {
		pool.Close()
		log.Info("postgres pool closed")
	}()

	if err := database.PrepareSchema(ctx, cfg.Database.URL, cfg.Database.AutoMigrate, log); err != nil {
		return err
	}

	hs := health.New(log, cfg.Health.CheckTimeout, cfg.Health.CacheTTL,
		health.Check{Name: "postgres", Fn: database.PingCheck(pool)},
	)

	ln, err := server.Listen(cfg.Worker.HTTPAddr)
	if err != nil {
		return err
	}
	srv := server.New(cfg.Worker.HTTPAddr, probeHandler(log, hs), cfg.HTTP, log)

	log.Info("worker ready; no job handlers are registered until range management (phase 5)")

	err = server.Run(ctx, log, srv, ln, server.ShutdownOptions{
		OnDrain: hs.SetDraining,
		Timeout: cfg.Shutdown.Timeout,
	})
	if err != nil {
		return err
	}
	log.Info("shutdown complete")
	return nil
}

// probeHandler serves the internal-only probe endpoints.
func probeHandler(log *slog.Logger, hs *health.Service) http.Handler {
	r := httpx.NewRouter()
	r.HandleFunc("GET "+pathHealth, hs.Live)
	r.HandleFunc("GET "+pathReady, hs.Ready)
	return httpx.Chain(r, httpx.RequestID, httpx.AccessLog(log, pathHealth, pathReady), httpx.Recover(log))
}
