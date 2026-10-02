// Package logging builds the process-wide structured logger.
//
// Every record logged with a context automatically carries the request ID,
// and attributes whose keys look like credentials are redacted before they
// reach the output.
package logging

import (
	"context"
	"io"
	"log/slog"
	"strings"

	"github.com/Darkload9999/VORTECH/backend/internal/buildinfo"
	"github.com/Darkload9999/VORTECH/backend/internal/config"
	"github.com/Darkload9999/VORTECH/backend/internal/requestid"
)

// Redacted replaces the value of any sensitive attribute.
const Redacted = "[REDACTED]"

// sensitiveKeyParts are matched case-insensitively against attribute keys.
var sensitiveKeyParts = []string{
	"password", "passwd", "secret", "token", "authorization",
	"cookie", "api_key", "apikey", "private_key", "credential",
	"database_url", "dsn",
}

// New returns a logger writing to w in the configured format and level.
// service identifies the binary (api, worker, migrate).
func New(w io.Writer, cfg config.LogConfig, service string) *slog.Logger {
	opts := &slog.HandlerOptions{
		Level:       cfg.Level,
		ReplaceAttr: redact,
	}
	var h slog.Handler
	if cfg.Format == config.LogFormatText {
		h = slog.NewTextHandler(w, opts)
	} else {
		h = slog.NewJSONHandler(w, opts)
	}
	return slog.New(&contextHandler{Handler: h}).With(
		slog.String("service", service),
		slog.String("version", buildinfo.Version),
	)
}

func redact(_ []string, a slog.Attr) slog.Attr {
	if isSensitive(a.Key) {
		return slog.String(a.Key, Redacted)
	}
	return a
}

func isSensitive(key string) bool {
	k := strings.ToLower(key)
	for _, part := range sensitiveKeyParts {
		if strings.Contains(k, part) {
			return true
		}
	}
	return false
}

// contextHandler enriches records with values carried by the context.
type contextHandler struct {
	slog.Handler
}

func (h *contextHandler) Handle(ctx context.Context, r slog.Record) error {
	if id := requestid.From(ctx); id != "" {
		r.AddAttrs(slog.String("request_id", id))
	}
	return h.Handler.Handle(ctx, r)
}

func (h *contextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &contextHandler{Handler: h.Handler.WithAttrs(attrs)}
}

func (h *contextHandler) WithGroup(name string) slog.Handler {
	return &contextHandler{Handler: h.Handler.WithGroup(name)}
}

// Discard returns a logger that drops everything; useful in tests.
func Discard() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}
