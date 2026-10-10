-- Proxied bytes per hour instead of per month (00019), so the Stats page can
-- group them into months of whatever time zone the admin picks
-- (server.timezone). hour is the start of the UTC hour. A month already
-- stored lands on its first hour, which is all 00019 knew.

-- +goose Up
CREATE TABLE proxied_bytes_hourly (
  hour  timestamptz PRIMARY KEY,
  bytes bigint      NOT NULL DEFAULT 0
);
INSERT INTO proxied_bytes_hourly (hour, bytes)
SELECT month::timestamp AT TIME ZONE 'UTC', bytes FROM proxied_bytes_monthly;
DROP TABLE proxied_bytes_monthly;

-- +goose Down
CREATE TABLE proxied_bytes_monthly (
  month date   PRIMARY KEY,
  bytes bigint NOT NULL DEFAULT 0
);
INSERT INTO proxied_bytes_monthly (month, bytes)
SELECT (date_trunc('month', hour AT TIME ZONE 'UTC'))::date, sum(bytes) FROM proxied_bytes_hourly GROUP BY 1;
DROP TABLE proxied_bytes_hourly;
