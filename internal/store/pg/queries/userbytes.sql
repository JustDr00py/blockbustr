-- name: AddUserBytes :exec
-- Adds to a user's total for an hour (hour: its start).
INSERT INTO user_bytes_hourly (user_id, hour, bytes) VALUES (@user_id, @hour, @bytes)
ON CONFLICT (user_id, hour) DO UPDATE SET bytes = user_bytes_hourly.bytes + EXCLUDED.bytes;

-- name: UserBytesSince :one
-- A user's proxied bytes since a time (the start of their quota month).
SELECT coalesce(sum(bytes), 0)::bigint AS bytes
FROM user_bytes_hourly
WHERE user_id = @user_id AND hour >= @since;

-- name: UserBytesByUserSince :many
-- Every user's proxied bytes since a time, for the admin UI.
SELECT user_id, sum(bytes)::bigint AS bytes
FROM user_bytes_hourly
WHERE hour >= @since
GROUP BY user_id;

-- name: PruneUserBytes :execrows
DELETE FROM user_bytes_hourly WHERE hour < @before;
