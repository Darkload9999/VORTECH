package health

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func discard() *slog.Logger { return slog.New(slog.DiscardHandler) }

func ok(context.Context) error { return nil }

func TestLiveAlwaysOK(t *testing.T) {
	failing := Check{Name: "db", Fn: func(context.Context) error { return errors.New("down") }}
	s := New(discard(), time.Second, 0, failing)
	s.SetDraining()

	rec := httptest.NewRecorder()
	s.Live(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("liveness must not depend on dependencies or draining, got %d", rec.Code)
	}
}

func TestReadyAllUp(t *testing.T) {
	s := New(discard(), time.Second, 0, Check{Name: "postgres", Fn: ok}, Check{Name: "other", Fn: ok})
	rec := httptest.NewRecorder()
	s.Ready(rec, httptest.NewRequest(http.MethodGet, "/ready", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Data Report `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Data.Status != "ready" || body.Data.Checks["postgres"] != StatusUp || body.Data.Checks["other"] != StatusUp {
		t.Fatalf("unexpected report %+v", body.Data)
	}
}

func TestReadyDependencyDownHidesErrorDetails(t *testing.T) {
	s := New(discard(), time.Second, 0,
		Check{Name: "postgres", Fn: func(context.Context) error {
			return errors.New("dial tcp 10.0.0.5:5432: password authentication failed for user vortech")
		}},
		Check{Name: "other", Fn: ok},
	)
	rec := httptest.NewRecorder()
	s.Ready(rec, httptest.NewRequest(http.MethodGet, "/ready", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d", rec.Code)
	}
	b := rec.Body.String()
	if !strings.Contains(b, CodeNotReady) || !strings.Contains(b, `"postgres":"down"`) || !strings.Contains(b, `"other":"up"`) {
		t.Fatalf("unexpected body %s", b)
	}
	if strings.Contains(b, "10.0.0.5") || strings.Contains(b, "password") {
		t.Fatalf("dependency error details leaked: %s", b)
	}
}

func TestReadyDraining(t *testing.T) {
	s := New(discard(), time.Second, 0, Check{Name: "postgres", Fn: ok})
	s.SetDraining()
	rec := httptest.NewRecorder()
	s.Ready(rec, httptest.NewRequest(http.MethodGet, "/ready", nil))
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), CodeDraining) {
		t.Fatalf("got %d %s", rec.Code, rec.Body.String())
	}
}

func TestCheckTimeout(t *testing.T) {
	slow := Check{Name: "slow", Fn: func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}}
	s := New(discard(), 20*time.Millisecond, 0, slow)

	start := time.Now()
	rep := s.Evaluate(context.Background())
	if rep.Ready || rep.Checks["slow"] != StatusDown {
		t.Fatalf("slow check should be reported down: %+v", rep)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("check timeout not enforced, took %s", elapsed)
	}
}

func TestChecksRunConcurrently(t *testing.T) {
	sleepy := func(context.Context) error { time.Sleep(50 * time.Millisecond); return nil }
	s := New(discard(), time.Second, 0,
		Check{Name: "a", Fn: sleepy}, Check{Name: "b", Fn: sleepy}, Check{Name: "c", Fn: sleepy})

	start := time.Now()
	if rep := s.Evaluate(context.Background()); !rep.Ready {
		t.Fatalf("expected ready: %+v", rep)
	}
	if elapsed := time.Since(start); elapsed > 140*time.Millisecond {
		t.Fatalf("checks appear sequential, took %s", elapsed)
	}
}

func TestResultCaching(t *testing.T) {
	var calls atomic.Int32
	counting := Check{Name: "db", Fn: func(context.Context) error { calls.Add(1); return nil }}
	s := New(discard(), time.Second, time.Minute, counting)

	now := time.Unix(1_000, 0)
	s.now = func() time.Time { return now }

	for range 5 {
		s.Evaluate(context.Background())
	}
	if calls.Load() != 1 {
		t.Fatalf("expected 1 call within TTL, got %d", calls.Load())
	}
	now = now.Add(2 * time.Minute)
	s.Evaluate(context.Background())
	if calls.Load() != 2 {
		t.Fatalf("expected re-evaluation after TTL, got %d calls", calls.Load())
	}
}

func TestCancelledRequestNotCached(t *testing.T) {
	var calls atomic.Int32
	c := Check{Name: "db", Fn: func(ctx context.Context) error { calls.Add(1); return ctx.Err() }}
	s := New(discard(), time.Second, time.Minute, c)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if rep := s.Evaluate(ctx); rep.Ready {
		t.Fatal("cancelled evaluation should not be ready")
	}
	if rep := s.Evaluate(context.Background()); !rep.Ready {
		t.Fatal("result from a cancelled request must not be cached")
	}
	if calls.Load() != 2 {
		t.Fatalf("calls = %d, want 2", calls.Load())
	}
}
