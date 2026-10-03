-- name: InsertNotification :one
INSERT INTO notifications (user_id, type, title, body, data)
VALUES (@user_id, @type, @title, @body, @data)
RETURNING id, type, title, body, data, read_at, created_at;

-- name: ListNotifications :many
-- Newest first; pass the last id of the previous page as before_id.
SELECT id, type, title, body, data, read_at, created_at
FROM notifications
WHERE user_id = @user_id
  AND (NOT @unread_only::boolean OR read_at IS NULL)
  AND (sqlc.narg(before_id)::uuid IS NULL OR id < sqlc.narg(before_id)::uuid)
ORDER BY id DESC
LIMIT @page_size;

-- name: CountUnreadNotifications :one
SELECT count(*)::int FROM notifications WHERE user_id = @user_id AND read_at IS NULL;

-- name: MarkNotificationRead :one
-- Scoped by user: a user can never mark someone else's notification.
UPDATE notifications SET read_at = COALESCE(read_at, now())
WHERE id = @id AND user_id = @user_id
RETURNING id, read_at;

-- name: MarkAllNotificationsRead :execrows
UPDATE notifications SET read_at = now() WHERE user_id = @user_id AND read_at IS NULL;
