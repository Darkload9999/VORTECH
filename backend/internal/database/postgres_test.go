package database

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/vortech/backend/internal/config"
)

func testDBConfig(url string) config.DatabaseConfig {
	return config.DatabaseConfig{
		URL:              url,
		MaxConns:         10,
		MinConns:         1,
		MaxConnLifetime:  time.Hour,
		MaxConnIdleTime:  time.Minute,
		ConnectTimeout:   200 * time.Millisecond,
		StartupTimeout:   600 * time.Millisecond,
		StatementTimeout: 15 * time.Second,
	}
}

func TestPoolConfigAppliesSettings(t *testing.T) {
	pcfg, err := poolConfig(testDBConfig("postgres://u:p@db.internal:5432/vortech?sslmode=disable"), "vortech-api")
	if err != nil {
		t.Fatal(err)
	}
	if pcfg.MaxConns != 10 || pcfg.MinConns != 1 {
		t.Errorf("pool sizes not applied: max=%d min=%d", pcfg.MaxConns, pcfg.MinConns)
	}
	if pcfg.MaxConnLifetimeJitter != 6*time.Minute {
		t.Errorf("lifetime jitter = %s", pcfg.MaxConnLifetimeJitter)
	}
	rp := pcfg.ConnConfig.RuntimeParams
	if rp["statement_timeout"] != "15000" {
		t.Errorf("statement_timeout = %q", rp["statement_timeout"])
	}
	if rp["idle_in_transaction_session_timeout"] != "60000" {
		t.Errorf("idle_in_transaction_session_timeout = %q", rp["idle_in_transaction_session_timeout"])
	}
	if rp["application_name"] != "vortech-api" || rp["timezone"] != "UTC" {
		t.Errorf("runtime params = %v", rp)
	}
}

func TestPoolConfigRespectsExplicitApplicationName(t *testing.T) {
	pcfg, err := poolConfig(testDBConfig("postgres://u:p@h:5432/db?application_name=custom"), "vortech-api")
	if err != nil {
		t.Fatal(err)
	}
	if got := pcfg.ConnConfig.RuntimeParams["application_name"]; got != "custom" {
		t.Errorf("application_name = %q, want custom", got)
	}
}

func TestPoolConfigErrorDoesNotLeakCredentials(t *testing.T) {
	_, err := poolConfig(testDBConfig("postgres://u:hunter2@h:notaport/db"), "x")
	if err == nil {
		t.Fatal("expected parse error")
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("error leaks password: %v", err)
	}
}

func TestConnectFailsFastWhenUnreachable(t *testing.T) {
	// Port 1 on loopback refuses connections immediately.
	cfg := testDBConfig("postgres://u:p@127.0.0.1:1/db?sslmode=disable")
	start := time.Now()
	_, err := Connect(context.Background(), cfg, "test", slog.New(slog.DiscardHandler))
	if err == nil {
		t.Fatal("expected error connecting to closed port")
	}
	if !strings.Contains(err.Error(), "postgres unreachable") {
		t.Fatalf("unexpected error: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("startup timeout not honoured, took %s", elapsed)
	}
}
