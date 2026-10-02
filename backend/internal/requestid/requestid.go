// Package requestid carries the per-request correlation ID through a context.
//
// It is intentionally tiny and dependency-free so that both the logging and
// HTTP layers can import it without creating cycles.
package requestid

import (
	"context"

	"github.com/google/uuid"
)

// Header is the HTTP header used to propagate request IDs. OpenResty sets it
// from $request_id; the API echoes it back on every response.
const Header = "X-Request-ID"

const maxLen = 128

type ctxKey struct{}

// New returns a fresh, time-ordered request ID (UUIDv7).
func New() string {
	id, err := uuid.NewV7()
	if err != nil {
		// NewV7 only fails if the system entropy source fails; fall back to v4,
		// which panics in the same circumstance and makes the failure explicit.
		return uuid.NewString()
	}
	return id.String()
}

// Valid reports whether an externally supplied ID is safe to adopt.
// IDs are written to logs and response headers, so only a conservative
// character set is accepted to rule out log/header injection.
func Valid(id string) bool {
	if id == "" || len(id) > maxLen {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '-', c == '_', c == '.', c == ':':
		default:
			return false
		}
	}
	return true
}

// With returns a copy of ctx carrying id.
func With(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, ctxKey{}, id)
}

// From returns the request ID stored in ctx, or "" if none is present.
func From(ctx context.Context) string {
	id, _ := ctx.Value(ctxKey{}).(string)
	return id
}
