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

-- name: UpdateUserPolicy :one
-- An admin's policy change (P4.1): the flags the database owns, and the
-- rest of the UserPolicy as stored overrides.
UPDATE users SET is_admin = @is_admin, is_disabled = @is_disabled, policy = @policy
WHERE id = @id
RETURNING *;

-- name: UpdateUserConfiguration :exec
-- The user's own settings (library order, hidden libraries, subtitle and
-- audio preferences), stored as overrides of Jellyfin's defaults.
UPDATE users SET configuration = @configuration WHERE id = @id;

-- name: SetUserPassword :exec
UPDATE users SET password_hash = $2 WHERE id = $1;

-- name: RenameUser :one
UPDATE users SET name = $2 WHERE id = $1 RETURNING *;

-- name: CountEnabledAdmins :one
SELECT count(*) FROM users WHERE is_admin AND NOT is_disabled;

-- name: DeleteUser :execrows
-- Tokens, devices and user data go with the user (ON DELETE CASCADE).
DELETE FROM users WHERE id = $1;
