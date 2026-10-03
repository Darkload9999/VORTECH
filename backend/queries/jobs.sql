-- Durable job queue (see internal/jobs).

-- name: EnqueueJob :one
INSERT INTO jobs (kind, payload, run_after, max_attempts)
VALUES (@kind, @payload, COALESCE(sqlc.narg(run_after)::timestamptz, now()), @max_attempts)
RETURNING id;

-- name: ClaimJob :one
-- Claims the oldest runnable job of the given kinds: a queued job that is
-- due, or a running job whose lease expired (its worker died). SKIP LOCKED
-- lets any number of workers claim concurrently without blocking.
UPDATE jobs
SET state = 'running',
    attempts = attempts + 1,
    locked_by = @worker::text,
    locked_until = now() + make_interval(secs => @lease_seconds::float8)
WHERE id = (
    SELECT j.id FROM jobs j
    WHERE j.kind = ANY(@kinds::text[])
      AND ((j.state = 'queued' AND j.run_after <= now())
        OR (j.state = 'running' AND j.locked_until < now()))
    ORDER BY j.run_after, j.id
    LIMIT 1
    FOR UPDATE SKIP LOCKED
)
RETURNING id, kind, payload, attempts, max_attempts;

-- name: ExtendJobLease :execrows
-- Heartbeat. Zero rows means the lease was lost (another worker took over).
UPDATE jobs SET locked_until = now() + make_interval(secs => @lease_seconds::float8)
WHERE id = @id AND locked_by = @worker::text AND state = 'running';

-- name: CompleteJob :execrows
UPDATE jobs SET state = 'succeeded', locked_by = NULL, locked_until = NULL, last_error = NULL
WHERE id = @id AND locked_by = @worker::text AND state = 'running';

-- name: FailJob :one
-- Records a failed attempt: the job is retried after a backoff unless the
-- error is permanent or attempts are exhausted.
UPDATE jobs
SET state = CASE WHEN @permanent::boolean OR attempts >= max_attempts THEN 'failed' ELSE 'queued' END,
    run_after = now() + make_interval(secs => @backoff_seconds::float8),
    last_error = @last_error::text,
    locked_by = NULL,
    locked_until = NULL
WHERE id = @id AND locked_by = @worker::text AND state = 'running'
RETURNING state;

-- name: ReleaseJob :execrows
-- Gives an interrupted job back (worker shutdown) without consuming an attempt.
UPDATE jobs SET state = 'queued', attempts = GREATEST(attempts - 1, 0), run_after = now(),
    locked_by = NULL, locked_until = NULL
WHERE id = @id AND locked_by = @worker::text AND state = 'running';

-- name: PruneFinishedJobs :execrows
-- Succeeded jobs are kept for a while for operators; failed ones longer.
DELETE FROM jobs
WHERE (state = 'succeeded' AND updated_at < now() - make_interval(secs => @succeeded_after_seconds::float8))
   OR (state = 'failed' AND updated_at < now() - make_interval(secs => @failed_after_seconds::float8));

-- name: CountJobsByState :many
SELECT kind, state, count(*)::int AS jobs FROM jobs GROUP BY kind, state ORDER BY kind, state;

-- name: SecondsUntilNextJob :one
-- How long until a job of these kinds becomes claimable (a retry's backoff
-- ends or a lease expires); -1 when none is pending. Computed by the
-- database so worker clock skew does not matter.
SELECT COALESCE(GREATEST(EXTRACT(EPOCH FROM min(CASE WHEN state = 'queued' THEN run_after ELSE locked_until END) - now()), 0), -1)::float8 AS seconds
FROM jobs
WHERE kind = ANY(@kinds::text[]) AND state IN ('queued', 'running');
