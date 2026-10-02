-- name: ListZones :many
SELECT z.id, z.code, z.name, z.kind, z.description, z.unlocked_by_default, z.required_career_rank,
       p.id AS parent_id, p.code AS parent_code
FROM world_zones z
LEFT JOIN world_zones p ON p.id = z.parent_id
WHERE z.company_id = @company_id
ORDER BY z.sort_order, z.code;

-- name: GetZone :one
SELECT z.id, z.code, z.name, z.kind, z.description, z.unlocked_by_default, z.required_career_rank,
       p.id AS parent_id, p.code AS parent_code
FROM world_zones z
LEFT JOIN world_zones p ON p.id = z.parent_id
WHERE z.company_id = @company_id AND z.id = @id;

-- name: ListZoneLocations :many
SELECT l.id, l.code, l.name, l.kind, l.description, d.code AS department_code
FROM locations l
LEFT JOIN departments d ON d.id = l.department_id
WHERE l.company_id = @company_id AND l.zone_id = @zone_id
ORDER BY l.code;

-- name: ListZoneObjects :many
SELECT o.id, o.object_key, o.name, o.kind, o.interactions, o.description,
       l.code AS location_code,
       a.id AS asset_id, a.asset_code, a.name AS asset_name, a.asset_type, a.hostname, a.ip_address,
       t.id AS leads_to_zone_id, t.code AS leads_to_zone_code
FROM world_objects o
JOIN locations l ON l.id = o.location_id
LEFT JOIN assets a ON a.id = o.asset_id
LEFT JOIN world_zones t ON t.id = o.leads_to_zone_id
WHERE o.company_id = @company_id AND l.zone_id = @zone_id
ORDER BY o.object_key;

-- name: ListZoneEmployees :many
-- NPCs whose home location is in the zone. The living-company scheduler
-- will move them during the day in a later phase.
SELECT e.id, e.employee_code, e.display_name, e.job_title, e.greeting, l.code AS location_code
FROM employees e
JOIN locations l ON l.id = e.home_location_id
WHERE e.company_id = @company_id AND l.zone_id = @zone_id AND e.is_npc AND e.status = 'active'
ORDER BY e.employee_code;

-- name: GetWorldObjectByKey :one
SELECT o.id, o.object_key, o.name, o.kind, o.interactions, o.description, o.attributes,
       l.id AS location_id, l.code AS location_code, l.name AS location_name,
       z.id AS zone_id, z.code AS zone_code, z.name AS zone_name,
       t.id AS leads_to_zone_id, t.code AS leads_to_zone_code,
       a.id AS asset_id, a.asset_code, a.name AS asset_name, a.asset_type, a.hostname, a.ip_address,
       a.operating_system, a.criticality,
       ad.code AS asset_department_code,
       ao.employee_code AS asset_owner_code, ao.display_name AS asset_owner_name
FROM world_objects o
JOIN locations l ON l.id = o.location_id
JOIN world_zones z ON z.id = l.zone_id
LEFT JOIN world_zones t ON t.id = o.leads_to_zone_id
LEFT JOIN assets a ON a.id = o.asset_id
LEFT JOIN departments ad ON ad.id = a.department_id
LEFT JOIN employees ao ON ao.id = a.owner_employee_id
WHERE o.company_id = @company_id AND o.object_key = @object_key;
