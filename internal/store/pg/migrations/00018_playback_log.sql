-- The playback log (admin UI's Playback page): one row per play, from the
-- Playing report or the first stream request of its play session, whichever
-- comes first, so admins can see who played what, from which addon version,
-- how it was delivered and how many bytes blockbustr proxied for it. Debrid
-- traffic of links an addon resolved with its own keys is only visible here
-- as these plays: the debrid service's own account page has the totals.
-- Names are copied in so the log outlives a deleted user or a removed item.

-- +goose Up
CREATE TABLE playback_log (
  id              bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  user_id         uuid        REFERENCES users (id) ON DELETE SET NULL,
  user_name       text        NOT NULL,
  device_name     text        NOT NULL DEFAULT '',
  client          text        NOT NULL DEFAULT '',
  item_id         uuid        NOT NULL,
  item_name       text        NOT NULL,
  runtime_ticks   bigint,
  play_session_id text,
  play_method     text        NOT NULL DEFAULT '',
  source_name     text        NOT NULL DEFAULT '', -- the version played
  addon           text        NOT NULL DEFAULT '', -- the addon that offered it
  delivery        text        NOT NULL DEFAULT '', -- local, proxied or redirected
  link_host       text        NOT NULL DEFAULT '', -- where the stream came from
  bytes           bigint      NOT NULL DEFAULT 0,  -- proxied by blockbustr
  position_ticks  bigint      NOT NULL DEFAULT 0,
  started_at      timestamptz NOT NULL DEFAULT now(),
  last_seen_at    timestamptz NOT NULL DEFAULT now(),
  stopped_at      timestamptz
);
CREATE UNIQUE INDEX playback_log_session ON playback_log (play_session_id) WHERE play_session_id IS NOT NULL;
CREATE INDEX playback_log_started ON playback_log (started_at DESC, id DESC);
CREATE INDEX playback_log_user ON playback_log (user_id, started_at DESC);

-- +goose Down
DROP TABLE playback_log;
