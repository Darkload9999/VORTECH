package jobs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"os"
	"runtime/debug"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Darkload9999/VORTECH/backend/internal/database"
	"github.com/Darkload9999/VORTECH/backend/internal/database/db"
)

// Handler executes one job. Returning nil completes it; an error schedules
// a retry with backoff unless it is Permanent or attempts are exhausted.
type Handler func(ctx context.Context, j Job) error

// Config tunes a Runner. Zero values take the defaults below.
type Config struct {
	// WorkerID identifies this process in locked_by (default host-pid-rand).
	WorkerID string
	// Concurrency is the number of jobs run at once (default 4).
	Concurrency int
	// Lease is how long a claim is valid without a heartbeat (default 60s).
	Lease time.Duration
	// Poll is the fallback polling interval; enqueues wake workers
	// immediately through LISTEN/NOTIFY (default 5s).
	Poll time.Duration
	// Grace is how long in-flight jobs may finish after shutdown starts
	// before they are cancelled and released (default 20s).
	Grace time.Duration
	// BackoffBase and BackoffMax bound the exponential retry delay
	// (defaults 5s and 5m).
	BackoffBase, BackoffMax time.Duration
}

func (c *Config) defaults() {
	if c.WorkerID == "" {
		host, _ := os.Hostname()
		c.WorkerID = fmt.Sprintf("%s-%d-%04x", host, os.Getpid(), rand.IntN(0x10000))
	}
	if c.Concurrency <= 0 {
		c.Concurrency = 4
	}
	if c.Lease <= 0 {
		c.Lease = 60 * time.Second
	}
	if c.Poll <= 0 {
		c.Poll = 5 * time.Second
	}
	if c.Grace <= 0 {
		c.Grace = 20 * time.Second
	}
	if c.BackoffBase <= 0 {
		c.BackoffBase = 5 * time.Second
	}
	if c.BackoffMax <= 0 {
		c.BackoffMax = 5 * time.Minute
	}
}

type registration struct {
	h       Handler
	timeout time.Duration
}

// Runner claims and executes jobs.
type Runner struct {
	pool *pgxpool.Pool
	q    *db.Queries
	log  *slog.Logger
	cfg  Config

	handlers map[string]registration
	wake     chan struct{}
}

// NewRunner returns a Runner; register handlers before calling Run.
func NewRunner(pool *pgxpool.Pool, log *slog.Logger, cfg Config) *Runner {
	cfg.defaults()
	return &Runner{
		pool: pool, q: db.New(pool), log: log.With("worker_id", cfg.WorkerID), cfg: cfg,
		handlers: map[string]registration{}, wake: make(chan struct{}, 1),
	}
}

// WorkerID returns the identity this runner claims jobs with.
func (r *Runner) WorkerID() string { return r.cfg.WorkerID }

// Handle registers h for kind; each run is bounded by timeout.
func (r *Runner) Handle(kind string, timeout time.Duration, h Handler) {
	r.handlers[kind] = registration{h: h, timeout: timeout}
}

// Wake makes the runner look for work now (non-blocking).
func (r *Runner) Wake() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// Run processes jobs until ctx ends, then lets in-flight jobs finish
// within the grace period; jobs still running after it are cancelled and
// handed back to the queue.
func (r *Runner) Run(ctx context.Context) error {
	if len(r.handlers) == 0 {
		return errors.New("jobs: no handlers registered")
	}
	kinds := make([]string, 0, len(r.handlers))
	for k := range r.handlers {
		kinds = append(kinds, k)
	}

	listenCtx, stopListen := context.WithCancel(ctx)
	defer stopListen()
	go database.Listen(listenCtx, r.pool, r.log, []string{database.ChannelJobs}, r.Wake, func(*pgconn.Notification) { r.Wake() })

	// Handlers run on workCtx, which outlives ctx by the grace period.
	workCtx, cancelWork := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelWork()

	slots := make(chan struct{}, r.cfg.Concurrency)
	var wg sync.WaitGroup
	timer := time.NewTimer(0)
	defer timer.Stop()

	r.log.Info("job runner started", "kinds", kinds, "concurrency", r.cfg.Concurrency)
	for {
		select {
		case <-ctx.Done():
			return r.drain(&wg, cancelWork)
		case <-r.wake:
		case <-timer.C:
		}
		// Claim while there are free slots and runnable jobs.
		for {
			select {
			case slots <- struct{}{}:
			case <-ctx.Done():
				return r.drain(&wg, cancelWork)
			}
			job, ok, err := r.claim(ctx, kinds)
			if err != nil || !ok {
				<-slots
				if err != nil && ctx.Err() == nil {
					r.log.Error("claim job", "error", err)
				}
				break
			}
			wg.Add(1)
			go func() {
				defer func() { <-slots; wg.Done(); r.Wake() }()
				r.execute(workCtx, ctx, job)
			}()
		}
		timer.Reset(r.nextPoll(ctx, kinds))
	}
}

// nextPoll returns how long to sleep: until the next retry or lease expiry
// if that comes before the regular poll.
func (r *Runner) nextPoll(ctx context.Context, kinds []string) time.Duration {
	secs, err := r.q.SecondsUntilNextJob(ctx, kinds)
	if err != nil || secs < 0 {
		return r.cfg.Poll
	}
	// A small floor avoids spinning on a job that is due but held by a
	// concurrent claimer's row lock.
	return min(max(time.Duration(secs*float64(time.Second)), 10*time.Millisecond), r.cfg.Poll)
}

func (r *Runner) drain(wg *sync.WaitGroup, cancelWork context.CancelFunc) error {
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(r.cfg.Grace):
		r.log.Warn("grace period over; cancelling in-flight jobs")
		cancelWork()
		<-done
	}
	r.log.Info("job runner stopped")
	return nil
}

func (r *Runner) claim(ctx context.Context, kinds []string) (Job, bool, error) {
	row, err := r.q.ClaimJob(ctx, db.ClaimJobParams{Worker: r.cfg.WorkerID, LeaseSeconds: r.cfg.Lease.Seconds(), Kinds: kinds})
	if errors.Is(err, pgx.ErrNoRows) {
		return Job{}, false, nil
	}
	if err != nil {
		return Job{}, false, err
	}
	return Job{ID: row.ID, Kind: row.Kind, Payload: row.Payload, Attempt: row.Attempts, MaxAttempts: row.MaxAttempts}, true, nil
}

// execute runs one job with a lease heartbeat and records the outcome.
// stopping reports shutdown, so a job cancelled by it is released rather
// than counted as a failure.
func (r *Runner) execute(workCtx, stopping context.Context, j Job) {
	reg := r.handlers[j.Kind]
	log := r.log.With("job_id", j.ID, "kind", j.Kind, "attempt", j.Attempt)

	ctx, cancel := context.WithTimeout(workCtx, reg.timeout)
	defer cancel()
	lost := make(chan struct{})
	go r.heartbeat(ctx, j.ID, cancel, lost, log)

	start := time.Now()
	var err error
	if j.Attempt > j.MaxAttempts {
		// Reclaimed after its worker died on the final attempt.
		err = Permanent(errors.New("attempts exhausted (worker lost while running)"))
	} else {
		err = r.safeCall(ctx, reg.h, j)
	}
	cancel()

	// Bookkeeping must not be cut short by shutdown.
	bctx, bcancel := context.WithTimeout(context.WithoutCancel(workCtx), 10*time.Second)
	defer bcancel()
	select {
	case <-lost:
		log.Warn("job lease lost; outcome discarded", "error", err)
		return
	default:
	}

	switch {
	case err == nil:
		if _, cerr := r.q.CompleteJob(bctx, db.CompleteJobParams{ID: j.ID, Worker: r.cfg.WorkerID}); cerr != nil {
			log.Error("record job success", "error", cerr)
			return
		}
		log.Info("job succeeded", "duration", time.Since(start).Round(time.Millisecond))
	case stopping.Err() != nil && errors.Is(err, context.Canceled):
		if _, rerr := r.q.ReleaseJob(bctx, db.ReleaseJobParams{ID: j.ID, Worker: r.cfg.WorkerID}); rerr != nil {
			log.Error("release job", "error", rerr)
			return
		}
		log.Info("job released for another worker (shutdown)")
	default:
		permanent := IsPermanent(err)
		backoff := r.backoff(j.Attempt)
		state, ferr := r.q.FailJob(bctx, db.FailJobParams{
			ID: j.ID, Worker: r.cfg.WorkerID, Permanent: permanent,
			BackoffSeconds: backoff.Seconds(), LastError: truncate(err.Error(), 2000),
		})
		if ferr != nil {
			log.Error("record job failure", "error", ferr, "job_error", err)
			return
		}
		if state == "failed" {
			log.Error("job failed permanently", "error", err, "permanent", permanent)
		} else {
			log.Warn("job failed; will retry", "error", err, "retry_in", backoff.Round(time.Second))
		}
	}
}

// heartbeat extends the lease every third of its length. If the lease is
// lost (the job was reclaimed elsewhere) it cancels the handler.
func (r *Runner) heartbeat(ctx context.Context, id uuid.UUID, cancel context.CancelFunc, lost chan<- struct{}, log *slog.Logger) {
	t := time.NewTicker(r.cfg.Lease / 3)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		n, err := r.q.ExtendJobLease(ctx, db.ExtendJobLeaseParams{ID: id, Worker: r.cfg.WorkerID, LeaseSeconds: r.cfg.Lease.Seconds()})
		if err != nil {
			if ctx.Err() == nil {
				log.Warn("extend job lease", "error", err)
			}
			continue
		}
		if n == 0 {
			close(lost)
			cancel()
			return
		}
	}
}

func (r *Runner) safeCall(ctx context.Context, h Handler, j Job) (err error) {
	defer func() {
		if p := recover(); p != nil {
			r.log.Error("job handler panicked", "job_id", j.ID, "kind", j.Kind, "panic", p, "stack", string(debug.Stack()))
			err = fmt.Errorf("handler panicked: %v", p)
		}
	}()
	return h(ctx, j)
}

// backoff is exponential with full jitter in [d/2, d].
func (r *Runner) backoff(attempt int32) time.Duration {
	d := r.cfg.BackoffBase
	for i := int32(1); i < attempt && d < r.cfg.BackoffMax; i++ {
		d *= 2
	}
	d = min(d, r.cfg.BackoffMax)
	return d/2 + time.Duration(rand.Int64N(int64(d/2)+1))
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
