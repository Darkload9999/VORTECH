// Command api runs the VORTECH platform HTTP API.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Darkload9999/VORTECH/backend/internal/asset"
	"github.com/Darkload9999/VORTECH/backend/internal/audit"
	"github.com/Darkload9999/VORTECH/backend/internal/auth"
	"github.com/Darkload9999/VORTECH/backend/internal/buildinfo"
	"github.com/Darkload9999/VORTECH/backend/internal/career"
	"github.com/Darkload9999/VORTECH/backend/internal/company"
	"github.com/Darkload9999/VORTECH/backend/internal/config"
	"github.com/Darkload9999/VORTECH/backend/internal/database"
	"github.com/Darkload9999/VORTECH/backend/internal/database/db"
	"github.com/Darkload9999/VORTECH/backend/internal/employee"
	"github.com/Darkload9999/VORTECH/backend/internal/event"
	"github.com/Darkload9999/VORTECH/backend/internal/health"
	"github.com/Darkload9999/VORTECH/backend/internal/interaction"
	"github.com/Darkload9999/VORTECH/backend/internal/logging"
	"github.com/Darkload9999/VORTECH/backend/internal/notification"
	"github.com/Darkload9999/VORTECH/backend/internal/player"
	"github.com/Darkload9999/VORTECH/backend/internal/server"
	"github.com/Darkload9999/VORTECH/backend/internal/websocket"
	"github.com/Darkload9999/VORTECH/backend/internal/world"
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
	rtCfg, err := config.LoadRealtime(os.LookupEnv)
	if err != nil {
		return err
	}
	log := logging.New(os.Stdout, cfg.Log, "api")
	log.Info("starting", "commit", buildinfo.Commit, "build_time", buildinfo.BuildTime,
		"config", cfg, "auth", authCfg, "realtime", rtCfg)

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
	auditRec := audit.NewRecorder(queries, log)
	authn := auth.NewAuthenticator(verifier,
		player.NewDirectory(pool, log, authCfg.IdentityCacheTTL),
		auditRec,
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
	// The active world changes only when a scenario is activated; a short
	// cache avoids a lookup on every request.
	companies := company.NewDirectory(queries, 10*time.Second)
	careers := career.NewService(queries)

	// Internal event bus → WebSocket gateway. Events are published after
	// the producing transaction commits.
	bus := event.NewBus(log)
	gateway := websocket.NewGateway(rtCfg, websocket.NewTicketStore(rtCfg.TicketTTL),
		websocket.WorldAuthorizer{Dir: companies, Q: queries, Access: careers}, bus, log)
	unsubscribe := bus.Subscribe("websocket-gateway", func(e event.Event) bool { return e.Topic != "" }, gateway.Deliver)

	interactions := interaction.NewService(pool, companies, careers, bus)
	handler := newHandler(deps{
		cfg:       cfg,
		log:       log,
		health:    hs,
		authn:     authn,
		players:   player.NewHandler(queries, log),
		companies: company.NewHandler(companies, queries, log),
		employees: employee.NewHandler(companies, queries, log),
		assets:    asset.NewHandler(companies, queries, log),
		world:     world.NewHandler(companies, queries, careers, log),
		progress:  career.NewHandler(careers, companies, queries, log),
		// 10 interactions/s sustained per player, bursts of 20.
		interactions:  interaction.NewHandler(interactions, interaction.NewLimiter(10, 20), log),
		notifications: notification.NewHandler(queries, log),
		announcer:     world.NewAnnouncer(companies, bus, auditRec, log),
		gateway:       gateway,
	})
	srv := server.New(cfg.HTTP.Addr, handler, cfg.HTTP, log)

	err = server.Run(ctx, log, srv, ln, server.ShutdownOptions{
		OnDrain: func() {
			hs.SetDraining()
			// Close WebSockets with "going away" so clients reconnect
			// (to another instance) while this one drains.
			gateway.Drain()
		},
		DrainDelay: cfg.Shutdown.DrainDelay,
		Timeout:    cfg.Shutdown.Timeout,
	})

	// Hijacked WebSocket connections are not tracked by http.Server.Shutdown.
	waitCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if werr := gateway.Wait(waitCtx); werr != nil {
		log.Warn("websocket handlers still running at shutdown", "error", werr)
	}
	unsubscribe()
	if berr := bus.Close(waitCtx); berr != nil {
		log.Warn("event bus did not stop cleanly", "error", berr)
	}
	if err != nil {
		return err
	}
	log.Info("shutdown complete")
	return nil
}
