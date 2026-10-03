package config

import (
	"strings"
	"testing"
	"time"
)

func TestLoadRealtime(t *testing.T) {
	cfg, err := LoadRealtime(env(map[string]string{"WS_ALLOWED_ORIGINS": "localhost:5173, *.vortech.test"}))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.AllowedOrigins) != 2 || cfg.MaxConnectionsPerUser != 5 || cfg.MaxConnectionAge != 30*time.Minute || cfg.TicketTTL != 30*time.Second {
		t.Fatalf("unexpected config %+v", cfg)
	}

	for name, e := range map[string]map[string]string{
		"wildcard origin":      {"WS_ALLOWED_ORIGINS": "*"},
		"scheme in origin":     {"WS_ALLOWED_ORIGINS": "https://evil.example"},
		"per-user above total": {"WS_MAX_CONNECTIONS": "2", "WS_MAX_CONNECTIONS_PER_USER": "5"},
		"age too short":        {"WS_MAX_CONNECTION_AGE": "10s"},
		"ticket ttl too long":  {"WS_TICKET_TTL": "1h"},
	} {
		if _, err := LoadRealtime(env(e)); err == nil || !strings.Contains(err.Error(), "WS_") {
			t.Errorf("%s: expected validation error, got %v", name, err)
		}
	}
}
