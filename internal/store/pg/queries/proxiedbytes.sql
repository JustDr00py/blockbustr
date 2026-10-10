-- name: AddProxiedBytes :exec
-- Adds to an hour's total (hour: its start).
INSERT INTO proxied_bytes_hourly (hour, bytes) VALUES (@hour, @bytes)
ON CONFLICT (hour) DO UPDATE SET bytes = proxied_bytes_hourly.bytes + EXCLUDED.bytes;

-- name: ListProxiedBytesByMonth :many
-- The newest calendar months first, as they fall in the time zone tz (an
-- IANA name).
SELECT to_char(date_trunc('month', hour AT TIME ZONE @tz::text), 'YYYY-MM')::text AS month,
       sum(bytes)::bigint AS bytes
FROM proxied_bytes_hourly
GROUP BY 1
ORDER BY 1 DESC
LIMIT @lim;
