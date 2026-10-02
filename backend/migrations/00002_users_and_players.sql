-- Real platform identities (mirrored from Keycloak) and their game profiles.
--
-- users mirrors just enough of a Keycloak account to attribute actions and
-- display a profile. Keycloak stays authoritative for credentials, MFA and
-- platform roles; no password or token is ever stored here.
--
-- Fictional enterprise identities (alex@nexora.local) are game data and
-- live in separate tables (phase 3), never here.

-- +goose Up
CREATE TABLE users (
    id               uuid        PRIMARY KEY DEFAULT uuidv7(),
    -- Keycloak "sub": stable for the lifetime of the account, unlike the
    -- username or email, and independent of Keycloak's public hostname.
    keycloak_subject text        NOT NULL,
    username         text        NOT NULL,
    email            text,
    email_verified   boolean     NOT NULL DEFAULT false,
    full_name        text,
    status           text        NOT NULL DEFAULT 'active',
    last_seen_at     timestamptz NOT NULL DEFAULT now(),
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT users_keycloak_subject_key UNIQUE (keycloak_subject),
    CONSTRAINT users_keycloak_subject_len_chk CHECK (char_length(keycloak_subject) BETWEEN 1 AND 255),
    CONSTRAINT users_username_len_chk CHECK (char_length(username) BETWEEN 1 AND 255),
    CONSTRAINT users_email_len_chk CHECK (email IS NULL OR char_length(email) <= 320),
    CONSTRAINT users_full_name_len_chk CHECK (full_name IS NULL OR char_length(full_name) <= 255),
    CONSTRAINT users_status_chk CHECK (status IN ('active', 'suspended'))
);

-- Not unique: Keycloak enforces email uniqueness, and this mirror can be
-- briefly stale when two accounts swap addresses.
CREATE INDEX users_email_idx ON users (lower(email)) WHERE email IS NOT NULL;

CREATE TRIGGER users_set_updated_at
    BEFORE UPDATE ON users
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- Game profile of a user holding the PLAYER role. Progression and game
-- permissions (unlocked zones, career level) attach to players in later
-- migrations.
CREATE TABLE players (
    id           uuid        PRIMARY KEY DEFAULT uuidv7(),
    user_id      uuid        NOT NULL REFERENCES users (id) ON DELETE RESTRICT,
    display_name text        NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT players_user_id_key UNIQUE (user_id),
    CONSTRAINT players_display_name_len_chk CHECK (char_length(display_name) BETWEEN 1 AND 64)
);

CREATE TRIGGER players_set_updated_at
    BEFORE UPDATE ON players
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- +goose Down
DROP TABLE IF EXISTS players;
DROP TABLE IF EXISTS users;
