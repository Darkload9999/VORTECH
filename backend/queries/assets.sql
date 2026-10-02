-- name: ListAssets :many
-- Keyset pagination on asset_code.
SELECT a.id, a.asset_code, a.name, a.asset_type, a.hostname, a.ip_address, a.network_cidr,
       a.criticality, a.status,
       d.code AS department_code,
       o.employee_code AS owner_code,
       l.code AS location_code
FROM assets a
LEFT JOIN departments d ON d.id = a.department_id
LEFT JOIN employees o ON o.id = a.owner_employee_id
LEFT JOIN locations l ON l.id = a.location_id
WHERE a.company_id = @company_id
  AND (sqlc.narg(asset_type)::text IS NULL OR a.asset_type = sqlc.narg(asset_type)::text)
  AND (sqlc.narg(department)::text IS NULL OR d.code = sqlc.narg(department)::text)
  AND (sqlc.narg(search)::text IS NULL
       OR a.name ILIKE sqlc.narg(search)::text
       OR a.asset_code ILIKE sqlc.narg(search)::text
       OR a.hostname ILIKE sqlc.narg(search)::text)
  AND (sqlc.narg(after)::text IS NULL OR a.asset_code > sqlc.narg(after)::text)
ORDER BY a.asset_code
LIMIT @page_size;

-- name: GetAsset :one
SELECT a.id, a.asset_code, a.name, a.asset_type, a.hostname, a.ip_address, a.network_cidr,
       a.operating_system, a.criticality, a.status, a.description, a.attributes,
       d.id AS department_id, d.code AS department_code, d.name AS department_name,
       o.id AS owner_id, o.employee_code AS owner_code, o.display_name AS owner_name,
       l.id AS location_id, l.code AS location_code, l.name AS location_name,
       z.id AS zone_id, z.code AS zone_code
FROM assets a
LEFT JOIN departments d ON d.id = a.department_id
LEFT JOIN employees o ON o.id = a.owner_employee_id
LEFT JOIN locations l ON l.id = a.location_id
LEFT JOIN world_zones z ON z.id = l.zone_id
WHERE a.company_id = @company_id AND a.id = @id;

-- name: ListAssetWorldObjects :many
SELECT object_key, name, kind FROM world_objects
WHERE company_id = @company_id AND asset_id = @asset_id
ORDER BY object_key;

-- name: ListCompanyRelationships :many
SELECT id, relationship_type,
       source_employee_id, source_identity_id, source_asset_id,
       target_employee_id, target_identity_id, target_asset_id
FROM asset_relationships
WHERE company_id = @company_id
ORDER BY id;

-- name: ListGraphEmployees :many
SELECT e.id, e.employee_code, e.display_name, e.job_title, d.code AS department_code
FROM employees e JOIN departments d ON d.id = e.department_id
WHERE e.company_id = @company_id;

-- name: ListGraphIdentities :many
SELECT id, username, email, identity_type, privileged, status
FROM enterprise_identities
WHERE company_id = @company_id;

-- name: ListGraphAssets :many
SELECT id, asset_code, name, asset_type, hostname, criticality
FROM assets
WHERE company_id = @company_id;

-- name: ListAssetNetworks :many
-- Networks the asset is connected to (CONNECTED_TO edges to network assets).
SELECT n.id, n.asset_code, n.name, n.network_cidr
FROM asset_relationships r
JOIN assets n ON n.id = r.target_asset_id
WHERE r.company_id = @company_id
  AND r.source_asset_id = @asset_id
  AND r.relationship_type = 'CONNECTED_TO'
  AND n.asset_type = 'network'
ORDER BY n.asset_code;
