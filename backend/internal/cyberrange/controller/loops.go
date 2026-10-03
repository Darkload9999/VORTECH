package controller

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Darkload9999/VORTECH/backend/internal/audit"
	"github.com/Darkload9999/VORTECH/backend/internal/cyberrange"
	"github.com/Darkload9999/VORTECH/backend/internal/database/db"
	"github.com/Darkload9999/VORTECH/backend/internal/jobs"
)

// Intervals schedules the periodic loops.
type Intervals struct {
	Admit, Expiry, Reconcile time.Duration
}

// RunLoops runs admission (a safety net behind the admit job), expiry and
// reconciliation until ctx ends.
func (c *Controller) RunLoops(ctx context.Context, iv Intervals) {
	loop := func(name string, every time.Duration, fn func(context.Context) error) {
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			if err := fn(ctx); err != nil && ctx.Err() == nil {
				c.log.Error("range loop failed", "loop", name, "error", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}
	done := make(chan struct{}, 3)
	for _, l := range []struct {
		name  string
		every time.Duration
		fn    func(context.Context) error
	}{
		{"admit", iv.Admit, func(ctx context.Context) error { _, err := c.Admit(ctx); return err }},
		{"expiry", iv.Expiry, func(ctx context.Context) error { _, err := c.ExpireDue(ctx); return err }},
		{"reconcile", iv.Reconcile, func(ctx context.Context) error { _, err := c.Reconcile(ctx); return err }},
	} {
		go func() { loop(l.name, l.every, l.fn); done <- struct{}{} }()
	}
	for range 3 {
		<-done
	}
}

// ExpireDue moves ranges past their TTL to EXPIRED, then STOPPING, and
// queues their teardown.
func (c *Controller) ExpireDue(ctx context.Context) (int, error) {
	ids, err := c.q.ListExpiredRanges(ctx, 50)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, id := range ids {
		err := pgx.BeginFunc(ctx, c.pool, func(tx pgx.Tx) error {
			q := db.New(tx)
			row, err := q.GetRangeView(ctx, id)
			if err != nil {
				return err
			}
			cur := cyberrange.State(row.Range.State)
			if (cur != cyberrange.Ready && cur != cyberrange.Active) || row.Range.ExpiresAt == nil || row.Range.ExpiresAt.After(c.now()) {
				return nil
			}
			if _, err := cyberrange.Transition(ctx, tx, id, cur, cyberrange.Expired, "time limit reached", cyberrange.ActorWorker); err != nil {
				return err
			}
			if _, err := cyberrange.Transition(ctx, tx, id, cyberrange.Expired, cyberrange.Stopping, "cleanup after expiry", cyberrange.ActorWorker); err != nil {
				return err
			}
			if _, err := jobs.Enqueue(ctx, q, tx, cyberrange.JobDestroy, cyberrange.RangeJob{RangeID: id}, jobs.Options{}); err != nil {
				return err
			}
			n++
			return nil
		})
		if err != nil && !errors.Is(err, cyberrange.ErrStateChanged) {
			return n, err
		}
	}
	if n > 0 {
		c.log.Info("ranges expired", "count", n)
	}
	return n, nil
}

// Report summarises one reconciliation pass.
type Report struct {
	Orphans       int // namespaces without a range, deleted or quarantined
	Vanished      int // live ranges whose namespace disappeared (now FAILED)
	Redriven      int // stuck ranges whose job was re-queued or that were failed
	PrunedJobs    int64
	ClusterRanges int
}

// Reconcile compares the cluster with the database and repairs drift:
//
//   - a managed namespace with no range that should have one (deleted,
//     destroyed or unknown) is an orphan: delete or quarantine it;
//   - a starting/ready/active range without its namespace is failed and
//     cleaned up;
//   - a range stuck in a state that needs a job, with no job pending, is
//     re-driven.
//
// The cluster is listed before the database is read, so a namespace in
// the snapshot always belongs to a range already visible in the database
// (provisioning creates the namespace only after the range is committed).
func (c *Controller) Reconcile(ctx context.Context) (Report, error) {
	var rep Report
	namespaces, err := c.cluster.ListManaged(ctx)
	if err != nil {
		return rep, err
	}
	rep.ClusterRanges = len(namespaces)
	rows, err := c.q.ListRangesWithNamespaces(ctx)
	if err != nil {
		return rep, err
	}
	expected := make(map[string]db.ListRangesWithNamespacesRow, len(rows))
	for _, r := range rows {
		expected[r.Namespace] = r
	}

	present := make(map[string]bool, len(namespaces))
	for _, ns := range namespaces {
		present[ns.Name] = true
		if _, ok := expected[ns.Name]; ok || ns.Terminating || ns.Quarantined {
			continue
		}
		if err := c.handleOrphan(ctx, ns.Name, ns.RangeID); err != nil {
			c.log.Error("handle orphan namespace", "namespace", ns.Name, "error", err)
			continue
		}
		rep.Orphans++
	}

	for _, r := range rows {
		st := cyberrange.State(r.State)
		if present[r.Namespace] || (st != cyberrange.Starting && st != cyberrange.Ready && st != cyberrange.Active) {
			continue
		}
		if err := c.failByID(ctx, r.ID, "the range's environment disappeared"); err != nil {
			c.log.Error("fail vanished range", "range_id", r.ID, "error", err)
			continue
		}
		rep.Vanished++
	}

	stuck, err := c.q.ListStuckRanges(ctx, db.ListStuckRangesParams{StaleSeconds: c.cfg.StaleAfter.Seconds(), MaxRows: 50})
	if err != nil {
		return rep, err
	}
	for _, s := range stuck {
		var err error
		switch cyberrange.State(s.State) {
		case cyberrange.Provisioning, cyberrange.Starting:
			err = c.failByID(ctx, s.ID, "provisioning stalled")
		default: // FAILED, EXPIRED, STOPPING: teardown must happen
			err = pgx.BeginFunc(ctx, c.pool, func(tx pgx.Tx) error {
				_, err := jobs.Enqueue(ctx, db.New(tx), tx, cyberrange.JobDestroy, cyberrange.RangeJob{RangeID: s.ID}, jobs.Options{})
				return err
			})
		}
		if err != nil {
			c.log.Error("re-drive stuck range", "range_id", s.ID, "state", s.State, "error", err)
			continue
		}
		rep.Redriven++
	}

	// Keep a week of successful jobs and a month of failures for operators.
	if rep.PrunedJobs, err = c.q.PruneFinishedJobs(ctx, db.PruneFinishedJobsParams{
		SucceededAfterSeconds: (7 * 24 * time.Hour).Seconds(), FailedAfterSeconds: (30 * 24 * time.Hour).Seconds(),
	}); err != nil {
		return rep, err
	}
	if rep.Orphans+rep.Vanished+rep.Redriven > 0 {
		c.log.Warn("reconciliation repaired drift", "orphans", rep.Orphans, "vanished", rep.Vanished, "redriven", rep.Redriven)
	}
	return rep, nil
}

func (c *Controller) handleOrphan(ctx context.Context, ns, rangeID string) error {
	action := "range.orphan_deleted"
	if c.cfg.OrphanPolicy == OrphanQuarantine {
		action = "range.orphan_quarantined"
		if err := c.cluster.Quarantine(ctx, ns); err != nil {
			return err
		}
	} else if err := c.cluster.Delete(ctx, ns); err != nil {
		return err
	}
	c.log.Warn("orphan range namespace handled", "namespace", ns, "policy", c.cfg.OrphanPolicy)
	return audit.Insert(ctx, c.q, audit.Entry{
		ActorType: audit.ActorService, ActorID: cyberrange.ActorWorker,
		Action: action, ResourceType: "namespace", ResourceID: ns, Result: audit.ResultSuccess,
		Metadata: map[string]any{"range_id": rangeID},
	})
}

func (c *Controller) failByID(ctx context.Context, id uuid.UUID, reason string) error {
	err := pgx.BeginFunc(ctx, c.pool, func(tx pgx.Tx) error {
		row, err := db.New(tx).GetRangeView(ctx, id)
		if err != nil {
			return err
		}
		return c.fail(ctx, tx, id, cyberrange.State(row.Range.State), reason)
	})
	if errors.Is(err, cyberrange.ErrStateChanged) {
		return nil
	}
	return err
}
