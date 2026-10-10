-- Bytes blockbustr proxied from remote sources, per calendar month (UTC), for
-- the admin UI's Stats page. The in-memory counter behind /metrics resets on
-- every restart; this is what survives. month is the first day of the month.

-- +goose Up
CREATE TABLE proxied_bytes_monthly (
  month date   PRIMARY KEY,
  bytes bigint NOT NULL DEFAULT 0
);

-- +goose Down
DROP TABLE proxied_bytes_monthly;
