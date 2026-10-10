-- name: AddProxiedBytes :exec
-- Adds to a month's total (month: its first day).
INSERT INTO proxied_bytes_monthly (month, bytes) VALUES (@month, @bytes)
ON CONFLICT (month) DO UPDATE SET bytes = proxied_bytes_monthly.bytes + EXCLUDED.bytes;

-- name: ListProxiedBytes :many
-- The newest months first.
SELECT month, bytes FROM proxied_bytes_monthly ORDER BY month DESC LIMIT @lim;
