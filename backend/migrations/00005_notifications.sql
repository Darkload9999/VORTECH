-- Durable per-user notifications. WebSocket delivery is best-effort; this
-- table is the source of truth clients re-read after reconnecting.

-- +goose Up
CREATE TABLE notifications (
    id         uuid        PRIMARY KEY DEFAULT uuidv7(),
    user_id    uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    type       text        NOT NULL,
    title      text        NOT NULL,
    body       text        NOT NULL DEFAULT '',
    data       jsonb       NOT NULL DEFAULT '{}'::jsonb,
    read_at    timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT notifications_type_chk CHECK (type ~ '^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)+$'),
    CONSTRAINT notifications_title_chk CHECK (char_length(title) BETWEEN 1 AND 200),
    CONSTRAINT notifications_body_chk CHECK (char_length(body) <= 2000),
    CONSTRAINT notifications_data_object_chk CHECK (jsonb_typeof(data) = 'object')
);
-- UUIDv7 ids are time-ordered, so id DESC is newest first.
CREATE INDEX notifications_user_idx ON notifications (user_id, id DESC);
CREATE INDEX notifications_unread_idx ON notifications (user_id, id DESC) WHERE read_at IS NULL;

-- +goose Down
DROP TABLE IF EXISTS notifications;
