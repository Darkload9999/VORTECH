-- name: InsertAuditLog :one
INSERT INTO audit_logs (
    actor_type,
    actor_id,
    action,
    resource_type,
    resource_id,
    result,
    request_id,
    source_ip,
    metadata
) VALUES (
    @actor_type,
    sqlc.narg(actor_id),
    @action,
    @resource_type,
    sqlc.narg(resource_id),
    @result,
    sqlc.narg(request_id),
    sqlc.narg(source_ip),
    @metadata
)
RETURNING id, occurred_at;

-- name: ListAuditLogs :many
-- Newest first with keyset pagination: pass the (occurred_at, id) of the
-- last row of the previous page as the cursor, or NULLs for the first page.
SELECT *
FROM audit_logs
WHERE sqlc.narg(cursor_occurred_at)::timestamptz IS NULL
   OR (occurred_at, id) < (sqlc.narg(cursor_occurred_at)::timestamptz, sqlc.narg(cursor_id)::uuid)
ORDER BY occurred_at DESC, id DESC
LIMIT @page_size;
