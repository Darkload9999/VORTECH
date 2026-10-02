-- Write side of the scenario importer. Entities are upserted by natural key
-- so their IDs survive re-imports; anything no longer present in the
-- scenario files is pruned with the Delete* queries.

-- name: GetScenarioBySlug :one
SELECT * FROM scenarios WHERE slug = @slug;

-- name: UpsertScenario :one
INSERT INTO scenarios (slug, name, version, description, content_hash)
VALUES (@slug, @name, @version, @description, @content_hash)
ON CONFLICT (slug) DO UPDATE
SET name         = EXCLUDED.name,
    version      = EXCLUDED.version,
    description  = EXCLUDED.description,
    content_hash = EXCLUDED.content_hash,
    imported_at  = now()
RETURNING id, (xmax = 0)::boolean AS inserted;

-- name: DeactivateOtherScenarios :exec
UPDATE scenarios SET is_active = false WHERE is_active AND id <> @id;

-- name: PublishAndActivateScenario :exec
UPDATE scenarios
SET status       = 'published',
    published_at = COALESCE(published_at, now()),
    is_active    = true
WHERE id = @id;

-- name: UpsertCompany :one
INSERT INTO companies (scenario_id, code, name, legal_name, industry, description, headquarters, founded_year, email_domain)
VALUES (@scenario_id, @code, @name, @legal_name, @industry, @description, @headquarters, sqlc.narg(founded_year), @email_domain)
ON CONFLICT (scenario_id) DO UPDATE
SET code         = EXCLUDED.code,
    name         = EXCLUDED.name,
    legal_name   = EXCLUDED.legal_name,
    industry     = EXCLUDED.industry,
    description  = EXCLUDED.description,
    headquarters = EXCLUDED.headquarters,
    founded_year = EXCLUDED.founded_year,
    email_domain = EXCLUDED.email_domain
RETURNING id;

-- name: UpsertDepartment :one
INSERT INTO departments (company_id, code, name, description, sort_order)
VALUES (@company_id, @code, @name, @description, @sort_order)
ON CONFLICT (company_id, code) DO UPDATE
SET name = EXCLUDED.name, description = EXCLUDED.description, sort_order = EXCLUDED.sort_order
RETURNING id;

-- name: SetDepartmentLinks :exec
UPDATE departments
SET parent_id = sqlc.narg(parent_id), head_employee_id = sqlc.narg(head_employee_id)
WHERE id = @id;

-- name: DeleteStaleDepartments :execrows
DELETE FROM departments WHERE company_id = @company_id AND NOT (code = ANY(@keep::text[]));

-- name: UpsertZone :one
INSERT INTO world_zones (company_id, code, name, kind, description, unlocked_by_default, required_career_rank, sort_order)
VALUES (@company_id, @code, @name, @kind, @description, @unlocked_by_default, @required_career_rank, @sort_order)
ON CONFLICT (company_id, code) DO UPDATE
SET name                 = EXCLUDED.name,
    kind                 = EXCLUDED.kind,
    description          = EXCLUDED.description,
    unlocked_by_default  = EXCLUDED.unlocked_by_default,
    required_career_rank = EXCLUDED.required_career_rank,
    sort_order           = EXCLUDED.sort_order
RETURNING id;

-- name: SetZoneParent :exec
UPDATE world_zones SET parent_id = sqlc.narg(parent_id) WHERE id = @id;

-- name: DeleteStaleZones :execrows
DELETE FROM world_zones WHERE company_id = @company_id AND NOT (code = ANY(@keep::text[]));

-- name: UpsertLocation :one
INSERT INTO locations (company_id, zone_id, code, name, kind, department_id, description)
VALUES (@company_id, @zone_id, @code, @name, @kind, sqlc.narg(department_id), @description)
ON CONFLICT (company_id, code) DO UPDATE
SET zone_id       = EXCLUDED.zone_id,
    name          = EXCLUDED.name,
    kind          = EXCLUDED.kind,
    department_id = EXCLUDED.department_id,
    description   = EXCLUDED.description
RETURNING id;

-- name: DeleteStaleLocations :execrows
DELETE FROM locations WHERE company_id = @company_id AND NOT (code = ANY(@keep::text[]));

-- name: UpsertEmployee :one
INSERT INTO employees (
    company_id, employee_code, first_name, last_name, display_name, job_title,
    department_id, home_location_id, employment_type, status, is_npc, persona, greeting
) VALUES (
    @company_id, @employee_code, @first_name, @last_name, @display_name, @job_title,
    @department_id, sqlc.narg(home_location_id), @employment_type, @status, @is_npc, @persona, @greeting
)
ON CONFLICT (company_id, employee_code) DO UPDATE
SET first_name       = EXCLUDED.first_name,
    last_name        = EXCLUDED.last_name,
    display_name     = EXCLUDED.display_name,
    job_title        = EXCLUDED.job_title,
    department_id    = EXCLUDED.department_id,
    home_location_id = EXCLUDED.home_location_id,
    employment_type  = EXCLUDED.employment_type,
    status           = EXCLUDED.status,
    is_npc           = EXCLUDED.is_npc,
    persona          = EXCLUDED.persona,
    greeting         = EXCLUDED.greeting
RETURNING id;

-- name: SetEmployeeManager :exec
UPDATE employees SET manager_id = sqlc.narg(manager_id) WHERE id = @id;

-- name: DeleteStaleEmployees :execrows
DELETE FROM employees WHERE company_id = @company_id AND NOT (employee_code = ANY(@keep::text[]));

-- name: DeleteCompanySchedules :exec
DELETE FROM employee_schedules s
USING employees e
WHERE s.employee_id = e.id AND e.company_id = @company_id;

-- name: InsertSchedule :exec
INSERT INTO employee_schedules (employee_id, weekday, starts_at, ends_at, location_id, activity)
VALUES (@employee_id, @weekday, @starts_at, @ends_at, sqlc.narg(location_id), @activity);

-- name: UpsertIdentity :one
INSERT INTO enterprise_identities (
    company_id, employee_id, username, email, identity_type, status, mfa_enabled, privileged, expires_at
) VALUES (
    @company_id, sqlc.narg(employee_id), @username, @email, @identity_type, @status, @mfa_enabled, @privileged, sqlc.narg(expires_at)
)
ON CONFLICT (company_id, username) DO UPDATE
SET employee_id   = EXCLUDED.employee_id,
    email         = EXCLUDED.email,
    identity_type = EXCLUDED.identity_type,
    status        = EXCLUDED.status,
    mfa_enabled   = EXCLUDED.mfa_enabled,
    privileged    = EXCLUDED.privileged,
    expires_at    = EXCLUDED.expires_at
RETURNING id;

-- name: DeleteStaleIdentities :execrows
DELETE FROM enterprise_identities WHERE company_id = @company_id AND NOT (username = ANY(@keep::text[]));

-- name: UpsertAsset :one
INSERT INTO assets (
    company_id, asset_code, name, asset_type, hostname, ip_address, network_cidr, operating_system,
    criticality, status, department_id, owner_employee_id, location_id, description, attributes
) VALUES (
    @company_id, @asset_code, @name, @asset_type, sqlc.narg(hostname), sqlc.narg(ip_address), sqlc.narg(network_cidr),
    sqlc.narg(operating_system), @criticality, @status, sqlc.narg(department_id), sqlc.narg(owner_employee_id),
    sqlc.narg(location_id), @description, @attributes
)
ON CONFLICT (company_id, asset_code) DO UPDATE
SET name              = EXCLUDED.name,
    asset_type        = EXCLUDED.asset_type,
    hostname          = EXCLUDED.hostname,
    ip_address        = EXCLUDED.ip_address,
    network_cidr      = EXCLUDED.network_cidr,
    operating_system  = EXCLUDED.operating_system,
    criticality       = EXCLUDED.criticality,
    status            = EXCLUDED.status,
    department_id     = EXCLUDED.department_id,
    owner_employee_id = EXCLUDED.owner_employee_id,
    location_id       = EXCLUDED.location_id,
    description       = EXCLUDED.description,
    attributes        = EXCLUDED.attributes
RETURNING id;

-- name: ClearAssetAddresses :exec
-- Run before upserting so hostnames/IPs can move between assets within one
-- import without tripping the per-company unique indexes.
UPDATE assets SET hostname = NULL, ip_address = NULL
WHERE company_id = @company_id AND (hostname IS NOT NULL OR ip_address IS NOT NULL);

-- name: DeleteStaleAssets :execrows
DELETE FROM assets WHERE company_id = @company_id AND NOT (asset_code = ANY(@keep::text[]));

-- name: UpsertWorldObject :one
INSERT INTO world_objects (
    company_id, location_id, object_key, name, kind, asset_id, leads_to_zone_id,
    interactions, description, content, attributes
) VALUES (
    @company_id, @location_id, @object_key, @name, @kind, sqlc.narg(asset_id), sqlc.narg(leads_to_zone_id),
    @interactions, @description, @content, @attributes
)
ON CONFLICT (company_id, object_key) DO UPDATE
SET location_id      = EXCLUDED.location_id,
    name             = EXCLUDED.name,
    kind             = EXCLUDED.kind,
    asset_id         = EXCLUDED.asset_id,
    leads_to_zone_id = EXCLUDED.leads_to_zone_id,
    interactions     = EXCLUDED.interactions,
    description      = EXCLUDED.description,
    content          = EXCLUDED.content,
    attributes       = EXCLUDED.attributes
RETURNING id;

-- name: DeleteStaleWorldObjects :execrows
DELETE FROM world_objects WHERE company_id = @company_id AND NOT (object_key = ANY(@keep::text[]));

-- name: DeleteCompanyRelationships :exec
DELETE FROM asset_relationships WHERE company_id = @company_id;

-- name: InsertRelationship :exec
INSERT INTO asset_relationships (
    company_id, relationship_type,
    source_employee_id, source_identity_id, source_asset_id,
    target_employee_id, target_identity_id, target_asset_id
) VALUES (
    @company_id, @relationship_type,
    sqlc.narg(source_employee_id), sqlc.narg(source_identity_id), sqlc.narg(source_asset_id),
    sqlc.narg(target_employee_id), sqlc.narg(target_identity_id), sqlc.narg(target_asset_id)
)
ON CONFLICT ON CONSTRAINT asset_relationships_unique_key DO NOTHING;

-- name: LockScenarioImport :exec
-- Serialises concurrent imports of the same scenario for the transaction.
SELECT pg_advisory_xact_lock(hashtextextended('scenario-import:' || @slug::text, 0));

-- name: GetCompanyIDByScenario :one
SELECT id FROM companies WHERE scenario_id = @scenario_id;
