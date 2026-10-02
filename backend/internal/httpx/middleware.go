package httpx

import (
	"context"
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/Darkload9999/VORTECH/backend/internal/requestid"
)

// Middleware decorates an http.Handler.
type Middleware func(http.Handler) http.Handler

// Chain applies middleware so that the first argument is the outermost.
func Chain(h http.Handler, mws ...Middleware) http.Handler {
	for i := len(mws) - 1; i >= 0; i-- {
		h = mws[i](h)
	}
	return h
}

// RequestID adopts a well-formed incoming X-Request-ID (set by OpenResty) or
// generates a new one, stores it in the context and echoes it on the response.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(requestid.Header)
		if !requestid.Valid(id) {
			id = requestid.New()
		}
		w.Header().Set(requestid.Header, id)
		next.ServeHTTP(w, r.WithContext(requestid.With(r.Context(), id)))
	})
}

// Recover converts panics into opaque 500 responses. The stack trace is
// logged, never returned to the client.
func Recover(log *slog.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			sw, ok := w.(*statusWriter)
			if !ok {
				sw = &statusWriter{ResponseWriter: w}
			}
			defer func() {
				rec := recover()
				if rec == nil {
					return
				}
				if rec == http.ErrAbortHandler {
					// Deliberate abort: let net/http handle it silently.
					panic(rec)
				}
				log.ErrorContext(r.Context(), "panic recovered",
					"panic", rec, "method", r.Method, "path", r.URL.Path, "stack", string(debug.Stack()))
				if !sw.wroteHeader {
					WriteError(sw, r, http.StatusInternalServerError, CodeInternal, "An internal error occurred.")
				}
			}()
			next.ServeHTTP(sw, r)
		})
	}
}

// AccessLog logs one line per request. Requests to quietPaths (health
// probes) are logged at debug level when successful and at most warn level
// when failing, to keep logs useful.
func AccessLog(log *slog.Logger, quietPaths ...string) Middleware {
	quiet := make(map[string]struct{}, len(quietPaths))
	for _, p := range quietPaths {
		quiet[p] = struct{}{}
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			sw := &statusWriter{ResponseWriter: w}
			next.ServeHTTP(sw, r)

			status := sw.status
			if status == 0 {
				status = http.StatusOK
			}
			_, isQuiet := quiet[r.URL.Path]
			level := slog.LevelInfo
			switch {
			case status >= 500 && !isQuiet:
				level = slog.LevelError
			case status >= 400:
				// A failing probe (dependency down, draining) is an expected
				// operational state, already explained by its own log line.
				level = slog.LevelWarn
			case isQuiet:
				level = slog.LevelDebug
			}
			// The query string is deliberately omitted: it may carry
			// one-time tokens (e.g. WebSocket tickets) in later phases.
			log.LogAttrs(r.Context(), level, "http request",
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Int("status", status),
				slog.Int64("bytes", sw.bytes),
				slog.Float64("duration_ms", float64(time.Since(start).Microseconds())/1000),
				slog.String("client_ip", ClientIP(r.Context()).String()),
				slog.String("user_agent", r.UserAgent()),
			)
		})
	}
}

// MaxBodyBytes caps the size of request bodies.
func MaxBodyBytes(limit int64) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.ContentLength > limit {
				WriteError(w, r, http.StatusRequestEntityTooLarge, CodePayloadTooLarge, "Request body is too large.")
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, limit)
			next.ServeHTTP(w, r)
		})
	}
}

// Timeout bounds the request context so that database queries and outbound
// calls made on behalf of the request are cancelled when it expires.
// Long-lived routes (WebSockets, terminals) must be mounted outside it.
func Timeout(d time.Duration) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithTimeout(r.Context(), d)
			defer cancel()
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// statusWriter records the status code and body size. Unwrap lets
// http.ResponseController reach the underlying writer for Flush/Hijack.
type statusWriter struct {
	http.ResponseWriter
	status      int
	bytes       int64
	wroteHeader bool
}

func (w *statusWriter) WriteHeader(code int) {
	if !w.wroteHeader {
		w.status = code
		w.wroteHeader = true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += int64(n)
	return n, err
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
