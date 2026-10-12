-- The release a user last watched an episode of a series in (a play that
-- ran about 5 minutes): later episodes prefer it (stremio.Prefs.Sticky).
-- binge_group is the addon's behaviorHints.bingeGroup, release_group the
-- group parsed from the stream's labels; either may be empty.

-- +goose Up
CREATE TABLE user_series_release (
    user_id       uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    series_id     uuid        NOT NULL REFERENCES items (id) ON DELETE CASCADE,
    release_group text        NOT NULL DEFAULT '',
    binge_group   text        NOT NULL DEFAULT '',
    set_at        timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, series_id)
);

-- +goose Down
DROP TABLE user_series_release;
