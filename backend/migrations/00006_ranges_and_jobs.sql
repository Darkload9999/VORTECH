-- Cyber ranges and the asynchronous job queue.
--
-- The API never talks to Kubernetes: it records a REQUESTED range and a
-- job in one transaction and returns 202. The worker claims jobs
-- (FOR UPDATE SKIP LOCKED with leases), provisions through the Kubernetes
-- API and records every state change in range_state_transitions.

-- +goose Up

-- Range blueprints from scenario-as-code (scenarios/<slug>/ranges.yaml).
-- Templates are never deleted while ranges reference them; removing one
-- from the scenario marks it inactive.
CREATE TABLE range_templates (
    id                uuid        PRIMARY KEY DEFAULT uuidv7(),
    scenario_id       uuid        NOT NULL REFERENCES scenarios (id) ON DELETE CASCADE,
    slug              text        NOT NULL,
    name              text        NOT NULL,
    description       text        NOT NULL DEFAULT '',
    ttl_seconds       integer     NOT NULL,
    cpu_millis        integer     NOT NULL,
    memory_mib        integer     NOT NULL,
    storage_mib       integer     NOT NULL,
    -- Validated workload and network specification (cyberrange.Spec).
    spec              jsonb       NOT NULL,
    active            boolean     NOT NULL DEFAULT true,
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT range_templates_scenario_slug_key UNIQUE (scenario_id, slug),
    CONSTRAINT range_templates_slug_chk CHECK (slug ~ '^[a-z0-9][a-z0-9-]{1,40}$'),
    CONSTRAINT range_templates_ttl_chk CHECK (ttl_seconds BETWEEN 300 AND 86400),
    CONSTRAINT range_templates_resources_chk CHECK (cpu_millis > 0 AND memory_mib > 0 AND storage_mib >= 0),
    CONSTRAINT range_templates_spec_object_chk CHECK (jsonb_typeof(spec) = 'object')
);
CREATE TRIGGER range_templates_set_updated_at BEFORE UPDATE ON range_templates
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE ranges (
    id                    uuid        PRIMARY KEY DEFAULT uuidv7(),
    player_id             uuid        NOT NULL REFERENCES players (id) ON DELETE RESTRICT,
    template_id           uuid        NOT NULL REFERENCES range_templates (id) ON DELETE RESTRICT,
    state                 text        NOT NULL DEFAULT 'REQUESTED',
    -- Kubernetes namespace: range-<id>.
    namespace             text        NOT NULL,
    requested_cpu_millis  integer     NOT NULL,
    requested_memory_mib  integer     NOT NULL,
    requested_storage_mib integer     NOT NULL,
    failure_reason        text,
    expires_at            timestamptz,
    ready_at              timestamptz,
    destroyed_at          timestamptz,
    created_at            timestamptz NOT NULL DEFAULT now(),
    updated_at            timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT ranges_namespace_key UNIQUE (namespace),
    CONSTRAINT ranges_namespace_chk CHECK (namespace = 'range-' || id::text),
    CONSTRAINT ranges_state_chk CHECK (state IN (
        'REQUESTED', 'QUEUED', 'PROVISIONING', 'STARTING', 'READY', 'ACTIVE',
        'STOPPING', 'DESTROYED', 'FAILED', 'EXPIRED')),
    CONSTRAINT ranges_resources_chk CHECK (requested_cpu_millis > 0 AND requested_memory_mib > 0 AND requested_storage_mib >= 0),
    CONSTRAINT ranges_destroyed_chk CHECK ((state = 'DESTROYED') = (destroyed_at IS NOT NULL))
);
-- At most one live range per player (race-free, enforced by the database).
CREATE UNIQUE INDEX ranges_one_live_per_player_idx ON ranges (player_id)
    WHERE state IN ('REQUESTED', 'QUEUED', 'PROVISIONING', 'STARTING', 'READY', 'ACTIVE');
CREATE INDEX ranges_player_idx ON ranges (player_id, id DESC);
CREATE INDEX ranges_state_idx ON ranges (state, created_at);
CREATE INDEX ranges_expiry_idx ON ranges (expires_at) WHERE state IN ('STARTING', 'READY', 'ACTIVE');
CREATE TRIGGER ranges_set_updated_at BEFORE UPDATE ON ranges
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- Append-only lifecycle history.
CREATE TABLE range_state_transitions (
    id         uuid        PRIMARY KEY DEFAULT uuidv7(),
    range_id   uuid        NOT NULL REFERENCES ranges (id) ON DELETE CASCADE,
    from_state text,
    to_state   text        NOT NULL,
    reason     text        NOT NULL DEFAULT '',
    actor      text        NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX range_state_transitions_range_idx ON range_state_transitions (range_id, id);

-- Kubernetes objects created for a range (for operators and reconciliation).
CREATE TABLE range_resources (
    range_id   uuid        NOT NULL REFERENCES ranges (id) ON DELETE CASCADE,
    kind       text        NOT NULL,
    name       text        NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (range_id, kind, name)
);

-- Durable job queue for the worker. Claimed with FOR UPDATE SKIP LOCKED;
-- a crashed worker's lease expires and another worker retries the job.
CREATE TABLE jobs (
    id           uuid        PRIMARY KEY DEFAULT uuidv7(),
    kind         text        NOT NULL,
    payload      jsonb       NOT NULL DEFAULT '{}'::jsonb,
    state        text        NOT NULL DEFAULT 'queued',
    attempts     integer     NOT NULL DEFAULT 0,
    max_attempts integer     NOT NULL DEFAULT 5,
    run_after    timestamptz NOT NULL DEFAULT now(),
    locked_by    text,
    locked_until timestamptz,
    last_error   text,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT jobs_kind_chk CHECK (kind ~ '^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)+$'),
    CONSTRAINT jobs_state_chk CHECK (state IN ('queued', 'running', 'succeeded', 'failed')),
    CONSTRAINT jobs_attempts_chk CHECK (attempts >= 0 AND max_attempts >= 1),
    CONSTRAINT jobs_payload_object_chk CHECK (jsonb_typeof(payload) = 'object')
);
CREATE INDEX jobs_claimable_idx ON jobs (run_after) WHERE state IN ('queued', 'running');
CREATE TRIGGER jobs_set_updated_at BEFORE UPDATE ON jobs
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- +goose Down
DROP TABLE IF EXISTS jobs;
DROP TABLE IF EXISTS range_resources;
DROP TABLE IF EXISTS range_state_transitions;
DROP TABLE IF EXISTS ranges;
DROP TABLE IF EXISTS range_templates;
