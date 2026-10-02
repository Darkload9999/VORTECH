-- name: UpsertUserFromToken :one
-- Creates the user on first sight and refreshes the mirrored profile
-- otherwise. The update always runs (last_seen_at changes) so RETURNING
-- yields a row in both cases; xmax = 0 identifies a fresh insert.
INSERT INTO users (keycloak_subject, username, email, email_verified, full_name)
VALUES (@keycloak_subject, @username, sqlc.narg(email), @email_verified, sqlc.narg(full_name))
ON CONFLICT (keycloak_subject) DO UPDATE
SET username       = EXCLUDED.username,
    email          = EXCLUDED.email,
    email_verified = EXCLUDED.email_verified,
    full_name      = EXCLUDED.full_name,
    last_seen_at   = now()
RETURNING id, status, (xmax = 0)::boolean AS inserted;

-- name: InsertPlayerIfMissing :one
-- Returns the new player's id, or no rows if the user already has one.
INSERT INTO players (user_id, display_name)
VALUES (@user_id, @display_name)
ON CONFLICT (user_id) DO NOTHING
RETURNING id;

-- name: GetPlayerIDByUserID :one
SELECT id FROM players WHERE user_id = @user_id;

-- name: GetUserProfile :one
-- The user together with their player profile, if any.
SELECT
    u.id,
    u.username,
    u.email,
    u.email_verified,
    u.full_name,
    u.status,
    u.created_at,
    p.id           AS player_id,
    p.display_name AS player_display_name,
    p.created_at   AS player_created_at
FROM users u
LEFT JOIN players p ON p.user_id = u.id
WHERE u.id = @user_id;
