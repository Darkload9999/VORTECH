-- Range templates and per-player cyber ranges (see internal/cyberrange).

-- name: UpsertRangeTemplate :one
INSERT INTO range_templates (scenario_id, slug, name, description, ttl_seconds, cpu_millis, memory_mib, storage_mib, spec, active)
VALUES (@scenario_id, @slug, @name, @description, @ttl_seconds, @cpu_millis, @memory_mib, @storage_mib, @spec, true)
ON CONFLICT (scenario_id, slug) DO UPDATE
SET name = EXCLUDED.name, description = EXCLUDED.description, ttl_seconds = EXCLUDED.ttl_seconds,
    cpu_millis = EXCLUDED.cpu_millis, memory_mib = EXCLUDED.memory_mib, storage_mib = EXCLUDED.storage_mib,
    spec = EXCLUDED.spec, active = true
RETURNING id;

-- name: DeactivateStaleRangeTemplates :execrows
-- Templates removed from the scenario stay for existing ranges but cannot
-- be used for new ones.
UPDATE range_templates SET active = false
WHERE scenario_id = @scenario_id AND active AND NOT (slug = ANY(@keep::text[]));

-- name: ListActiveRangeTemplates :many
SELECT t.id, t.slug, t.name, t.description, t.ttl_seconds, t.cpu_millis, t.memory_mib, t.storage_mib, t.spec
FROM range_templates t
JOIN scenarios s ON s.id = t.scenario_id
WHERE s.is_active AND t.active
ORDER BY t.cpu_millis, t.slug;

-- name: GetActiveRangeTemplateBySlug :one
SELECT t.id, t.slug, t.name, t.description, t.ttl_seconds, t.cpu_millis, t.memory_mib, t.storage_mib, t.spec
FROM range_templates t
JOIN scenarios s ON s.id = t.scenario_id
WHERE s.is_active AND t.active AND t.slug = @slug;

-- name: GetRangeTemplate :one
SELECT * FROM range_templates WHERE id = @id;

-- name: CreateRange :one
INSERT INTO ranges (id, player_id, template_id, namespace, requested_cpu_millis, requested_memory_mib, requested_storage_mib)
VALUES (@id, @player_id, @template_id, @namespace, @cpu_millis, @memory_mib, @storage_mib)
RETURNING *;

-- name: GetRangeView :one
SELECT sqlc.embed(r), t.slug AS template_slug, t.name AS template_name, p.user_id
FROM ranges r
JOIN range_templates t ON t.id = r.template_id
JOIN players p ON p.id = r.player_id
WHERE r.id = @id;

-- name: ListPlayerRanges :many
-- Newest first; pass the last id of the previous page as before_id.
SELECT sqlc.embed(r), t.slug AS template_slug, t.name AS template_name, p.user_id
FROM ranges r
JOIN range_templates t ON t.id = r.template_id
JOIN players p ON p.id = r.player_id
WHERE r.player_id = @player_id
  AND (sqlc.narg(before_id)::uuid IS NULL OR r.id < sqlc.narg(before_id)::uuid)
ORDER BY r.id DESC
LIMIT @page_size;

-- name: TransitionRange :one
-- Compare-and-set state change: it only applies if the range is still in
-- from_state, so concurrent actors (API, worker, expiry, reconciler) can
-- never overwrite each other's transitions.
UPDATE ranges r
SET state = @to_state::text,
    failure_reason = CASE WHEN @to_state::text = 'FAILED' THEN @reason::text ELSE r.failure_reason END,
    ready_at = CASE WHEN @to_state::text = 'READY' THEN now() ELSE r.ready_at END,
    expires_at = CASE WHEN @to_state::text = 'READY'
        THEN now() + make_interval(secs => (SELECT t.ttl_seconds FROM range_templates t WHERE t.id = r.template_id))
        ELSE r.expires_at END,
    destroyed_at = CASE WHEN @to_state::text = 'DESTROYED' THEN now() ELSE NULL END
WHERE r.id = @id AND r.state = @from_state::text
RETURNING *;

-- name: InsertRangeTransition :exec
INSERT INTO range_state_transitions (range_id, from_state, to_state, reason, actor)
VALUES (@range_id, @from_state, @to_state, @reason, @actor);

-- name: ListRangeTransitions :many
SELECT from_state, to_state, reason, actor, created_at
FROM range_state_transitions WHERE range_id = @range_id ORDER BY id;

-- name: RecordRangeResources :exec
INSERT INTO range_resources (range_id, kind, name)
SELECT @range_id::uuid, unnest(@kinds::text[]), unnest(@names::text[])
ON CONFLICT DO NOTHING;

-- name: LockRangeAdmission :exec
-- Serialises capacity decisions across workers for the transaction.
SELECT pg_advisory_xact_lock(hashtextextended('range-admission', 0));

-- name: SumRangeUsage :one
-- Resources reserved by ranges in the given (capacity-consuming) states.
SELECT COALESCE(sum(requested_cpu_millis), 0)::bigint AS cpu_millis,
       COALESCE(sum(requested_memory_mib), 0)::bigint AS memory_mib,
       COALESCE(sum(requested_storage_mib), 0)::bigint AS storage_mib,
       count(*)::int AS ranges
FROM ranges WHERE state = ANY(@states::text[]);

-- name: ListPendingRanges :many
-- Ranges waiting for capacity, first come first served.
SELECT * FROM ranges WHERE state IN ('REQUESTED', 'QUEUED')
ORDER BY created_at, id
LIMIT @max_rows
FOR UPDATE;

-- name: ListExpiredRanges :many
SELECT id FROM ranges
WHERE state IN ('READY', 'ACTIVE') AND expires_at <= now()
ORDER BY expires_at
LIMIT @max_rows;

-- name: ListRangesWithNamespaces :many
-- Ranges whose namespace may exist in the cluster (reconciliation).
SELECT id, state, namespace, updated_at FROM ranges
WHERE state NOT IN ('REQUESTED', 'QUEUED', 'DESTROYED');

-- name: RangeStatesByID :many
SELECT id, state, updated_at FROM ranges WHERE id = ANY(@ids::uuid[]);

-- name: ListStuckRanges :many
-- Ranges in a state that needs a job, without one queued or running, and
-- unchanged for longer than stale_seconds (the job failed permanently or
-- was lost). The reconciler re-drives them.
SELECT r.id, r.state FROM ranges r
WHERE r.state IN ('PROVISIONING', 'STARTING', 'FAILED', 'EXPIRED', 'STOPPING')
  AND r.updated_at < now() - make_interval(secs => @stale_seconds::float8)
  AND NOT EXISTS (
      SELECT 1 FROM jobs j
      WHERE j.kind IN ('range.provision', 'range.destroy')
        AND j.state IN ('queued', 'running')
        AND j.payload ->> 'range_id' = r.id::text)
ORDER BY r.updated_at
LIMIT @max_rows;

-- name: CountRangesByState :many
SELECT state, count(*)::int AS ranges FROM ranges GROUP BY state ORDER BY state;
