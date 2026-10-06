-- name: CreateUser :one
INSERT INTO users (name, password_hash, is_admin)
VALUES ($1, $2, $3)
RETURNING *;

-- name: GetUserByID :one
SELECT * FROM users WHERE id = $1;

-- name: GetUserByName :one
-- users.name is citext, so this is case-insensitive like Jellyfin's login.
SELECT * FROM users WHERE name = $1;

-- name: ListUsers :many
SELECT * FROM users ORDER BY name;

-- name: CountUsers :one
SELECT count(*) FROM users;

-- name: SetUserLastLogin :exec
UPDATE users SET last_login_at = now() WHERE id = $1;

-- name: ResetAdminCredentials :exec
-- Bootstrap/recovery: (re)set the configured admin's password and make sure
-- it is an enabled administrator.
UPDATE users SET password_hash = $2, is_admin = true, is_disabled = false WHERE id = $1;
