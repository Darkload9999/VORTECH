//go:build integration

// Package integration runs tests against real dependencies.
//
// TEST_DATABASE_URL must point at a PostgreSQL 18+ server where the user may
// CREATE DATABASE. Each run creates a uniquely named, disposable database
// and drops it afterwards, so runs never interfere with each other or with
// development data.
//
//	make test-integration
package integration

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Darkload9999/VORTECH/backend/internal/config"
	"github.com/Darkload9999/VORTECH/backend/internal/database"
)

// testDBURL is the connection string of the disposable database.
var testDBURL string

func TestMain(m *testing.M) {
	adminURL := os.Getenv("TEST_DATABASE_URL")
	if adminURL == "" {
		fmt.Fprintln(os.Stderr, "TEST_DATABASE_URL not set; skipping integration tests")
		os.Exit(0)
	}
	os.Exit(runWithDisposableDB(m, adminURL))
}

func runWithDisposableDB(m *testing.M, adminURL string) int {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	admin, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		fmt.Fprintln(os.Stderr, "connect to TEST_DATABASE_URL:", err)
		return 1
	}
	defer admin.Close(context.Background())

	name := "vortech_it_" + uuid.NewString()[:8]
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		fmt.Fprintln(os.Stderr, "create test database:", err)
		return 1
	}
	defer func() {
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer dropCancel()
		if _, err := admin.Exec(dropCtx, "DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)"); err != nil {
			fmt.Fprintln(os.Stderr, "drop test database:", err)
		}
	}()

	u, err := url.Parse(adminURL)
	if err != nil {
		fmt.Fprintln(os.Stderr, "parse TEST_DATABASE_URL:", err)
		return 1
	}
	u.Path = "/" + name
	testDBURL = u.String()

	return m.Run()
}

func testLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

func dbConfig() config.DatabaseConfig {
	return config.DatabaseConfig{
		URL:              testDBURL,
		MaxConns:         5,
		MinConns:         0,
		MaxConnLifetime:  time.Hour,
		MaxConnIdleTime:  time.Minute,
		ConnectTimeout:   5 * time.Second,
		StartupTimeout:   10 * time.Second,
		StatementTimeout: 5 * time.Second,
	}
}

// migratedPool returns a pool on the disposable database with all
// migrations applied.
func migratedPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	if err := database.PrepareSchema(ctx, testDBURL, true, testLogger()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := database.Connect(ctx, dbConfig(), "vortech-it", testLogger())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}
