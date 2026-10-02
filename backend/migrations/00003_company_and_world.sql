-- Scenario-defined fictional enterprise and its physical/digital world.
--
-- Everything here is imported from scenario-as-code (scenarios/<slug>/*.yaml)
-- by `scenario import`; the API only reads it. Rows are upserted by their
-- natural codes so IDs stay stable across re-imports and player state that
-- references them (zone unlocks, interactions) survives scenario updates.
--
-- Fictional identities (alex@nexora.local) live in enterprise_identities and
-- are never Keycloak users.

-- +goose Up
CREATE TABLE scenarios (
    id           uuid        PRIMARY KEY DEFAULT uuidv7(),
    slug         text        NOT NULL,
    name         text        NOT NULL,
    version      text        NOT NULL,
    description  text        NOT NULL DEFAULT '',
    status       text        NOT NULL DEFAULT 'draft',
    -- The scenario whose world the platform currently serves.
    is_active    boolean     NOT NULL DEFAULT false,
    -- SHA-256 over the source files: re-importing identical content is a no-op.
    content_hash text        NOT NULL,
    imported_at  timestamptz NOT NULL DEFAULT now(),
    published_at timestamptz,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT scenarios_slug_key UNIQUE (slug),
    CONSTRAINT scenarios_slug_chk CHECK (slug ~ '^[a-z0-9][a-z0-9-]{1,62}$'),
    CONSTRAINT scenarios_status_chk CHECK (status IN ('draft', 'published', 'archived')),
    CONSTRAINT scenarios_active_published_chk CHECK (NOT is_active OR status = 'published'),
    CONSTRAINT scenarios_published_at_chk CHECK (status <> 'published' OR published_at IS NOT NULL)
);
CREATE UNIQUE INDEX scenarios_single_active_idx ON scenarios ((true)) WHERE is_active;
CREATE TRIGGER scenarios_set_updated_at BEFORE UPDATE ON scenarios
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE companies (
    id           uuid        PRIMARY KEY DEFAULT uuidv7(),
    scenario_id  uuid        NOT NULL REFERENCES scenarios (id) ON DELETE CASCADE,
    code         text        NOT NULL,
    name         text        NOT NULL,
    legal_name   text        NOT NULL,
    industry     text        NOT NULL,
    description  text        NOT NULL DEFAULT '',
    headquarters text        NOT NULL DEFAULT '',
    founded_year integer,
    -- Fictional organisations only: reserved/special-use TLDs.
    email_domain text        NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT companies_scenario_key UNIQUE (scenario_id),
    CONSTRAINT companies_code_chk CHECK (code ~ '^[A-Z][A-Z0-9_-]{1,31}$'),
    CONSTRAINT companies_email_domain_chk
        CHECK (email_domain ~ '^([a-z0-9-]+\.)+(local|test|example|internal|invalid)$'),
    CONSTRAINT companies_founded_year_chk CHECK (founded_year IS NULL OR founded_year BETWEEN 1800 AND 2200)
);
CREATE TRIGGER companies_set_updated_at BEFORE UPDATE ON companies
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE departments (
    id          uuid        PRIMARY KEY DEFAULT uuidv7(),
    company_id  uuid        NOT NULL REFERENCES companies (id) ON DELETE CASCADE,
    code        text        NOT NULL,
    name        text        NOT NULL,
    description text        NOT NULL DEFAULT '',
    parent_id   uuid        REFERENCES departments (id) ON DELETE SET NULL,
    sort_order  integer     NOT NULL DEFAULT 0,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT departments_company_code_key UNIQUE (company_id, code),
    CONSTRAINT departments_code_chk CHECK (code ~ '^[A-Z][A-Z0-9_-]{0,31}$'),
    CONSTRAINT departments_not_own_parent_chk CHECK (parent_id IS DISTINCT FROM id)
);
CREATE TRIGGER departments_set_updated_at BEFORE UPDATE ON departments
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- Navigable areas of the 3D world (sites, buildings, floors, areas).
CREATE TABLE world_zones (
    id                   uuid        PRIMARY KEY DEFAULT uuidv7(),
    company_id           uuid        NOT NULL REFERENCES companies (id) ON DELETE CASCADE,
    code                 text        NOT NULL,
    name                 text        NOT NULL,
    kind                 text        NOT NULL,
    parent_id            uuid        REFERENCES world_zones (id) ON DELETE SET NULL,
    description          text        NOT NULL DEFAULT '',
    -- Progression gates. A zone is accessible when unlocked by default or
    -- explicitly unlocked for the player, and the player's career rank is
    -- at least required_career_rank.
    unlocked_by_default  boolean     NOT NULL DEFAULT false,
    required_career_rank integer     NOT NULL DEFAULT 0,
    sort_order           integer     NOT NULL DEFAULT 0,
    created_at           timestamptz NOT NULL DEFAULT now(),
    updated_at           timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT world_zones_company_code_key UNIQUE (company_id, code),
    CONSTRAINT world_zones_code_chk CHECK (code ~ '^[A-Z][A-Z0-9_]{0,63}$'),
    CONSTRAINT world_zones_kind_chk CHECK (kind IN ('site', 'building', 'floor', 'area')),
    CONSTRAINT world_zones_rank_chk CHECK (required_career_rank >= 0),
    CONSTRAINT world_zones_not_own_parent_chk CHECK (parent_id IS DISTINCT FROM id)
);
CREATE INDEX world_zones_parent_idx ON world_zones (parent_id);
CREATE TRIGGER world_zones_set_updated_at BEFORE UPDATE ON world_zones
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- Places inside a zone (offices, meeting rooms, the server room).
CREATE TABLE locations (
    id            uuid        PRIMARY KEY DEFAULT uuidv7(),
    company_id    uuid        NOT NULL REFERENCES companies (id) ON DELETE CASCADE,
    zone_id       uuid        NOT NULL REFERENCES world_zones (id) ON DELETE CASCADE,
    code          text        NOT NULL,
    name          text        NOT NULL,
    kind          text        NOT NULL,
    department_id uuid        REFERENCES departments (id) ON DELETE SET NULL,
    description   text        NOT NULL DEFAULT '',
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT locations_company_code_key UNIQUE (company_id, code),
    CONSTRAINT locations_code_chk CHECK (code ~ '^[A-Z][A-Z0-9_]{0,63}$'),
    CONSTRAINT locations_kind_chk CHECK (kind IN (
        'lobby', 'reception', 'office', 'open_office', 'meeting_room', 'soc',
        'lab', 'server_room', 'datacenter', 'common_area', 'corridor'))
);
CREATE INDEX locations_zone_idx ON locations (zone_id);
CREATE TRIGGER locations_set_updated_at BEFORE UPDATE ON locations
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE employees (
    id               uuid        PRIMARY KEY DEFAULT uuidv7(),
    company_id       uuid        NOT NULL REFERENCES companies (id) ON DELETE CASCADE,
    employee_code    text        NOT NULL,
    first_name       text        NOT NULL,
    last_name        text        NOT NULL,
    display_name     text        NOT NULL,
    job_title        text        NOT NULL,
    department_id    uuid        NOT NULL REFERENCES departments (id),
    manager_id       uuid        REFERENCES employees (id) ON DELETE SET NULL,
    home_location_id uuid        REFERENCES locations (id) ON DELETE SET NULL,
    employment_type  text        NOT NULL DEFAULT 'full_time',
    status           text        NOT NULL DEFAULT 'active',
    -- Represented in the world as a deterministic NPC.
    is_npc           boolean     NOT NULL DEFAULT true,
    persona          text        NOT NULL DEFAULT '',
    greeting         text        NOT NULL DEFAULT '',
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT employees_company_code_key UNIQUE (company_id, employee_code),
    CONSTRAINT employees_code_chk CHECK (employee_code ~ '^[A-Z][A-Z0-9-]{1,31}$'),
    CONSTRAINT employees_employment_type_chk CHECK (employment_type IN ('full_time', 'part_time', 'contractor')),
    CONSTRAINT employees_status_chk CHECK (status IN ('active', 'on_leave', 'terminated')),
    CONSTRAINT employees_not_own_manager_chk CHECK (manager_id IS DISTINCT FROM id)
);
CREATE INDEX employees_department_idx ON employees (department_id);
CREATE INDEX employees_manager_idx ON employees (manager_id);
CREATE INDEX employees_home_location_idx ON employees (home_location_id);
CREATE TRIGGER employees_set_updated_at BEFORE UPDATE ON employees
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

ALTER TABLE departments
    ADD COLUMN head_employee_id uuid REFERENCES employees (id) ON DELETE SET NULL;

-- Weekly routine driving the deterministic NPC scheduler.
CREATE TABLE employee_schedules (
    id          uuid    PRIMARY KEY DEFAULT uuidv7(),
    employee_id uuid    NOT NULL REFERENCES employees (id) ON DELETE CASCADE,
    weekday     integer NOT NULL, -- ISO 8601: 1 = Monday … 7 = Sunday
    starts_at   time    NOT NULL,
    ends_at     time    NOT NULL,
    location_id uuid    REFERENCES locations (id) ON DELETE SET NULL,
    activity    text    NOT NULL,

    CONSTRAINT employee_schedules_weekday_chk CHECK (weekday BETWEEN 1 AND 7),
    CONSTRAINT employee_schedules_window_chk CHECK (ends_at > starts_at),
    CONSTRAINT employee_schedules_activity_chk CHECK (activity IN ('working', 'meeting', 'break', 'offsite', 'on_call'))
);
CREATE INDEX employee_schedules_employee_idx ON employee_schedules (employee_id, weekday, starts_at);

-- Fictional directory accounts inside the simulated enterprise.
CREATE TABLE enterprise_identities (
    id            uuid        PRIMARY KEY DEFAULT uuidv7(),
    company_id    uuid        NOT NULL REFERENCES companies (id) ON DELETE CASCADE,
    employee_id   uuid        REFERENCES employees (id) ON DELETE SET NULL, -- NULL for service accounts
    username      text        NOT NULL,
    email         text        NOT NULL,
    identity_type text        NOT NULL,
    status        text        NOT NULL DEFAULT 'enabled',
    mfa_enabled   boolean     NOT NULL DEFAULT false,
    privileged    boolean     NOT NULL DEFAULT false,
    expires_at    timestamptz,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT enterprise_identities_company_username_key UNIQUE (company_id, username),
    CONSTRAINT enterprise_identities_company_email_key UNIQUE (company_id, email),
    CONSTRAINT enterprise_identities_username_chk CHECK (username ~ '^[a-z][a-z0-9._-]{0,63}$'),
    CONSTRAINT enterprise_identities_type_chk CHECK (identity_type IN ('user', 'admin', 'service', 'contractor')),
    CONSTRAINT enterprise_identities_status_chk CHECK (status IN ('enabled', 'disabled', 'locked', 'expired')),
    CONSTRAINT enterprise_identities_service_chk CHECK (identity_type <> 'service' OR employee_id IS NULL)
);
CREATE INDEX enterprise_identities_employee_idx ON enterprise_identities (employee_id);
CREATE TRIGGER enterprise_identities_set_updated_at BEFORE UPDATE ON enterprise_identities
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- Digital assets: devices, servers, applications, networks, groups, cloud.
CREATE TABLE assets (
    id                uuid        PRIMARY KEY DEFAULT uuidv7(),
    company_id        uuid        NOT NULL REFERENCES companies (id) ON DELETE CASCADE,
    asset_code        text        NOT NULL,
    name              text        NOT NULL,
    asset_type        text        NOT NULL,
    hostname          text,
    ip_address        inet,
    network_cidr      cidr,
    operating_system  text,
    criticality       text        NOT NULL DEFAULT 'medium',
    status            text        NOT NULL DEFAULT 'active',
    department_id     uuid        REFERENCES departments (id) ON DELETE SET NULL,
    owner_employee_id uuid        REFERENCES employees (id) ON DELETE SET NULL,
    location_id       uuid        REFERENCES locations (id) ON DELETE SET NULL,
    description       text        NOT NULL DEFAULT '',
    attributes        jsonb       NOT NULL DEFAULT '{}'::jsonb,
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT assets_company_code_key UNIQUE (company_id, asset_code),
    CONSTRAINT assets_code_chk CHECK (asset_code ~ '^[A-Z][A-Z0-9-]{1,63}$'),
    CONSTRAINT assets_type_chk CHECK (asset_type IN (
        'workstation', 'laptop', 'server', 'virtual_machine', 'network_device',
        'firewall', 'network', 'application', 'database', 'identity_group',
        'cloud_resource', 'printer', 'iot_device', 'mobile_device', 'storage')),
    CONSTRAINT assets_hostname_chk CHECK (hostname IS NULL OR hostname ~ '^[A-Za-z0-9][A-Za-z0-9.-]{0,252}$'),
    CONSTRAINT assets_network_cidr_chk CHECK (network_cidr IS NULL OR asset_type = 'network'),
    CONSTRAINT assets_criticality_chk CHECK (criticality IN ('low', 'medium', 'high', 'critical')),
    CONSTRAINT assets_status_chk CHECK (status IN ('active', 'maintenance', 'decommissioned')),
    CONSTRAINT assets_attributes_object_chk CHECK (jsonb_typeof(attributes) = 'object')
);
CREATE UNIQUE INDEX assets_company_hostname_idx ON assets (company_id, lower(hostname)) WHERE hostname IS NOT NULL;
CREATE UNIQUE INDEX assets_company_ip_idx ON assets (company_id, ip_address) WHERE ip_address IS NOT NULL;
CREATE INDEX assets_company_type_idx ON assets (company_id, asset_type, asset_code);
CREATE INDEX assets_department_idx ON assets (department_id);
CREATE INDEX assets_owner_idx ON assets (owner_employee_id);
CREATE INDEX assets_location_idx ON assets (location_id);
CREATE TRIGGER assets_set_updated_at BEFORE UPDATE ON assets
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- Interactive objects in the 3D scene, keyed by their Three.js object name.
-- asset_id is the physical → digital mapping behind the Digital Twin.
CREATE TABLE world_objects (
    id               uuid        PRIMARY KEY DEFAULT uuidv7(),
    company_id       uuid        NOT NULL REFERENCES companies (id) ON DELETE CASCADE,
    location_id      uuid        NOT NULL REFERENCES locations (id) ON DELETE CASCADE,
    object_key       text        NOT NULL,
    name             text        NOT NULL,
    kind             text        NOT NULL,
    asset_id         uuid        REFERENCES assets (id) ON DELETE SET NULL,
    -- Doors: the zone entered through this object.
    leads_to_zone_id uuid        REFERENCES world_zones (id) ON DELETE SET NULL,
    interactions     text[]      NOT NULL DEFAULT '{}',
    description      text        NOT NULL DEFAULT '',
    -- Readable text for documents and whiteboards.
    content          text        NOT NULL DEFAULT '',
    attributes       jsonb       NOT NULL DEFAULT '{}'::jsonb,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT world_objects_company_key_key UNIQUE (company_id, object_key),
    CONSTRAINT world_objects_key_chk CHECK (object_key ~ '^[a-z0-9][a-z0-9_.-]{0,127}$'),
    CONSTRAINT world_objects_kind_chk CHECK (kind IN (
        'workstation', 'server_rack', 'door', 'terminal', 'document', 'badge_reader',
        'printer', 'whiteboard', 'screen', 'npc_spawn', 'furniture')),
    CONSTRAINT world_objects_interactions_chk CHECK (interactions <@ ARRAY[
        'OPEN_DOOR', 'CLOSE_DOOR', 'USE_WORKSTATION', 'INSPECT_OBJECT',
        'ACCESS_TERMINAL', 'READ_DOCUMENT', 'START_ENGAGEMENT']::text[]),
    CONSTRAINT world_objects_door_chk CHECK (kind <> 'door' OR leads_to_zone_id IS NOT NULL),
    CONSTRAINT world_objects_attributes_object_chk CHECK (jsonb_typeof(attributes) = 'object')
);
CREATE INDEX world_objects_location_idx ON world_objects (location_id);
CREATE INDEX world_objects_asset_idx ON world_objects (asset_id);
CREATE TRIGGER world_objects_set_updated_at BEFORE UPDATE ON world_objects
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- Digital-twin graph edges. Each endpoint is exactly one of employee,
-- enterprise identity or asset, with real foreign keys for each kind.
CREATE TABLE asset_relationships (
    id                 uuid        PRIMARY KEY DEFAULT uuidv7(),
    company_id         uuid        NOT NULL REFERENCES companies (id) ON DELETE CASCADE,
    relationship_type  text        NOT NULL,
    source_employee_id uuid        REFERENCES employees (id) ON DELETE CASCADE,
    source_identity_id uuid        REFERENCES enterprise_identities (id) ON DELETE CASCADE,
    source_asset_id    uuid        REFERENCES assets (id) ON DELETE CASCADE,
    target_employee_id uuid        REFERENCES employees (id) ON DELETE CASCADE,
    target_identity_id uuid        REFERENCES enterprise_identities (id) ON DELETE CASCADE,
    target_asset_id    uuid        REFERENCES assets (id) ON DELETE CASCADE,
    created_at         timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT asset_relationships_type_chk CHECK (relationship_type IN (
        'OWNS', 'USES', 'MEMBER_OF', 'CONNECTED_TO', 'HOSTED_ON', 'DEPENDS_ON',
        'HAS_ACCESS_TO', 'MANAGES', 'LOCATED_IN', 'AUTHENTICATES_TO', 'HAS_IDENTITY')),
    CONSTRAINT asset_relationships_one_source_chk
        CHECK (num_nonnulls(source_employee_id, source_identity_id, source_asset_id) = 1),
    CONSTRAINT asset_relationships_one_target_chk
        CHECK (num_nonnulls(target_employee_id, target_identity_id, target_asset_id) = 1),
    CONSTRAINT asset_relationships_unique_key UNIQUE NULLS NOT DISTINCT (
        company_id, relationship_type,
        source_employee_id, source_identity_id, source_asset_id,
        target_employee_id, target_identity_id, target_asset_id)
);
CREATE INDEX asset_relationships_company_idx ON asset_relationships (company_id);
CREATE INDEX asset_relationships_source_employee_idx ON asset_relationships (source_employee_id) WHERE source_employee_id IS NOT NULL;
CREATE INDEX asset_relationships_source_identity_idx ON asset_relationships (source_identity_id) WHERE source_identity_id IS NOT NULL;
CREATE INDEX asset_relationships_source_asset_idx ON asset_relationships (source_asset_id) WHERE source_asset_id IS NOT NULL;
CREATE INDEX asset_relationships_target_employee_idx ON asset_relationships (target_employee_id) WHERE target_employee_id IS NOT NULL;
CREATE INDEX asset_relationships_target_identity_idx ON asset_relationships (target_identity_id) WHERE target_identity_id IS NOT NULL;
CREATE INDEX asset_relationships_target_asset_idx ON asset_relationships (target_asset_id) WHERE target_asset_id IS NOT NULL;

-- +goose Down
DROP TABLE IF EXISTS asset_relationships;
DROP TABLE IF EXISTS world_objects;
DROP TABLE IF EXISTS assets;
DROP TABLE IF EXISTS enterprise_identities;
DROP TABLE IF EXISTS employee_schedules;
ALTER TABLE IF EXISTS departments DROP COLUMN IF EXISTS head_employee_id;
DROP TABLE IF EXISTS employees;
DROP TABLE IF EXISTS locations;
DROP TABLE IF EXISTS world_zones;
DROP TABLE IF EXISTS departments;
DROP TABLE IF EXISTS companies;
DROP TABLE IF EXISTS scenarios;
