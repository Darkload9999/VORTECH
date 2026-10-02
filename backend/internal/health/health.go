// Package health implements liveness and readiness probes.
//
// Liveness answers "is the process able to serve at all?" and never touches
// dependencies, so a database outage cannot cause Kubernetes to restart
// healthy pods. Readiness answers "should traffic be routed here?" and
// checks the dependencies the process cannot work without.
package health

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Darkload9999/VORTECH/backend/internal/httpx"
)

// Readiness error codes.
const (
	CodeNotReady = "SERVICE_NOT_READY"
	CodeDraining = "SERVICE_DRAINING"
)

// Check is a named readiness dependency check. Fn must honour ctx.
type Check struct {
	Name string
	Fn   func(ctx context.Context) error
}

// Status values reported per check.
const (
	StatusUp   = "up"
	StatusDown = "down"
)

// Report is the outcome of a readiness evaluation.
type Report struct {
	Ready  bool              `json:"-"`
	Status string            `json:"status"`
	Checks map[string]string `json:"checks"`
}

// Service evaluates readiness and tracks the draining state.
type Service struct {
	log      *slog.Logger
	checks   []Check
	timeout  time.Duration
	cacheTTL time.Duration
	now      func() time.Time

	draining atomic.Bool

	mu       sync.Mutex
	cached   Report
	cachedAt time.Time
}

// New returns a Service. timeout bounds each check; results are cached for
// cacheTTL so that frequent probes and accidental public exposure cannot
// turn into a load source for the database.
func New(log *slog.Logger, timeout, cacheTTL time.Duration, checks ...Check) *Service {
	return &Service{
		log:      log,
		checks:   checks,
		timeout:  timeout,
		cacheTTL: cacheTTL,
		now:      time.Now,
	}
}

// SetDraining marks the instance as shutting down; readiness fails from now
// on so that load balancers stop routing new requests here.
func (s *Service) SetDraining() { s.draining.Store(true) }

// Draining reports whether SetDraining has been called.
func (s *Service) Draining() bool { return s.draining.Load() }

// Evaluate runs all checks concurrently, reusing a recent result if fresh.
func (s *Service) Evaluate(ctx context.Context) Report {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.cachedAt.IsZero() && s.now().Sub(s.cachedAt) < s.cacheTTL {
		return s.cached
	}

	type result struct {
		name string
		err  error
	}
	results := make(chan result, len(s.checks))
	for _, c := range s.checks {
		go func() {
			cctx, cancel := context.WithTimeout(ctx, s.timeout)
			defer cancel()
			results <- result{name: c.Name, err: c.Fn(cctx)}
		}()
	}

	rep := Report{Ready: true, Status: "ready", Checks: make(map[string]string, len(s.checks))}
	for range s.checks {
		r := <-results
		if r.err != nil {
			rep.Ready = false
			rep.Checks[r.name] = StatusDown
			// Failure details go to the log only; probe responses may be
			// reachable by untrusted clients.
			s.log.WarnContext(ctx, "readiness check failed", "check", r.name, "error", r.err)
			continue
		}
		rep.Checks[r.name] = StatusUp
	}
	if !rep.Ready {
		rep.Status = "not_ready"
	}

	// Don't cache results computed under a cancelled request context: they
	// reflect the caller going away, not the dependency.
	if ctx.Err() == nil {
		s.cached, s.cachedAt = rep, s.now()
	}
	return rep
}

// Live handles liveness probes.
func (s *Service) Live(w http.ResponseWriter, _ *http.Request) {
	httpx.JSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// Ready handles readiness probes.
func (s *Service) Ready(w http.ResponseWriter, r *http.Request) {
	if s.Draining() {
		httpx.WriteAPIError(w, r, &httpx.Error{
			Status:  http.StatusServiceUnavailable,
			Code:    CodeDraining,
			Message: "Instance is shutting down.",
		})
		return
	}
	rep := s.Evaluate(r.Context())
	if !rep.Ready {
		httpx.WriteAPIError(w, r, &httpx.Error{
			Status:  http.StatusServiceUnavailable,
			Code:    CodeNotReady,
			Message: "One or more required dependencies are unavailable.",
			Details: map[string]any{"checks": rep.Checks},
		})
		return
	}
	httpx.JSON(w, http.StatusOK, rep)
}
