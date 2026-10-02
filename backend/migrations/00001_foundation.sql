-- Foundation schema shared by every module.
--
-- Requires PostgreSQL 18+ (uuidv7()). Time-ordered UUIDv7 keys keep B-tree
-- inserts append-mostly, which matters for high-volume tables such as
-- audit_logs, telemetry_events and security_events.

-- +goose Up

-- Generic trigger maintaining updated_at; attached to mutable tables in
-- later migrations:
--   CREATE TRIGGER <table>_set_updated_at BEFORE UPDATE ON <table>
--     FOR EACH ROW EXECUTE FUNCTION set_updated_at();
-- +goose StatementBegin
CREATE FUNCTION set_updated_at() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    NEW.updated_at := now();
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

-- Security audit trail. Rows are immutable once written.
--
-- actor_id is deliberately text without a foreign key: audit records must
-- outlive the users, ranges and other resources they describe, and actors
-- may be non-user principals (the worker, the reconciler).
CREATE TABLE audit_logs (
    id            uuid        PRIMARY KEY DEFAULT uuidv7(),
    occurred_at   timestamptz NOT NULL DEFAULT now(),
    actor_type    text        NOT NULL,
    actor_id      text,
    action        text        NOT NULL,
    resource_type text        NOT NULL,
    resource_id   text,
    result        text        NOT NULL,
    request_id    text,
    source_ip     inet,
    metadata      jsonb       NOT NULL DEFAULT '{}'::jsonb,

    CONSTRAINT audit_logs_actor_type_chk
        CHECK (actor_type IN ('user', 'system', 'service', 'anonymous')),
    CONSTRAINT audit_logs_anonymous_actor_chk
        CHECK (actor_type <> 'anonymous' OR actor_id IS NULL),
    CONSTRAINT audit_logs_result_chk
        CHECK (result IN ('success', 'failure', 'denied')),
    -- Dotted lowercase names, e.g. range.created, terminal.opened, authz.denied
    CONSTRAINT audit_logs_action_chk
        CHECK (action ~ '^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)+$'),
    CONSTRAINT audit_logs_resource_type_chk
        CHECK (resource_type ~ '^[a-z][a-z0-9_]*$'),
    CONSTRAINT audit_logs_metadata_object_chk
        CHECK (jsonb_typeof(metadata) = 'object')
);

CREATE INDEX audit_logs_occurred_at_idx ON audit_logs (occurred_at DESC, id DESC);
CREATE INDEX audit_logs_actor_idx ON audit_logs (actor_type, actor_id, occurred_at DESC);
CREATE INDEX audit_logs_resource_idx ON audit_logs (resource_type, resource_id, occurred_at DESC);

-- Application code can never rewrite history. Retention is handled by
-- dropping time partitions or by a maintenance role, not by the API.
-- +goose StatementBegin
CREATE FUNCTION audit_logs_reject_mutation() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'audit_logs is append-only: % is not permitted', TG_OP
        USING ERRCODE = 'insufficient_privilege';
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER audit_logs_append_only
    BEFORE UPDATE OR DELETE ON audit_logs
    FOR EACH ROW EXECUTE FUNCTION audit_logs_reject_mutation();

-- +goose Down
DROP TABLE IF EXISTS audit_logs;
DROP FUNCTION IF EXISTS audit_logs_reject_mutation();
DROP FUNCTION IF EXISTS set_updated_at();
