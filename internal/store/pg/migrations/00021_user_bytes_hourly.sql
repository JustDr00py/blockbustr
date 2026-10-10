-- Bytes blockbustr proxied per user per hour, for monthly data quotas
-- (a user policy's MonthlyDataGB). Unlike the playback log's per-play
-- bytes, it also counts what no logged play claims: downloads, and streams
-- whose player never reported playing. hour is the start of the UTC hour.
-- Plays already logged seed it, so this month's usage starts out right.

-- +goose Up
CREATE TABLE user_bytes_hourly (
  user_id uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
  hour    timestamptz NOT NULL,
  bytes   bigint      NOT NULL DEFAULT 0,
  PRIMARY KEY (user_id, hour)
);
INSERT INTO user_bytes_hourly (user_id, hour, bytes)
SELECT user_id, date_trunc('hour', started_at), sum(bytes)
FROM playback_log
WHERE user_id IS NOT NULL AND bytes > 0
GROUP BY 1, 2;

-- +goose Down
DROP TABLE user_bytes_hourly;
