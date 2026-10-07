-- name: UpsertDevice :exec
-- A device belongs to whichever user last signed in on it (DESIGN §4).
INSERT INTO devices (id, user_id, name, app_name, app_version)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (id) DO UPDATE
SET user_id      = EXCLUDED.user_id,
    name         = EXCLUDED.name,
    app_name     = EXCLUDED.app_name,
    app_version  = EXCLUDED.app_version,
    last_seen_at = now();

-- name: CreateAccessToken :exec
INSERT INTO access_tokens (token_sha, user_id, device_id) VALUES ($1, $2, $3);

-- name: GetTokenSession :one
-- Resolves a token (sha256) to its user and device. Revoked tokens and
-- disabled users don't resolve.
SELECT t.user_id, t.device_id, u.name AS user_name, u.is_admin, u.policy,
       d.name AS device_name, d.app_name, d.app_version
FROM access_tokens t
JOIN users   u ON u.id = t.user_id
JOIN devices d ON d.id = t.device_id
WHERE t.token_sha = $1
  AND t.revoked_at IS NULL
  AND NOT u.is_disabled;

-- name: TouchAccessToken :exec
UPDATE access_tokens SET last_used_at = now() WHERE token_sha = $1;

-- name: RevokeAccessToken :execrows
UPDATE access_tokens SET revoked_at = now() WHERE token_sha = $1 AND revoked_at IS NULL;

-- name: RevokeDeviceTokens :execrows
-- Jellyfin signs a device out of older sessions when it logs in again.
UPDATE access_tokens SET revoked_at = now() WHERE device_id = $1 AND revoked_at IS NULL;

-- name: RevokeUserTokens :many
-- Signs a user out everywhere (disabled, deleted): returns the devices, so
-- their cached sessions can be dropped too.
UPDATE access_tokens SET revoked_at = now() WHERE user_id = $1 AND revoked_at IS NULL
RETURNING device_id;

-- name: ActiveUserDevices :many
-- Devices with a live token of the user: their cached sessions are dropped
-- when the user's policy changes, so the change applies at once.
SELECT DISTINCT device_id FROM access_tokens WHERE user_id = $1 AND revoked_at IS NULL;
