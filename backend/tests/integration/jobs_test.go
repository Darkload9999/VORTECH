//go:build integration

package integration

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Darkload9999/VORTECH/backend/internal/database/db"
	"github.com/Darkload9999/VORTECH/backend/internal/jobs"
)

// uniqueKind isolates each test's jobs in the shared test database.
func uniqueKind(t *testing.T) string {
	t.Helper()
	return "test." + strings.NewReplacer("-", "", "0", "a", "1", "b", "2", "c", "3", "d", "4", "e", "5", "f", "6", "g", "7", "h", "8", "i", "9", "j").
		Replace(uuid.NewString()[:12])
}

func enqueue(t *testing.T, pool *pgxpool.Pool, kind string, payload any, opts jobs.Options) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
		var err error
		id, err = jobs.Enqueue(context.Background(), db.New(tx), tx, kind, payload, opts)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

type jobRow struct {
	State     string
	Attempts  int32
	LastError *string
}

func jobState(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) jobRow {
	t.Helper()
	var r jobRow
	if err := pool.QueryRow(context.Background(), "SELECT state, attempts, last_error FROM jobs WHERE id = $1", id).Scan(&r.State, &r.Attempts, &r.LastError); err != nil {
		t.Fatal(err)
	}
	return r
}

func waitJob(t *testing.T, pool *pgxpool.Pool, id uuid.UUID, state string) jobRow {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		r := jobState(t, pool, id)
		if r.State == state {
			return r
		}
		if time.Now().After(deadline) {
			t.Fatalf("job %s: state %s, want %s (%+v)", id, r.State, state, r)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func startRunner(t *testing.T, r *jobs.Runner) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
}

func TestJobsRunRetryAndPermanentFailure(t *testing.T) {
	pool := migratedPool(t)
	kind := uniqueKind(t)
	var calls atomic.Int32
	r := jobs.NewRunner(pool, testLogger(), jobs.Config{Poll: time.Hour, BackoffBase: 10 * time.Millisecond, BackoffMax: 20 * time.Millisecond})
	r.Handle(kind, 5*time.Second, func(_ context.Context, j jobs.Job) error {
		var p struct{ Mode string }
		if err := j.Decode(&p); err != nil {
			return err
		}
		calls.Add(1)
		switch {
		case p.Mode == "flaky" && j.Attempt < 3:
			return errors.New("transient")
		case p.Mode == "fatal":
			return jobs.Permanent(errors.New("bad input"))
		case p.Mode == "panic":
			panic("boom")
		}
		return nil
	})
	startRunner(t, r)

	// Poll is an hour: these complete only because NOTIFY wakes the runner.
	ok := enqueue(t, pool, kind, map[string]string{"mode": "ok"}, jobs.Options{})
	flaky := enqueue(t, pool, kind, map[string]string{"mode": "flaky"}, jobs.Options{})
	fatal := enqueue(t, pool, kind, map[string]string{"mode": "fatal"}, jobs.Options{})
	panics := enqueue(t, pool, kind, map[string]string{"mode": "panic"}, jobs.Options{MaxAttempts: 2})

	if r := waitJob(t, pool, ok, "succeeded"); r.Attempts != 1 {
		t.Errorf("ok attempts = %d", r.Attempts)
	}
	if r := waitJob(t, pool, flaky, "succeeded"); r.Attempts != 3 {
		t.Errorf("flaky attempts = %d, want 3", r.Attempts)
	}
	if r := waitJob(t, pool, fatal, "failed"); r.Attempts != 1 || r.LastError == nil || *r.LastError != "bad input" {
		t.Errorf("permanent failure must not retry: %+v", r)
	}
	if r := waitJob(t, pool, panics, "failed"); r.Attempts != 2 || !strings.Contains(*r.LastError, "panicked") {
		t.Errorf("panicking job: %+v", r)
	}
}

func TestJobsEnqueueIsTransactional(t *testing.T) {
	pool := migratedPool(t)
	kind := uniqueKind(t)
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := jobs.Enqueue(ctx, db.New(tx), tx, kind, map[string]any{}, jobs.Options{}); err != nil {
		t.Fatal(err)
	}
	_ = tx.Rollback(ctx)
	if n := countRows(t, pool, "SELECT count(*) FROM jobs WHERE kind = $1", kind); n != 0 {
		t.Fatalf("rolled-back enqueue left %d job(s)", n)
	}
}

func TestJobsClaimIsExclusiveUnderConcurrency(t *testing.T) {
	pool := migratedPool(t)
	kind := uniqueKind(t)
	const n = 40
	for range n {
		enqueue(t, pool, kind, map[string]any{}, jobs.Options{})
	}
	var mu sync.Mutex
	seen := map[uuid.UUID]int{}
	handler := func(_ context.Context, j jobs.Job) error {
		mu.Lock()
		seen[j.ID]++
		mu.Unlock()
		time.Sleep(5 * time.Millisecond)
		return nil
	}
	for range 3 {
		r := jobs.NewRunner(pool, testLogger(), jobs.Config{Concurrency: 4, Poll: 50 * time.Millisecond})
		r.Handle(kind, 5*time.Second, handler)
		startRunner(t, r)
	}
	deadline := time.Now().Add(15 * time.Second)
	for countRows(t, pool, "SELECT count(*) FROM jobs WHERE kind = $1 AND state = 'succeeded'", kind) != n {
		if time.Now().After(deadline) {
			t.Fatal("jobs not all completed")
		}
		time.Sleep(20 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	for id, c := range seen {
		if c != 1 {
			t.Errorf("job %s ran %d times", id, c)
		}
	}
	if len(seen) != n {
		t.Errorf("ran %d distinct jobs, want %d", len(seen), n)
	}
}

func TestJobsExpiredLeaseIsReclaimed(t *testing.T) {
	pool := migratedPool(t)
	kind := uniqueKind(t)
	id := enqueue(t, pool, kind, map[string]any{}, jobs.Options{})

	// A worker claims the job and dies without completing it.
	q := db.New(pool)
	if _, err := q.ClaimJob(context.Background(), db.ClaimJobParams{Worker: "dead-worker", LeaseSeconds: 0.2, Kinds: []string{kind}}); err != nil {
		t.Fatal(err)
	}
	// The dead worker can no longer record an outcome once taken over.
	r := jobs.NewRunner(pool, testLogger(), jobs.Config{Poll: 50 * time.Millisecond})
	var ran atomic.Bool
	r.Handle(kind, 5*time.Second, func(context.Context, jobs.Job) error { ran.Store(true); return nil })
	startRunner(t, r)

	row := waitJob(t, pool, id, "succeeded")
	if !ran.Load() || row.Attempts != 2 {
		t.Fatalf("reclaimed job: ran=%v %+v", ran.Load(), row)
	}
	if n, _ := q.CompleteJob(context.Background(), db.CompleteJobParams{ID: id, Worker: "dead-worker"}); n != 0 {
		t.Fatal("a worker that lost its lease must not complete the job")
	}
}

func TestJobsShutdownReleasesInFlightJobs(t *testing.T) {
	pool := migratedPool(t)
	kind := uniqueKind(t)
	started := make(chan struct{})
	r := jobs.NewRunner(pool, testLogger(), jobs.Config{Poll: 50 * time.Millisecond, Grace: 50 * time.Millisecond})
	r.Handle(kind, time.Minute, func(ctx context.Context, _ jobs.Job) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()

	id := enqueue(t, pool, kind, map[string]any{}, jobs.Options{})
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("job never started")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if row := jobState(t, pool, id); row.State != "queued" || row.Attempts != 0 {
		t.Fatalf("interrupted job must be released without using an attempt: %+v", row)
	}
}
