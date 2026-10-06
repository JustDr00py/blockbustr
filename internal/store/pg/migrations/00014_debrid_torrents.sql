-- Torrents blockbustr added to a debrid account (TASKS P3.8, DESIGN §7.3):
-- an infoHash stream played once is found again instead of being added
-- again (Real-Debrid keeps duplicates), and its last known status tells
-- the stream ranking it is cached. Rows are keyed by provider, not account
-- id, like the resolver; a deleted account's rows are dropped with it.

-- +goose Up
CREATE TABLE debrid_torrents (
  provider text NOT NULL REFERENCES debrid_accounts (provider) ON DELETE CASCADE,
  info_hash text NOT NULL CHECK (info_hash = lower(info_hash)),
  torrent_id text NOT NULL,
  status text NOT NULL,
  added_at timestamptz NOT NULL DEFAULT now(),
  checked_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (provider, info_hash)
);
CREATE INDEX debrid_torrents_hash ON debrid_torrents (info_hash);

-- +goose Down
DROP TABLE debrid_torrents;
