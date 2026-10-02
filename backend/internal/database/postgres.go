// Package database owns PostgreSQL connectivity: the shared connection pool,
// schema migrations and the sqlc-generated query layer (subpackage db).
package database

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vortech/backend/internal/config"
)

// idleInTransactionTimeout terminates sessions that hold a transaction open
// without doing work (e.g. a handler that forgot to commit), releasing locks.
const idleInTransactionTimeout = 60 * time.Second

// Connect creates the connection pool and waits until PostgreSQL accepts
// connections, retrying with backoff for up to cfg.StartupTimeout. This
// tolerates the database starting slightly after the application (compose,
// node reboots) while still failing fast on persistent errors.
func Connect(ctx context.Context, cfg config.DatabaseConfig, appName string, log *slog.Logger) (*pgxpool.Pool, error) {
	pcfg, err := poolConfig(cfg, appName)
	if err != nil {
		return nil, err
	}

	pool, err := pgxpool.NewWithConfig(ctx, pcfg)
	if err != nil {
		return nil, fmt.Errorf("create connection pool: %w", err)
	}

	startupCtx, cancel := context.WithTimeout(ctx, cfg.StartupTimeout)
	defer cancel()

	backoff := 250 * time.Millisecond
	for attempt := 1; ; attempt++ {
		pingCtx, pingCancel := context.WithTimeout(startupCtx, cfg.ConnectTimeout)
		err = pool.Ping(pingCtx)
		pingCancel()
		if err == nil {
			log.Info("connected to postgres",
				"host", pcfg.ConnConfig.Host,
				"database", pcfg.ConnConfig.Database,
				"max_conns", pcfg.MaxConns,
				"attempts", attempt)
			return pool, nil
		}

		log.Warn("postgres not reachable yet", "attempt", attempt, "error", err, "retry_in", backoff)
		select {
		case <-startupCtx.Done():
			pool.Close()
			return nil, fmt.Errorf("postgres unreachable after %d attempts within %s: %w", attempt, cfg.StartupTimeout, err)
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 5*time.Second)
	}
}

func poolConfig(cfg config.DatabaseConfig, appName string) (*pgxpool.Config, error) {
	pcfg, err := pgxpool.ParseConfig(cfg.URL)
	if err != nil {
		// pgx redacts the password in parse errors, but keep the message
		// generic regardless: it may still echo other connection details.
		return nil, errors.New("parse DATABASE_URL: invalid connection string")
	}

	pcfg.MaxConns = cfg.MaxConns
	pcfg.MinConns = cfg.MinConns
	pcfg.MaxConnLifetime = cfg.MaxConnLifetime
	// Spread reconnects so connections created together don't expire together.
	pcfg.MaxConnLifetimeJitter = cfg.MaxConnLifetime / 10
	pcfg.MaxConnIdleTime = cfg.MaxConnIdleTime
	pcfg.HealthCheckPeriod = 30 * time.Second
	pcfg.ConnConfig.ConnectTimeout = cfg.ConnectTimeout

	rp := pcfg.ConnConfig.RuntimeParams
	if _, ok := rp["application_name"]; !ok {
		rp["application_name"] = appName
	}
	// Server-side guards: a runaway query cannot hold a pool connection
	// forever even if a caller forgets a context deadline.
	rp["statement_timeout"] = strconv.FormatInt(cfg.StatementTimeout.Milliseconds(), 10)
	rp["idle_in_transaction_session_timeout"] = strconv.FormatInt(idleInTransactionTimeout.Milliseconds(), 10)
	rp["timezone"] = "UTC"
	return pcfg, nil
}

// PingCheck adapts the pool into a readiness check function.
func PingCheck(pool *pgxpool.Pool) func(context.Context) error {
	return pool.Ping
}
