-- Server-wide settings as small JSON values: the stable server id now,
-- branding and other admin settings later.

-- +goose Up
CREATE TABLE server_settings (
    key        text        PRIMARY KEY,
    value      jsonb       NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE IF EXISTS server_settings;
