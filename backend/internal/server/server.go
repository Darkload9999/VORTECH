// Package server owns the HTTP listener lifecycle: construction with safe
// timeouts, serving, and the drain-then-shutdown sequence run on SIGTERM.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/vortech/backend/internal/config"
)

// New returns an *http.Server with timeouts from cfg. Timeouts protect the
// process against slow-loris style clients even when OpenResty is bypassed.
func New(addr string, h http.Handler, cfg config.HTTPConfig, log *slog.Logger) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		ReadTimeout:       cfg.ReadTimeout,
		WriteTimeout:      cfg.WriteTimeout,
		IdleTimeout:       cfg.IdleTimeout,
		MaxHeaderBytes:    64 << 10,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
}

// ShutdownOptions controls the termination sequence.
type ShutdownOptions struct {
	// OnDrain runs as soon as shutdown begins, before the drain delay;
	// typically it flips readiness to failing.
	OnDrain func()
	// DrainDelay keeps the listener open while load balancers notice the
	// failing readiness probe and stop routing new requests here.
	DrainDelay time.Duration
	// Timeout bounds how long in-flight requests may take to complete.
	Timeout time.Duration
}

// Run serves on ln until ctx is cancelled, then performs a graceful
// shutdown. It returns nil after a clean shutdown, or an error if serving
// failed or in-flight requests did not finish within the timeout.
func Run(ctx context.Context, log *slog.Logger, srv *http.Server, ln net.Listener, opts ShutdownOptions) error {
	serveErr := make(chan error, 1)
	go func() {
		err := srv.Serve(ln)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		serveErr <- err
	}()
	log.Info("http server listening", "addr", ln.Addr().String())

	select {
	case err := <-serveErr:
		if err != nil {
			return fmt.Errorf("http server: %w", err)
		}
		return nil
	case <-ctx.Done():
	}

	log.Info("shutdown started: draining", "drain_delay", opts.DrainDelay, "timeout", opts.Timeout)
	if opts.OnDrain != nil {
		opts.OnDrain()
	}
	if opts.DrainDelay > 0 {
		time.Sleep(opts.DrainDelay)
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), opts.Timeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		// Force-close whatever is left so the process can exit.
		_ = srv.Close()
		return fmt.Errorf("graceful shutdown incomplete: %w", err)
	}
	if err := <-serveErr; err != nil {
		return fmt.Errorf("http server: %w", err)
	}
	log.Info("http server stopped")
	return nil
}

// Listen opens a TCP listener on addr.
func Listen(addr string) (net.Listener, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", addr, err)
	}
	return ln, nil
}
