-- Player progression and server-validated world interactions.
--
-- Career levels are platform data (not scenario data): a player's career
-- spans scenarios. A player's rank is derived from XP at read time (the
-- highest level whose min_xp <= xp), so rank and XP can never disagree.
--
-- Zone access rule (implemented in internal/world/access.go):
--   accessible = (explicitly unlocked
--                 OR (rank >= required_career_rank
--                     AND (unlocked_by_default OR required_career_rank > 0)))
--                AND the parent zone is accessible
-- so a zone is open by default, rank-gated (opens automatically at the
-- required rank), or explicit-only (unlocked by an engagement, instructor
-- or admin). Explicit unlocks bypass the rank requirement.

-- +goose Up
CREATE TABLE career_levels (
    id          uuid        PRIMARY KEY DEFAULT uuidv7(),
    rank        integer     NOT NULL,
    code        text        NOT NULL,
    name        text        NOT NULL,
    description text        NOT NULL DEFAULT '',
    min_xp      integer     NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT career_levels_rank_key UNIQUE (rank),
    CONSTRAINT career_levels_code_key UNIQUE (code),
    CONSTRAINT career_levels_min_xp_key UNIQUE (min_xp),
    CONSTRAINT career_levels_rank_chk CHECK (rank >= 0),
    CONSTRAINT career_levels_code_chk CHECK (code ~ '^[A-Z][A-Z0-9_]{0,31}$'),
    CONSTRAINT career_levels_min_xp_chk CHECK (min_xp >= 0 AND (rank <> 0 OR min_xp = 0))
);
CREATE TRIGGER career_levels_set_updated_at BEFORE UPDATE ON career_levels
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

INSERT INTO career_levels (rank, code, name, description, min_xp) VALUES
    (0, 'INTERN',     'Security Intern',             'Learning the environment and the rules of engagement.', 0),
    (1, 'ANALYST_I',  'Security Analyst I',          'Trusted with routine investigations and assessments.', 100),
    (2, 'ANALYST_II', 'Security Analyst II',         'Runs engagements against core infrastructure.',        300),
    (3, 'SENIOR',     'Senior Security Engineer',    'Leads complex engagements and incident response.',     700),
    (4, 'LEAD',       'Security Lead',               'Plans programmes and mentors the team.',               1500),
    (5, 'PRINCIPAL',  'Principal Security Engineer', 'Shapes NEXORA''s security strategy.',                   3000);

CREATE TABLE player_progress (
    player_id       uuid        PRIMARY KEY REFERENCES players (id) ON DELETE CASCADE,
    xp              integer     NOT NULL DEFAULT 0,
    -- Last zone the player entered (authoritative position for interaction
    -- checks). NULL means the player has not entered the world yet.
    current_zone_id uuid        REFERENCES world_zones (id) ON DELETE SET NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT player_progress_xp_chk CHECK (xp >= 0)
);
CREATE TRIGGER player_progress_set_updated_at BEFORE UPDATE ON player_progress
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- Explicit zone grants. Removing a zone from a scenario removes its grants.
CREATE TABLE player_zone_unlocks (
    player_id   uuid        NOT NULL REFERENCES players (id) ON DELETE CASCADE,
    zone_id     uuid        NOT NULL REFERENCES world_zones (id) ON DELETE CASCADE,
    source      text        NOT NULL,
    unlocked_at timestamptz NOT NULL DEFAULT now(),

    PRIMARY KEY (player_id, zone_id),
    CONSTRAINT player_zone_unlocks_source_chk CHECK (source IN ('engagement', 'instructor', 'admin'))
);
CREATE INDEX player_zone_unlocks_zone_idx ON player_zone_unlocks (zone_id);

-- Assets a player has found in the world (first sighting only).
CREATE TABLE discoveries (
    player_id      uuid        NOT NULL REFERENCES players (id) ON DELETE CASCADE,
    asset_id       uuid        NOT NULL REFERENCES assets (id) ON DELETE CASCADE,
    object_id      uuid        REFERENCES world_objects (id) ON DELETE SET NULL,
    discovered_via text        NOT NULL,
    discovered_at  timestamptz NOT NULL DEFAULT now(),

    PRIMARY KEY (player_id, asset_id)
);
CREATE INDEX discoveries_asset_idx ON discoveries (asset_id);

-- Every attempted interaction with its server-side outcome. High volume:
-- partition by month or prune once retention requirements are set.
CREATE TABLE interactions (
    id               uuid        PRIMARY KEY DEFAULT uuidv7(),
    player_id        uuid        NOT NULL REFERENCES players (id) ON DELETE CASCADE,
    company_id       uuid        NOT NULL REFERENCES companies (id) ON DELETE CASCADE,
    interaction_type text        NOT NULL,
    zone_id          uuid        REFERENCES world_zones (id) ON DELETE SET NULL,
    object_id        uuid        REFERENCES world_objects (id) ON DELETE SET NULL,
    employee_id      uuid        REFERENCES employees (id) ON DELETE SET NULL,
    outcome          text        NOT NULL,
    reason           text,
    xp_awarded       integer     NOT NULL DEFAULT 0,
    request_id       text,
    created_at       timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT interactions_type_chk CHECK (interaction_type IN (
        'ENTER_ZONE', 'LEAVE_ZONE', 'OPEN_DOOR', 'CLOSE_DOOR', 'USE_WORKSTATION',
        'TALK_TO_NPC', 'INSPECT_OBJECT', 'ACCESS_TERMINAL', 'READ_DOCUMENT', 'START_ENGAGEMENT')),
    CONSTRAINT interactions_outcome_chk CHECK (outcome IN ('allowed', 'denied')),
    CONSTRAINT interactions_reason_chk CHECK ((outcome = 'denied') = (reason IS NOT NULL)),
    CONSTRAINT interactions_xp_chk CHECK (xp_awarded >= 0)
);
CREATE INDEX interactions_player_created_idx ON interactions (player_id, created_at DESC);

-- +goose Down
DROP TABLE IF EXISTS interactions;
DROP TABLE IF EXISTS discoveries;
DROP TABLE IF EXISTS player_zone_unlocks;
DROP TABLE IF EXISTS player_progress;
DROP TABLE IF EXISTS career_levels;
