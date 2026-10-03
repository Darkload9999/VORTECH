// Package jobs is the durable background job queue.
//
// Jobs are rows in the jobs table, enqueued inside the transaction that
// needs them (so a job exists if and only if its cause committed) and
// claimed by workers with FOR UPDATE SKIP LOCKED. A claim is a lease: the
// runner extends it while the handler works, and a job whose worker died
// is claimed again once the lease expires. Handlers must therefore be
// idempotent. Delivery is at-least-once.
package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/Darkload9999/VORTECH/backend/internal/database"
	"github.com/Darkload9999/VORTECH/backend/internal/database/db"
)

// DefaultMaxAttempts bounds retries of a failing job.
const DefaultMaxAttempts = 5

// Job is one claimed job as seen by a handler.
type Job struct {
	ID          uuid.UUID
	Kind        string
	Payload     json.RawMessage
	Attempt     int32 // 1 on the first run
	MaxAttempts int32
}

// Decode unmarshals the payload into v.
func (j Job) Decode(v any) error {
	if err := json.Unmarshal(j.Payload, v); err != nil {
		return Permanent(fmt.Errorf("decode %s payload: %w", j.Kind, err))
	}
	return nil
}

// LastAttempt reports whether a failure now is final.
func (j Job) LastAttempt() bool { return j.Attempt >= j.MaxAttempts }

// Options tune one enqueued job.
type Options struct {
	RunAfter    time.Time
	MaxAttempts int32
}

// Enqueue inserts a job with q and wakes workers through ex. Both should
// be bound to the same transaction: the job and the wake-up then take
// effect only when it commits.
func Enqueue(ctx context.Context, q *db.Queries, ex database.Execer, kind string, payload any, opts Options) (uuid.UUID, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return uuid.Nil, fmt.Errorf("encode %s payload: %w", kind, err)
	}
	if opts.MaxAttempts <= 0 {
		opts.MaxAttempts = DefaultMaxAttempts
	}
	params := db.EnqueueJobParams{Kind: kind, Payload: raw, MaxAttempts: opts.MaxAttempts}
	if !opts.RunAfter.IsZero() {
		params.RunAfter = &opts.RunAfter
	}
	id, err := q.EnqueueJob(ctx, params)
	if err != nil {
		return uuid.Nil, fmt.Errorf("enqueue %s: %w", kind, err)
	}
	if err := database.Notify(ctx, ex, database.ChannelJobs, kind); err != nil {
		return uuid.Nil, err
	}
	return id, nil
}

type permanentError struct{ err error }

func (e permanentError) Error() string { return e.err.Error() }
func (e permanentError) Unwrap() error { return e.err }

// Permanent marks an error that retrying cannot fix; the job fails at once.
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return permanentError{err}
}

// IsPermanent reports whether err was marked with Permanent.
func IsPermanent(err error) bool {
	var p permanentError
	return errors.As(err, &p)
}
