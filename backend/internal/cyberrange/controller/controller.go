// Package controller drives ranges from intent to reality. It runs in the
// worker: job handlers for admission, provisioning and teardown, plus
// periodic loops for expiry and reconciliation against the cluster.
//
// The database is the source of truth for what should exist; Kubernetes is
// made to match it. Every state change is a compare-and-set transition, so
// the API, several workers and the loops can act concurrently without
// overwriting each other, and every handler is idempotent because jobs are
// delivered at least once.
package controller

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Darkload9999/VORTECH/backend/internal/audit"
	"github.com/Darkload9999/VORTECH/backend/internal/cyberrange"
	"github.com/Darkload9999/VORTECH/backend/internal/cyberrange/kube"
	"github.com/Darkload9999/VORTECH/backend/internal/database/db"
	"github.com/Darkload9999/VORTECH/backend/internal/jobs"
)

// Orphan policies (see config.RangeConfig.OrphanPolicy).
const (
	OrphanDelete     = "delete"
	OrphanQuarantine = "quarantine"
)

// Config tunes the controller.
type Config struct {
	Capacity         cyberrange.Capacity
	ImageAllowlist   []string
	OrphanPolicy     string
	ProvisionTimeout time.Duration
	DestroyTimeout   time.Duration
	StaleAfter       time.Duration
}

// Controller provisions, expires, destroys and reconciles ranges.
type Controller struct {
	pool    *pgxpool.Pool
	q       *db.Queries
	cluster kube.Cluster
	log     *slog.Logger
	cfg     Config
	now     func() time.Time
}

// New returns a Controller.
func New(pool *pgxpool.Pool, cluster kube.Cluster, log *slog.Logger, cfg Config) *Controller {
	return &Controller{pool: pool, q: db.New(pool), cluster: cluster, log: log, cfg: cfg, now: time.Now}
}

// Register installs the range job handlers on r.
func (c *Controller) Register(r *jobs.Runner) {
	r.Handle(cyberrange.JobAdmit, time.Minute, func(ctx context.Context, _ jobs.Job) error {
		_, err := c.Admit(ctx)
		return err
	})
	// The provision handler waits up to ProvisionTimeout for readiness;
	// the job timeout leaves room for applying objects and bookkeeping.
	r.Handle(cyberrange.JobProvision, c.cfg.ProvisionTimeout+2*time.Minute, c.provision)
	r.Handle(cyberrange.JobDestroy, c.cfg.DestroyTimeout+time.Minute, c.destroy)
}

// Admit is the capacity scheduler. Under a global lock it walks pending
// ranges in arrival order, starts provisioning each one that fits, and
// queues the rest. Admission is strictly first come, first served: once a
// range does not fit, later (possibly smaller) ranges wait too, so large
// templates are never starved.
func (c *Controller) Admit(ctx context.Context) (admitted int, err error) {
	err = pgx.BeginFunc(ctx, c.pool, func(tx pgx.Tx) error {
		admitted = 0
		q := db.New(tx)
		if err := q.LockRangeAdmission(ctx); err != nil {
			return err
		}
		u, err := q.SumRangeUsage(ctx, cyberrange.CapacityStates())
		if err != nil {
			return err
		}
		usage := cyberrange.Resources{CPUMillis: u.CpuMillis, MemoryMiB: u.MemoryMib, StorageMiB: u.StorageMib}
		pending, err := q.ListPendingRanges(ctx, 100)
		if err != nil {
			return err
		}
		blocked := ""
		for _, r := range pending {
			cur := cyberrange.State(r.State)
			req := cyberrange.Resources{CPUMillis: int64(r.RequestedCpuMillis), MemoryMiB: int64(r.RequestedMemoryMib), StorageMiB: int64(r.RequestedStorageMib)}

			if ok, dim := c.cfg.Capacity.Fits(cyberrange.Resources{}, req); !ok {
				// It could never fit, even on an empty cluster.
				if err := c.fail(ctx, tx, r.ID, cur, "the range exceeds the platform's total "+dim+" capacity"); err != nil {
					return err
				}
				continue
			}
			if blocked == "" {
				ok, dim := c.cfg.Capacity.Fits(usage, req)
				if ok {
					if _, err := cyberrange.Transition(ctx, tx, r.ID, cur, cyberrange.Provisioning, "capacity reserved", cyberrange.ActorWorker); err != nil {
						return err
					}
					if _, err := jobs.Enqueue(ctx, q, tx, cyberrange.JobProvision, cyberrange.RangeJob{RangeID: r.ID}, jobs.Options{}); err != nil {
						return err
					}
					usage = usage.Add(req)
					admitted++
					continue
				}
				blocked = dim
			}
			if cur == cyberrange.Requested {
				if _, err := cyberrange.Transition(ctx, tx, r.ID, cur, cyberrange.Queued, "waiting for "+blocked+" capacity", cyberrange.ActorWorker); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err == nil && admitted > 0 {
		c.log.Info("ranges admitted", "count", admitted)
	}
	return admitted, err
}

// provision applies a range's objects and waits until its workloads run.
func (c *Controller) provision(ctx context.Context, j jobs.Job) error {
	var p cyberrange.RangeJob
	if err := j.Decode(&p); err != nil {
		return err
	}
	log := c.log.With("range_id", p.RangeID)
	row, err := c.q.GetRangeView(ctx, p.RangeID)
	if errors.Is(err, pgx.ErrNoRows) {
		return jobs.Permanent(fmt.Errorf("range %s not found", p.RangeID))
	}
	if err != nil {
		return err
	}
	r := row.Range
	state := cyberrange.State(r.State)
	if state != cyberrange.Provisioning && state != cyberrange.Starting {
		log.Info("provision skipped: range is no longer starting", "state", state)
		return nil
	}

	tpl, err := c.q.GetRangeTemplate(ctx, r.TemplateID)
	if err != nil {
		return err
	}
	var spec cyberrange.Spec
	if err := json.Unmarshal(tpl.Spec, &spec); err != nil {
		return c.failJob(ctx, j, r.ID, "the range template is invalid", jobs.Permanent(err))
	}
	if errs := spec.Validate(c.cfg.ImageAllowlist); len(errs) > 0 {
		return c.failJob(ctx, j, r.ID, "the range template is not permitted on this platform",
			jobs.Permanent(fmt.Errorf("template %s: %s", tpl.Slug, strings.Join(errs, "; "))))
	}
	secrets, err := generateSecrets(spec.SecretKeys())
	if err != nil {
		return err
	}
	bundle := kube.Build(kube.BuildInput{
		RangeID: r.ID, PlayerID: r.PlayerID, Namespace: r.Namespace, Spec: spec, Secrets: secrets,
		ExpiresAt: c.now().Add(time.Duration(tpl.TtlSeconds)*time.Second + c.cfg.ProvisionTimeout),
	})

	refs, applyErr := c.cluster.Apply(ctx, bundle)
	if err := c.recordResources(ctx, r.ID, refs); err != nil {
		log.Warn("record range resources", "error", err)
	}
	if applyErr != nil {
		return c.failJob(ctx, j, r.ID, "the cluster rejected the range", applyErr)
	}

	if state == cyberrange.Provisioning {
		err := pgx.BeginFunc(ctx, c.pool, func(tx pgx.Tx) error {
			_, err := cyberrange.Transition(ctx, tx, r.ID, cyberrange.Provisioning, cyberrange.Starting, "objects applied", cyberrange.ActorWorker)
			return err
		})
		if errors.Is(err, cyberrange.ErrStateChanged) {
			// Destroyed while we applied; the reconciler removes anything
			// created after the destroy job ran.
			log.Info("range stopped during provisioning")
			return nil
		}
		if err != nil {
			return err
		}
	}

	waitCtx, cancel := context.WithTimeout(ctx, c.cfg.ProvisionTimeout)
	err = c.cluster.WaitReady(waitCtx, r.Namespace)
	cancel()
	if err != nil {
		reason := "the range did not become ready in time"
		if errors.Is(err, kube.ErrPermanent) {
			reason = "a range workload could not start"
		}
		if ctx.Err() != nil {
			return ctx.Err() // shutdown or lost lease: retry elsewhere
		}
		return c.failJob(ctx, j, r.ID, reason, jobs.Permanent(err))
	}

	err = pgx.BeginFunc(ctx, c.pool, func(tx pgx.Tx) error {
		v, err := cyberrange.Transition(ctx, tx, r.ID, cyberrange.Starting, cyberrange.Ready, "all workloads ready", cyberrange.ActorWorker)
		if err != nil {
			return err
		}
		return audit.Insert(ctx, db.New(tx), audit.Entry{
			ActorType: audit.ActorService, ActorID: cyberrange.ActorWorker,
			Action: "range.provisioned", ResourceType: "range", ResourceID: r.ID.String(), Result: audit.ResultSuccess,
			Metadata: map[string]any{"template": tpl.Slug, "objects": len(refs), "expires_at": v.ExpiresAt},
		})
	})
	if errors.Is(err, cyberrange.ErrStateChanged) {
		log.Info("range stopped before it became ready")
		return nil
	}
	if err == nil {
		log.Info("range ready", "template", tpl.Slug)
	}
	return err
}

// failJob decides whether a provisioning error is final. Transient errors
// are retried by the job queue; final ones fail the range (which queues
// its cleanup). The player sees reason, never cluster internals.
func (c *Controller) failJob(ctx context.Context, j jobs.Job, id uuid.UUID, reason string, cause error) error {
	if !jobs.IsPermanent(cause) && !errors.Is(cause, kube.ErrPermanent) && !j.LastAttempt() {
		return cause
	}
	// Record the failure even if the job's context just ended.
	ctx = context.WithoutCancel(ctx)
	err := pgx.BeginFunc(ctx, c.pool, func(tx pgx.Tx) error {
		row, err := db.New(tx).GetRangeView(ctx, id)
		if err != nil {
			return err
		}
		if err := c.fail(ctx, tx, id, cyberrange.State(row.Range.State), reason); err != nil {
			return err
		}
		return audit.Insert(ctx, db.New(tx), audit.Entry{
			ActorType: audit.ActorService, ActorID: cyberrange.ActorWorker,
			Action: "range.failed", ResourceType: "range", ResourceID: id.String(), Result: audit.ResultFailure,
			Metadata: map[string]any{"reason": reason, "error": truncate(cause.Error(), 1000)},
		})
	})
	if err != nil {
		return fmt.Errorf("fail range: %w (cause: %v)", err, cause)
	}
	return jobs.Permanent(cause)
}

// fail moves a range to FAILED and queues its cleanup, if its state allows.
func (c *Controller) fail(ctx context.Context, tx pgx.Tx, id uuid.UUID, cur cyberrange.State, reason string) error {
	if !cyberrange.CanTransition(cur, cyberrange.Failed) {
		return nil // already stopping or gone
	}
	if _, err := cyberrange.Transition(ctx, tx, id, cur, cyberrange.Failed, reason, cyberrange.ActorWorker); err != nil {
		return err
	}
	_, err := jobs.Enqueue(ctx, db.New(tx), tx, cyberrange.JobDestroy, cyberrange.RangeJob{RangeID: id}, jobs.Options{})
	return err
}

// destroy deletes a range's namespace and marks it DESTROYED.
func (c *Controller) destroy(ctx context.Context, j jobs.Job) error {
	var p cyberrange.RangeJob
	if err := j.Decode(&p); err != nil {
		return err
	}
	row, err := c.q.GetRangeView(ctx, p.RangeID)
	if errors.Is(err, pgx.ErrNoRows) {
		return jobs.Permanent(fmt.Errorf("range %s not found", p.RangeID))
	}
	if err != nil {
		return err
	}
	r := row.Range
	switch state := cyberrange.State(r.State); {
	case state == cyberrange.Destroyed:
		return nil
	case state != cyberrange.Stopping:
		if !cyberrange.CanTransition(state, cyberrange.Stopping) {
			return jobs.Permanent(fmt.Errorf("range %s cannot stop from %s", r.ID, state))
		}
		err := pgx.BeginFunc(ctx, c.pool, func(tx pgx.Tx) error {
			_, err := cyberrange.Transition(ctx, tx, r.ID, state, cyberrange.Stopping, "cleanup", cyberrange.ActorWorker)
			return err
		})
		if err != nil {
			return err // ErrStateChanged: retried with a fresh read
		}
	}

	if err := c.cluster.Delete(ctx, r.Namespace); err != nil {
		return err
	}
	waitCtx, cancel := context.WithTimeout(ctx, c.cfg.DestroyTimeout)
	err = c.cluster.WaitGone(waitCtx, r.Namespace)
	cancel()
	if err != nil {
		return err
	}

	return pgx.BeginFunc(ctx, c.pool, func(tx pgx.Tx) error {
		q := db.New(tx)
		if _, err := cyberrange.Transition(ctx, tx, r.ID, cyberrange.Stopping, cyberrange.Destroyed, "namespace deleted", cyberrange.ActorWorker); err != nil {
			return err
		}
		if err := audit.Insert(ctx, q, audit.Entry{
			ActorType: audit.ActorService, ActorID: cyberrange.ActorWorker,
			Action: "range.destroyed", ResourceType: "range", ResourceID: r.ID.String(), Result: audit.ResultSuccess,
		}); err != nil {
			return err
		}
		// Capacity was freed: let queued ranges in.
		_, err := jobs.Enqueue(ctx, q, tx, cyberrange.JobAdmit, map[string]any{}, jobs.Options{})
		return err
	})
}

func (c *Controller) recordResources(ctx context.Context, id uuid.UUID, refs []kube.ResourceRef) error {
	if len(refs) == 0 {
		return nil
	}
	kinds, names := make([]string, len(refs)), make([]string, len(refs))
	for i, r := range refs {
		kinds[i], names[i] = r.Kind, r.Name
	}
	return c.q.RecordRangeResources(ctx, db.RecordRangeResourcesParams{RangeID: id, Kinds: kinds, Names: names})
}

// generateSecrets creates a fresh random value per key. On a retried
// provision the Secret already exists and keeps its original values.
func generateSecrets(keys []string) (map[string]string, error) {
	out := make(map[string]string, len(keys))
	for _, k := range keys {
		b := make([]byte, 24)
		if _, err := rand.Read(b); err != nil {
			return nil, err
		}
		out[k] = base64.RawURLEncoding.EncodeToString(b)
	}
	return out, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
