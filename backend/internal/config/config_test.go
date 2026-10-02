package config

import (
	"log/slog"
	"strings"
	"testing"
	"time"
)

const testDSN = "postgres://vortech:s3cret-pass@localhost:5432/vortech?sslmode=disable"

func env(kv map[string]string) LookupFunc {
	return func(k string) (string, bool) {
		v, ok := kv[k]
		return v, ok
	}
}

func TestLoadDefaults(t *testing.T) {
	cfg, err := Load(env(map[string]string{"DATABASE_URL": testDSN}))
	if err != nil {
		t.Fatalf("Load() unexpected error: %v", err)
	}
	if cfg.Env != EnvDevelopment {
		t.Errorf("Env = %q, want development", cfg.Env)
	}
	if cfg.HTTP.Addr != ":8080" {
		t.Errorf("HTTP.Addr = %q, want :8080", cfg.HTTP.Addr)
	}
	if cfg.Log.Format != LogFormatText {
		t.Errorf("development should default to text logs, got %q", cfg.Log.Format)
	}
	if !cfg.Database.AutoMigrate {
		t.Error("development should auto-migrate by default")
	}
	if cfg.Database.MaxConns != 20 || cfg.Database.MinConns != 2 {
		t.Errorf("unexpected pool defaults: max=%d min=%d", cfg.Database.MaxConns, cfg.Database.MinConns)
	}
	if cfg.HTTP.MaxBodyBytes != 1<<20 {
		t.Errorf("MaxBodyBytes = %d, want 1MiB", cfg.HTTP.MaxBodyBytes)
	}
}

func TestLoadProductionDefaults(t *testing.T) {
	cfg, err := Load(env(map[string]string{"APP_ENV": "production", "DATABASE_URL": testDSN}))
	if err != nil {
		t.Fatalf("Load() unexpected error: %v", err)
	}
	if cfg.Log.Format != LogFormatJSON {
		t.Errorf("production should default to json logs, got %q", cfg.Log.Format)
	}
	if cfg.Database.AutoMigrate {
		t.Error("production must not auto-migrate by default")
	}
	if !cfg.IsProduction() {
		t.Error("IsProduction() = false")
	}
}

func TestLoadOverrides(t *testing.T) {
	cfg, err := Load(env(map[string]string{
		"APP_ENV":               "Staging",
		"DATABASE_URL":          testDSN,
		"HTTP_ADDR":             "127.0.0.1:9000",
		"HTTP_REQUEST_TIMEOUT":  "3s",
		"LOG_LEVEL":             "debug",
		"DATABASE_MAX_CONNS":    "40",
		"HTTP_TRUSTED_PROXIES":  "10.42.0.0/16, 127.0.0.1 ,::1",
		"SHUTDOWN_DRAIN_DELAY":  "5s",
		"DATABASE_AUTO_MIGRATE": "true",
	}))
	if err != nil {
		t.Fatalf("Load() unexpected error: %v", err)
	}
	if cfg.Env != EnvStaging {
		t.Errorf("APP_ENV should be case-insensitive, got %q", cfg.Env)
	}
	if cfg.HTTP.RequestTimeout != 3*time.Second {
		t.Errorf("RequestTimeout = %s", cfg.HTTP.RequestTimeout)
	}
	if cfg.Log.Level != slog.LevelDebug {
		t.Errorf("Log.Level = %s", cfg.Log.Level)
	}
	if cfg.Database.MaxConns != 40 {
		t.Errorf("MaxConns = %d", cfg.Database.MaxConns)
	}
	got := make([]string, len(cfg.HTTP.TrustedProxies))
	for i, p := range cfg.HTTP.TrustedProxies {
		got[i] = p.String()
	}
	if strings.Join(got, ",") != "10.42.0.0/16,127.0.0.1/32,::1/128" {
		t.Errorf("TrustedProxies = %v", got)
	}
	if !cfg.Database.AutoMigrate {
		t.Error("explicit DATABASE_AUTO_MIGRATE=true ignored")
	}
}

func TestLoadValidationErrors(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		wantErr string
	}{
		{"missing database url", map[string]string{}, "DATABASE_URL: required"},
		{"bad env", map[string]string{"APP_ENV": "prod", "DATABASE_URL": testDSN}, "APP_ENV"},
		{"bad scheme", map[string]string{"DATABASE_URL": "mysql://u:p@h/db"}, "scheme must be postgres"},
		{"no database name", map[string]string{"DATABASE_URL": "postgres://u:p@h:5432/"}, "database name is required"},
		{"bad addr", map[string]string{"DATABASE_URL": testDSN, "HTTP_ADDR": "8080"}, "HTTP_ADDR"},
		{"bad port", map[string]string{"DATABASE_URL": testDSN, "HTTP_ADDR": ":99999"}, "invalid port"},
		{"bad duration", map[string]string{"DATABASE_URL": testDSN, "HTTP_READ_TIMEOUT": "ten"}, "HTTP_READ_TIMEOUT: expected a duration"},
		{"zero duration", map[string]string{"DATABASE_URL": testDSN, "SHUTDOWN_TIMEOUT": "0s"}, "SHUTDOWN_TIMEOUT: must be greater than zero"},
		{"request exceeds write timeout", map[string]string{"DATABASE_URL": testDSN, "HTTP_REQUEST_TIMEOUT": "60s"}, "must not exceed HTTP_WRITE_TIMEOUT"},
		{"bad level", map[string]string{"DATABASE_URL": testDSN, "LOG_LEVEL": "loud"}, "LOG_LEVEL"},
		{"bad format", map[string]string{"DATABASE_URL": testDSN, "LOG_FORMAT": "xml"}, "LOG_FORMAT"},
		{"min > max conns", map[string]string{"DATABASE_URL": testDSN, "DATABASE_MIN_CONNS": "30"}, "must not exceed DATABASE_MAX_CONNS"},
		{"bad bool", map[string]string{"DATABASE_URL": testDSN, "DATABASE_AUTO_MIGRATE": "maybe"}, "DATABASE_AUTO_MIGRATE"},
		{"bad proxy", map[string]string{"DATABASE_URL": testDSN, "HTTP_TRUSTED_PROXIES": "10.0.0.0/33"}, "HTTP_TRUSTED_PROXIES"},
		{"text logs in production", map[string]string{"APP_ENV": "production", "DATABASE_URL": testDSN, "LOG_FORMAT": "text"}, "must be json in production"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(env(tt.env))
			if err == nil {
				t.Fatalf("Load() succeeded, want error containing %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error %q does not contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestLoadReportsAllErrors(t *testing.T) {
	_, err := Load(env(map[string]string{"APP_ENV": "nope", "LOG_FORMAT": "xml"}))
	if err == nil {
		t.Fatal("expected error")
	}
	for _, want := range []string{"APP_ENV", "LOG_FORMAT", "DATABASE_URL"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("aggregated error missing %s: %v", want, err)
		}
	}
}

func TestErrorsNeverLeakDatabasePassword(t *testing.T) {
	// A URL that fails validation must not echo its credentials.
	_, err := Load(env(map[string]string{"DATABASE_URL": "mysql://user:hunter2@h/db"}))
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("error leaks password: %v", err)
	}
	_, err = Load(env(map[string]string{"DATABASE_URL": "postgres://user:hunter2@[::1"}))
	if err == nil || strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("unparseable URL error leaks password or is nil: %v", err)
	}
}

func TestLogValueRedactsSecrets(t *testing.T) {
	cfg, err := Load(env(map[string]string{"DATABASE_URL": testDSN}))
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	slog.New(slog.NewTextHandler(&b, nil)).Info("cfg", "config", cfg)
	out := b.String()
	if strings.Contains(out, "s3cret-pass") {
		t.Fatalf("log output leaks password: %s", out)
	}
	if !strings.Contains(out, "REDACTED") {
		t.Fatalf("expected redacted database URL in log output: %s", out)
	}
}

func TestRedactURL(t *testing.T) {
	got := RedactURL("postgres://u:p@h:5432/db?sslmode=disable&password=x")
	if got != "postgres://u:REDACTED@h:5432/db" {
		t.Errorf("RedactURL() = %q", got)
	}
	if got := RedactURL("postgres://h/db"); got != "postgres://h/db" {
		t.Errorf("RedactURL() without credentials = %q", got)
	}
}
