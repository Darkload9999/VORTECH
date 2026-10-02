// Command api runs the VORTECH platform HTTP API.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/vortech/backend/internal/buildinfo"
	"github.com/vortech/backend/internal/config"
	"github.com/vortech/backend/internal/database"
	"github.com/vortech/backend/internal/health"
	"github.com/vortech/backend/internal/logging"
	"github.com/vortech/backend/internal/server"
)

func main() {
	healthcheck := flag.Bool("healthcheck", false, "probe the local liveness endpoint and exit (container HEALTHCHECK)")
	flag.Parse()

	if *healthcheck {
		addr, ok := os.LookupEnv("HTTP_ADDR")
		if !ok || addr == "" {
			addr = ":8080"
		}
		if err := server.Probe(context.Background(), addr, pathHealth); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}

	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "api: fatal:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load(os.LookupEnv)
	if err != nil {
		return err
	}
	log := logging.New(os.Stdout, cfg.Log, "api")
	log.Info("starting", "commit", buildinfo.Commit, "build_time", buildinfo.BuildTime, "config", cfg)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// After the first signal, restore default handling so a second
	// SIGINT/SIGTERM terminates immediately instead of waiting for the drain.
	context.AfterFunc(ctx, stop)

	pool, err := database.Connect(ctx, cfg.Database, "vortech-api", log)
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

	ln, err := server.Listen(cfg.HTTP.Addr)
	if err != nil {
		return err
	}
	srv := server.New(cfg.HTTP.Addr, newHandler(cfg, log, hs), cfg.HTTP, log)

	err = server.Run(ctx, log, srv, ln, server.ShutdownOptions{
		OnDrain:    hs.SetDraining,
		DrainDelay: cfg.Shutdown.DrainDelay,
		Timeout:    cfg.Shutdown.Timeout,
	})
	if err != nil {
		return err
	}
	log.Info("shutdown complete")
	return nil
}
