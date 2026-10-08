-- name: GetSetting :one
SELECT value FROM server_settings WHERE key = $1;

-- name: InsertSettingIfMissing :exec
-- First writer wins, so concurrent starts agree on generated values.
INSERT INTO server_settings (key, value) VALUES ($1, $2)
ON CONFLICT (key) DO NOTHING;

-- name: ListAdminSettings :many
-- The admin UI's settings (internal/settings), by their config keys.
SELECT key, value FROM server_settings WHERE key = ANY (@keys::text[]) ORDER BY key;

-- name: UpsertSetting :exec
INSERT INTO server_settings (key, value) VALUES (@key, @value)
ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = now();

-- name: DeleteSetting :exec
DELETE FROM server_settings WHERE key = @key;
