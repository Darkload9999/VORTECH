// Package config loads and validates process configuration from the
// environment. Configuration is read once at startup; invalid configuration
// is a fatal error so misconfigured instances never start serving traffic.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Environment names accepted in APP_ENV.
const (
	EnvDevelopment = "development"
	EnvTest        = "test"
	EnvStaging     = "staging"
	EnvProduction  = "production"
)

// Log formats accepted in LOG_FORMAT.
const (
	LogFormatJSON = "json"
	LogFormatText = "text"
)

// Config is the complete, validated process configuration.
type Config struct {
	Env string

	HTTP     HTTPConfig
	Worker   WorkerConfig
	Shutdown ShutdownConfig
	Log      LogConfig
	Database DatabaseConfig
	Health   HealthConfig
}

// HTTPConfig configures the public API listener.
type HTTPConfig struct {
	Addr              string
	ReadHeaderTimeout time.Duration
	ReadTimeout       time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration
	// RequestTimeout bounds the context of every ordinary API request.
	RequestTimeout time.Duration
	MaxBodyBytes   int64
	// TrustedProxies lists the reverse proxies (OpenResty, the K3s ingress
	// network) whose X-Forwarded-For header is believed.
	TrustedProxies []netip.Prefix
}

// WorkerConfig configures the background worker process.
type WorkerConfig struct {
	// HTTPAddr serves the worker's health/readiness probes. It is never
	// exposed publicly.
	HTTPAddr string
}

// ShutdownConfig controls graceful termination.
type ShutdownConfig struct {
	// DrainDelay is how long the process keeps serving (while reporting
	// not-ready) after SIGTERM, giving load balancers time to stop routing.
	DrainDelay time.Duration
	// Timeout bounds how long in-flight work may take to finish.
	Timeout time.Duration
}

// LogConfig configures structured logging.
type LogConfig struct {
	Level  slog.Level
	Format string
}

// DatabaseConfig configures the PostgreSQL connection pool.
type DatabaseConfig struct {
	URL              string
	MaxConns         int32
	MinConns         int32
	MaxConnLifetime  time.Duration
	MaxConnIdleTime  time.Duration
	ConnectTimeout   time.Duration
	StartupTimeout   time.Duration
	StatementTimeout time.Duration
	// AutoMigrate applies pending migrations at startup. When false, the
	// process refuses to start against a schema with pending migrations.
	AutoMigrate bool
}

// HealthConfig configures readiness probing.
type HealthConfig struct {
	CheckTimeout time.Duration
	CacheTTL     time.Duration
}

// LookupFunc matches os.LookupEnv so tests can inject an environment.
type LookupFunc func(key string) (string, bool)

// Load reads configuration from lookup, applies defaults and validates the
// result. All problems are reported together.
func Load(lookup LookupFunc) (*Config, error) {
	p := parser{lookup: lookup}

	env := strings.ToLower(p.str("APP_ENV", EnvDevelopment))
	isLocal := env == EnvDevelopment || env == EnvTest

	defaultFormat := LogFormatJSON
	if env == EnvDevelopment {
		defaultFormat = LogFormatText
	}

	cfg := &Config{
		Env: env,
		HTTP: HTTPConfig{
			Addr:              p.str("HTTP_ADDR", ":8080"),
			ReadHeaderTimeout: p.duration("HTTP_READ_HEADER_TIMEOUT", 5*time.Second),
			ReadTimeout:       p.duration("HTTP_READ_TIMEOUT", 15*time.Second),
			WriteTimeout:      p.duration("HTTP_WRITE_TIMEOUT", 30*time.Second),
			IdleTimeout:       p.duration("HTTP_IDLE_TIMEOUT", 120*time.Second),
			RequestTimeout:    p.duration("HTTP_REQUEST_TIMEOUT", 20*time.Second),
			MaxBodyBytes:      p.int64("HTTP_MAX_BODY_BYTES", 1<<20),
			TrustedProxies:    p.prefixes("HTTP_TRUSTED_PROXIES"),
		},
		Worker: WorkerConfig{
			HTTPAddr: p.str("WORKER_HTTP_ADDR", ":8081"),
		},
		Shutdown: ShutdownConfig{
			DrainDelay: p.duration("SHUTDOWN_DRAIN_DELAY", 0),
			Timeout:    p.duration("SHUTDOWN_TIMEOUT", 25*time.Second),
		},
		Log: LogConfig{
			Level:  p.logLevel("LOG_LEVEL", slog.LevelInfo),
			Format: strings.ToLower(p.str("LOG_FORMAT", defaultFormat)),
		},
		Database: DatabaseConfig{
			URL:              p.str("DATABASE_URL", ""),
			MaxConns:         p.int32("DATABASE_MAX_CONNS", 20),
			MinConns:         p.int32("DATABASE_MIN_CONNS", 2),
			MaxConnLifetime:  p.duration("DATABASE_MAX_CONN_LIFETIME", time.Hour),
			MaxConnIdleTime:  p.duration("DATABASE_MAX_CONN_IDLE_TIME", 30*time.Minute),
			ConnectTimeout:   p.duration("DATABASE_CONNECT_TIMEOUT", 5*time.Second),
			StartupTimeout:   p.duration("DATABASE_STARTUP_TIMEOUT", 30*time.Second),
			StatementTimeout: p.duration("DATABASE_STATEMENT_TIMEOUT", 15*time.Second),
			AutoMigrate:      p.bool("DATABASE_AUTO_MIGRATE", isLocal),
		},
		Health: HealthConfig{
			CheckTimeout: p.duration("READINESS_CHECK_TIMEOUT", 2*time.Second),
			CacheTTL:     p.duration("READINESS_CACHE_TTL", time.Second),
		},
	}

	errs := p.errs
	errs = append(errs, cfg.validate()...)
	if len(errs) > 0 {
		return nil, fmt.Errorf("invalid configuration:\n%w", errors.Join(errs...))
	}
	return cfg, nil
}

// IsProduction reports whether the process runs in the production environment.
func (c *Config) IsProduction() bool { return c.Env == EnvProduction }

func (c *Config) validate() []error {
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	switch c.Env {
	case EnvDevelopment, EnvTest, EnvStaging, EnvProduction:
	default:
		add("APP_ENV: must be one of development, test, staging, production (got %q)", c.Env)
	}

	if err := validateAddr(c.HTTP.Addr); err != nil {
		add("HTTP_ADDR: %v", err)
	}
	if err := validateAddr(c.Worker.HTTPAddr); err != nil {
		add("WORKER_HTTP_ADDR: %v", err)
	}

	positive := []struct {
		name string
		d    time.Duration
	}{
		{"HTTP_READ_HEADER_TIMEOUT", c.HTTP.ReadHeaderTimeout},
		{"HTTP_READ_TIMEOUT", c.HTTP.ReadTimeout},
		{"HTTP_WRITE_TIMEOUT", c.HTTP.WriteTimeout},
		{"HTTP_IDLE_TIMEOUT", c.HTTP.IdleTimeout},
		{"HTTP_REQUEST_TIMEOUT", c.HTTP.RequestTimeout},
		{"SHUTDOWN_TIMEOUT", c.Shutdown.Timeout},
		{"DATABASE_MAX_CONN_LIFETIME", c.Database.MaxConnLifetime},
		{"DATABASE_MAX_CONN_IDLE_TIME", c.Database.MaxConnIdleTime},
		{"DATABASE_CONNECT_TIMEOUT", c.Database.ConnectTimeout},
		{"DATABASE_STARTUP_TIMEOUT", c.Database.StartupTimeout},
		{"DATABASE_STATEMENT_TIMEOUT", c.Database.StatementTimeout},
		{"READINESS_CHECK_TIMEOUT", c.Health.CheckTimeout},
	}
	for _, v := range positive {
		if v.d <= 0 {
			add("%s: must be greater than zero", v.name)
		}
	}
	if c.Shutdown.DrainDelay < 0 {
		add("SHUTDOWN_DRAIN_DELAY: must not be negative")
	}
	if c.Health.CacheTTL < 0 {
		add("READINESS_CACHE_TTL: must not be negative")
	}
	if c.HTTP.RequestTimeout > c.HTTP.WriteTimeout {
		add("HTTP_REQUEST_TIMEOUT (%s) must not exceed HTTP_WRITE_TIMEOUT (%s), or responses for slow requests are cut off",
			c.HTTP.RequestTimeout, c.HTTP.WriteTimeout)
	}
	if c.HTTP.MaxBodyBytes <= 0 {
		add("HTTP_MAX_BODY_BYTES: must be greater than zero")
	}

	switch c.Log.Format {
	case LogFormatJSON, LogFormatText:
	default:
		add("LOG_FORMAT: must be json or text (got %q)", c.Log.Format)
	}

	if c.Database.URL == "" {
		add("DATABASE_URL: required")
	} else if err := validateDatabaseURL(c.Database.URL); err != nil {
		add("DATABASE_URL: %v", err)
	}
	if c.Database.MaxConns < 1 {
		add("DATABASE_MAX_CONNS: must be at least 1")
	}
	if c.Database.MinConns < 0 {
		add("DATABASE_MIN_CONNS: must not be negative")
	}
	if c.Database.MinConns > c.Database.MaxConns {
		add("DATABASE_MIN_CONNS (%d) must not exceed DATABASE_MAX_CONNS (%d)", c.Database.MinConns, c.Database.MaxConns)
	}

	if c.IsProduction() && c.Log.Format != LogFormatJSON {
		add("LOG_FORMAT: must be json in production so logs remain machine-parseable")
	}
	return errs
}

func validateAddr(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("must be host:port (got %q)", addr)
	}
	if host != "" && net.ParseIP(host) == nil && host != "localhost" {
		return fmt.Errorf("host must be empty, localhost or an IP address (got %q)", host)
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 0 || n > 65535 {
		return fmt.Errorf("invalid port %q", port)
	}
	return nil
}

func validateDatabaseURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		// url.Parse errors echo the input, which contains the password.
		return errors.New("not a valid URL")
	}
	if u.Scheme != "postgres" && u.Scheme != "postgresql" {
		return fmt.Errorf("scheme must be postgres or postgresql (got %q)", u.Scheme)
	}
	if u.Host == "" {
		return errors.New("host is required")
	}
	if strings.Trim(u.Path, "/") == "" {
		return errors.New("database name is required")
	}
	return nil
}

// LogValue implements slog.LogValuer so the configuration can be logged at
// startup without leaking credentials.
func (c *Config) LogValue() slog.Value {
	proxies := make([]string, len(c.HTTP.TrustedProxies))
	for i, p := range c.HTTP.TrustedProxies {
		proxies[i] = p.String()
	}
	return slog.GroupValue(
		slog.String("env", c.Env),
		slog.String("http_addr", c.HTTP.Addr),
		slog.String("worker_http_addr", c.Worker.HTTPAddr),
		slog.Any("trusted_proxies", proxies),
		slog.String("log_level", c.Log.Level.String()),
		slog.String("log_format", c.Log.Format),
		slog.String("database", RedactURL(c.Database.URL)),
		slog.Int("database_max_conns", int(c.Database.MaxConns)),
		slog.Bool("database_auto_migrate", c.Database.AutoMigrate),
		slog.Duration("shutdown_drain_delay", c.Shutdown.DrainDelay),
		slog.Duration("shutdown_timeout", c.Shutdown.Timeout),
	)
}

// RedactURL removes the password and query string from a connection URL.
func RedactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "[unparseable]"
	}
	if _, ok := u.User.Password(); ok {
		u.User = url.UserPassword(u.User.Username(), "REDACTED")
	}
	u.RawQuery = ""
	return u.String()
}

// parser accumulates per-variable parse errors so they can be reported
// together with validation errors.
type parser struct {
	lookup LookupFunc
	errs   []error
}

func (p *parser) raw(key string) (string, bool) {
	v, ok := p.lookup(key)
	if !ok {
		return "", false
	}
	v = strings.TrimSpace(v)
	return v, v != ""
}

func (p *parser) fail(key, want, got string) {
	p.errs = append(p.errs, fmt.Errorf("%s: expected %s (got %q)", key, want, got))
}

func (p *parser) str(key, def string) string {
	if v, ok := p.raw(key); ok {
		return v
	}
	return def
}

func (p *parser) duration(key string, def time.Duration) time.Duration {
	v, ok := p.raw(key)
	if !ok {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		p.fail(key, "a duration such as 5s or 1m", v)
		return def
	}
	return d
}

func (p *parser) int64(key string, def int64) int64 {
	v, ok := p.raw(key)
	if !ok {
		return def
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		p.fail(key, "an integer", v)
		return def
	}
	return n
}

func (p *parser) int32(key string, def int32) int32 {
	v, ok := p.raw(key)
	if !ok {
		return def
	}
	n, err := strconv.ParseInt(v, 10, 32)
	if err != nil {
		p.fail(key, "a 32-bit integer", v)
		return def
	}
	return int32(n)
}

func (p *parser) bool(key string, def bool) bool {
	v, ok := p.raw(key)
	if !ok {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		p.fail(key, "true or false", v)
		return def
	}
	return b
}

func (p *parser) logLevel(key string, def slog.Level) slog.Level {
	v, ok := p.raw(key)
	if !ok {
		return def
	}
	var l slog.Level
	if err := l.UnmarshalText([]byte(v)); err != nil {
		p.fail(key, "debug, info, warn or error", v)
		return def
	}
	return l
}

func (p *parser) prefixes(key string) []netip.Prefix {
	v, ok := p.raw(key)
	if !ok {
		return nil
	}
	var out []netip.Prefix
	for _, item := range strings.Split(v, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		// Accept bare addresses as single-host prefixes.
		if !strings.Contains(item, "/") {
			addr, err := netip.ParseAddr(item)
			if err != nil {
				p.fail(key, "a comma-separated list of IPs or CIDRs", item)
				continue
			}
			out = append(out, netip.PrefixFrom(addr.Unmap(), addr.Unmap().BitLen()))
			continue
		}
		pfx, err := netip.ParsePrefix(item)
		if err != nil {
			p.fail(key, "a comma-separated list of IPs or CIDRs", item)
			continue
		}
		out = append(out, pfx.Masked())
	}
	return out
}
