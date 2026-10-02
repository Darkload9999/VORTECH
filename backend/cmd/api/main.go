// Command api runs the VORTECH platform HTTP API.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/vortech/backend/internal/audit"
	"github.com/vortech/backend/internal/auth"
	"github.com/vortech/backend/internal/buildinfo"
	"github.com/vortech/backend/internal/config"
	"github.com/vortech/backend/internal/database"
	"github.com/vortech/backend/internal/database/db"
	"github.com/vortech/backend/internal/health"
	"github.com/vortech/backend/internal/logging"
	"github.com/vortech/backend/internal/player"
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
	authCfg, err := config.LoadAuth(os.LookupEnv, cfg.Env)
	if err != nil {
		return err
	}
	log := logging.New(os.Stdout, cfg.Log, "api")
	log.Info("starting", "commit", buildinfo.Commit, "build_time", buildinfo.BuildTime, "config", cfg, "auth", authCfg)

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

	// Signing keys load in the background: Keycloak being briefly
	// unavailable delays readiness instead of failing startup.
	keys := auth.NewKeySet(authCfg.JWKSURL(), log)
	go keys.Run(ctx, authCfg.JWKSRefreshInterval)

	queries := db.New(pool)
	verifier := auth.NewVerifier(keys, auth.VerifierConfig{
		Issuer:   authCfg.Issuer(),
		Audience: authCfg.Audience,
		ClientID: authCfg.ClientID,
		Leeway:   authCfg.ClockSkew,
	})
	authn := auth.NewAuthenticator(verifier,
		player.NewDirectory(pool, log, authCfg.IdentityCacheTTL),
		audit.NewRecorder(queries, log),
		log,
	)

	hs := health.New(log, cfg.Health.CheckTimeout, cfg.Health.CacheTTL,
		health.Check{Name: "postgres", Fn: database.PingCheck(pool)},
		health.Check{Name: "keycloak", Fn: keys.Ready},
	)

	ln, err := server.Listen(cfg.HTTP.Addr)
	if err != nil {
		return err
	}
	handler := newHandler(deps{
		cfg:     cfg,
		log:     log,
		health:  hs,
		authn:   authn,
		players: player.NewHandler(queries, log),
	})
	srv := server.New(cfg.HTTP.Addr, handler, cfg.HTTP, log)

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
