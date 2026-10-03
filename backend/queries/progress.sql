-- name: ListCareerLevels :many
SELECT rank, code, name, description, min_xp FROM career_levels ORDER BY rank;

-- name: EnsurePlayerProgress :exec
INSERT INTO player_progress (player_id) VALUES (@player_id) ON CONFLICT (player_id) DO NOTHING;

-- name: LockPlayerProgress :one
-- Serialises a player's state changes for the duration of the transaction.
SELECT player_id, xp, current_zone_id FROM player_progress WHERE player_id = @player_id FOR UPDATE;

-- name: GetPlayerProgress :one
SELECT player_id, xp, current_zone_id FROM player_progress WHERE player_id = @player_id;

-- name: SetCurrentZone :exec
UPDATE player_progress SET current_zone_id = sqlc.narg(zone_id) WHERE player_id = @player_id;

-- name: AddPlayerXP :one
UPDATE player_progress SET xp = xp + @xp WHERE player_id = @player_id RETURNING xp;

-- name: ListZoneUnlocks :many
SELECT u.zone_id, z.code AS zone_code, u.source, u.unlocked_at
FROM player_zone_unlocks u
JOIN world_zones z ON z.id = u.zone_id
WHERE u.player_id = @player_id AND z.company_id = @company_id
ORDER BY z.code;

-- name: GrantZoneUnlock :exec
INSERT INTO player_zone_unlocks (player_id, zone_id, source)
VALUES (@player_id, @zone_id, @source)
ON CONFLICT (player_id, zone_id) DO NOTHING;

-- name: InsertDiscovery :one
-- Returns no rows if the player already discovered the asset.
INSERT INTO discoveries (player_id, asset_id, object_id, discovered_via)
VALUES (@player_id, @asset_id, sqlc.narg(object_id), @discovered_via)
ON CONFLICT (player_id, asset_id) DO NOTHING
RETURNING asset_id;

-- name: CountDiscoveries :one
SELECT count(*)::int FROM discoveries d
JOIN assets a ON a.id = d.asset_id
WHERE d.player_id = @player_id AND a.company_id = @company_id;

-- name: InsertInteraction :one
INSERT INTO interactions (
    player_id, company_id, interaction_type, zone_id, object_id, employee_id,
    outcome, reason, xp_awarded, request_id
) VALUES (
    @player_id, @company_id, @interaction_type, sqlc.narg(zone_id), sqlc.narg(object_id), sqlc.narg(employee_id),
    @outcome, sqlc.narg(reason), @xp_awarded, sqlc.narg(request_id)
)
RETURNING id, created_at;

-- name: ListZoneRules :many
SELECT id, code, name, parent_id, unlocked_by_default, required_career_rank
FROM world_zones
WHERE company_id = @company_id;

-- name: GetInteractionObject :one
SELECT o.id, o.object_key, o.name, o.kind, o.interactions, o.description, o.content,
       l.zone_id, l.code AS location_code,
       o.leads_to_zone_id,
       a.id AS asset_id, a.asset_code, a.name AS asset_name, a.asset_type, a.hostname,
       a.ip_address, a.operating_system, a.criticality,
       ow.employee_code AS owner_code, ow.display_name AS owner_name
FROM world_objects o
JOIN locations l ON l.id = o.location_id
LEFT JOIN assets a ON a.id = o.asset_id
LEFT JOIN employees ow ON ow.id = a.owner_employee_id
WHERE o.company_id = @company_id AND o.object_key = @object_key;

-- name: GetInteractionNPC :one
SELECT e.id, e.employee_code, e.display_name, e.job_title, e.greeting, e.is_npc, e.status,
       l.zone_id
FROM employees e
LEFT JOIN locations l ON l.id = e.home_location_id
WHERE e.company_id = @company_id AND e.id = @id;
