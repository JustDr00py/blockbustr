-- name: GetSetting :one
SELECT value FROM server_settings WHERE key = $1;

-- name: InsertSettingIfMissing :exec
-- First writer wins, so concurrent starts agree on generated values.
INSERT INTO server_settings (key, value) VALUES ($1, $2)
ON CONFLICT (key) DO NOTHING;
