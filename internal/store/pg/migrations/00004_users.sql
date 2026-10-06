-- DESIGN §4 and §3.2: users, devices, tokens (stored as sha256 only), per-user item state.

-- +goose Up
CREATE TABLE users (
    id            uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    name          citext      NOT NULL UNIQUE,
    password_hash text,        -- NULL = passwordless user (Jellyfin allows these)
    is_admin      boolean     NOT NULL DEFAULT false,
    is_disabled   boolean     NOT NULL DEFAULT false,
    policy        jsonb       NOT NULL DEFAULT '{}',
    configuration jsonb       NOT NULL DEFAULT '{}',
    created_at    timestamptz NOT NULL DEFAULT now(),
    last_login_at timestamptz
);

-- A client's DeviceId. Like Jellyfin, one device belongs to whichever user last
-- signed in on it.
CREATE TABLE devices (
    id           text        PRIMARY KEY,
    user_id      uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    name         text        NOT NULL DEFAULT '',
    app_name     text        NOT NULL DEFAULT '',
    app_version  text        NOT NULL DEFAULT '',
    last_seen_at timestamptz NOT NULL DEFAULT now(),
    capabilities jsonb       NOT NULL DEFAULT '{}'
);
CREATE INDEX devices_user_idx ON devices (user_id);

CREATE TABLE access_tokens (
    token_sha    bytea       PRIMARY KEY CHECK (octet_length(token_sha) = 32),
    user_id      uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    device_id    text        NOT NULL REFERENCES devices (id) ON DELETE CASCADE,
    created_at   timestamptz NOT NULL DEFAULT now(),
    last_used_at timestamptz NOT NULL DEFAULT now(),
    revoked_at   timestamptz
);
CREATE INDEX access_tokens_user_idx   ON access_tokens (user_id);
CREATE INDEX access_tokens_device_idx ON access_tokens (device_id);

CREATE TABLE user_data (
    user_id                 uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    item_id                 uuid        NOT NULL REFERENCES items (id) ON DELETE CASCADE,
    played                  boolean     NOT NULL DEFAULT false,
    play_count              int         NOT NULL DEFAULT 0,
    playback_position_ticks bigint      NOT NULL DEFAULT 0,
    is_favorite             boolean     NOT NULL DEFAULT false,
    rating                  real,
    last_played_at          timestamptz,
    audio_stream_idx        int,
    subtitle_stream_idx     int,
    updated_at              timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, item_id)
);
-- Continue Watching (/UserItems/Resume) and favourites filters.
CREATE INDEX user_data_resume_idx    ON user_data (user_id, last_played_at DESC) WHERE playback_position_ticks > 0;
CREATE INDEX user_data_favorites_idx ON user_data (user_id) WHERE is_favorite;

CREATE TABLE display_preferences (
    user_id    uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    pref_id    text        NOT NULL,   -- e.g. "usersettings"
    client     text        NOT NULL,
    data       jsonb       NOT NULL DEFAULT '{}',
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, pref_id, client)
);

-- +goose Down
DROP TABLE IF EXISTS display_preferences;
DROP TABLE IF EXISTS user_data;
DROP TABLE IF EXISTS access_tokens;
DROP TABLE IF EXISTS devices;
DROP TABLE IF EXISTS users;
