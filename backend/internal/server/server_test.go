package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vortech/backend/internal/config"
)

func testHTTPConfig() config.HTTPConfig {
	return config.HTTPConfig{
		ReadHeaderTimeout: time.Second,
		ReadTimeout:       5 * time.Second,
		WriteTimeout:      5 * time.Second,
		IdleTimeout:       5 * time.Second,
	}
}

func TestGracefulShutdownCompletesInFlightRequests(t *testing.T) {
	log := slog.New(slog.DiscardHandler)
	started := make(chan struct{})
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		time.Sleep(200 * time.Millisecond)
		_, _ = io.WriteString(w, "done")
	})

	ln, err := Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := New(ln.Addr().String(), h, testHTTPConfig(), log)

	ctx, cancel := context.WithCancel(context.Background())
	var drained atomic.Bool
	runErr := make(chan error, 1)
	go func() {
		runErr <- Run(ctx, log, srv, ln, ShutdownOptions{
			OnDrain: func() { drained.Store(true) },
			Timeout: 5 * time.Second,
		})
	}()

	type result struct {
		body string
		err  error
	}
	resCh := make(chan result, 1)
	go func() {
		resp, err := http.Get("http://" + ln.Addr().String() + "/")
		if err != nil {
			resCh <- result{err: err}
			return
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		resCh <- result{body: string(b), err: err}
	}()

	<-started
	cancel() // simulate SIGTERM while the request is in flight

	res := <-resCh
	if res.err != nil || res.body != "done" {
		t.Fatalf("in-flight request should complete during shutdown: body=%q err=%v", res.body, res.err)
	}
	if err := <-runErr; err != nil {
		t.Fatalf("Run() returned error: %v", err)
	}
	if !drained.Load() {
		t.Fatal("OnDrain was not invoked")
	}

	// New connections must be refused after shutdown.
	if _, err := http.Get("http://" + ln.Addr().String() + "/"); err == nil {
		t.Fatal("expected connection error after shutdown")
	}
}

func TestShutdownTimeoutReportsError(t *testing.T) {
	log := slog.New(slog.DiscardHandler)
	started := make(chan struct{})
	release := make(chan struct{})
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
	})
	defer close(release)

	ln, err := Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := New(ln.Addr().String(), h, testHTTPConfig(), log)
	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() {
		runErr <- Run(ctx, log, srv, ln, ShutdownOptions{Timeout: 50 * time.Millisecond})
	}()

	go func() {
		resp, err := http.Get("http://" + ln.Addr().String() + "/")
		if err == nil {
			resp.Body.Close()
		}
	}()
	<-started
	cancel()

	err = <-runErr
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline exceeded, got %v", err)
	}
}

func TestListenError(t *testing.T) {
	ln, err := Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if _, err := Listen(ln.Addr().String()); err == nil {
		t.Fatal("expected error when port is already in use")
	}
}
