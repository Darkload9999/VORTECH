package config

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// RealtimeConfig configures the WebSocket gateway.
type RealtimeConfig struct {
	// AllowedOrigins are extra Origin host patterns (e.g. "localhost:5173")
	// accepted besides same-origin. Production sits behind one origin via
	// OpenResty and needs none.
	AllowedOrigins        []string
	MaxConnections        int
	MaxConnectionsPerUser int
	// MaxConnectionAge forces periodic reconnects so every connection is
	// re-authorised with a fresh ticket (and thus a valid access token).
	MaxConnectionAge time.Duration
	PingInterval     time.Duration
	TicketTTL        time.Duration
}

// LoadRealtime reads and validates the WebSocket configuration.
func LoadRealtime(lookup LookupFunc) (RealtimeConfig, error) {
	p := parser{lookup: lookup}
	cfg := RealtimeConfig{
		MaxConnections:        int(p.int32("WS_MAX_CONNECTIONS", 5000)),
		MaxConnectionsPerUser: int(p.int32("WS_MAX_CONNECTIONS_PER_USER", 5)),
		MaxConnectionAge:      p.duration("WS_MAX_CONNECTION_AGE", 30*time.Minute),
		PingInterval:          p.duration("WS_PING_INTERVAL", 25*time.Second),
		TicketTTL:             p.duration("WS_TICKET_TTL", 30*time.Second),
	}
	for _, o := range strings.Split(p.str("WS_ALLOWED_ORIGINS", ""), ",") {
		if o = strings.TrimSpace(o); o != "" {
			cfg.AllowedOrigins = append(cfg.AllowedOrigins, o)
		}
	}

	errs := p.errs
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	if cfg.MaxConnections < 1 {
		add("WS_MAX_CONNECTIONS: must be at least 1")
	}
	if cfg.MaxConnectionsPerUser < 1 || cfg.MaxConnectionsPerUser > cfg.MaxConnections {
		add("WS_MAX_CONNECTIONS_PER_USER: must be between 1 and WS_MAX_CONNECTIONS")
	}
	if cfg.MaxConnectionAge < time.Minute || cfg.MaxConnectionAge > 24*time.Hour {
		add("WS_MAX_CONNECTION_AGE: must be between 1m and 24h")
	}
	if cfg.PingInterval < time.Second || cfg.PingInterval > 5*time.Minute {
		add("WS_PING_INTERVAL: must be between 1s and 5m")
	}
	if cfg.TicketTTL < time.Second || cfg.TicketTTL > 5*time.Minute {
		add("WS_TICKET_TTL: must be between 1s and 5m")
	}
	for _, o := range cfg.AllowedOrigins {
		if o == "*" || strings.Contains(o, "://") || strings.ContainsAny(o, " /") {
			add("WS_ALLOWED_ORIGINS: %q must be a host[:port] pattern (no scheme, no bare *)", o)
		}
	}
	if len(errs) > 0 {
		return RealtimeConfig{}, fmt.Errorf("invalid realtime configuration:\n%w", errors.Join(errs...))
	}
	return cfg, nil
}

// LogValue implements slog.LogValuer.
func (r RealtimeConfig) LogValue() slog.Value {
	return slog.GroupValue(
		slog.Any("allowed_origins", r.AllowedOrigins),
		slog.Int("max_connections", r.MaxConnections),
		slog.Int("max_connections_per_user", r.MaxConnectionsPerUser),
		slog.Duration("max_connection_age", r.MaxConnectionAge),
		slog.Duration("ping_interval", r.PingInterval),
	)
}
