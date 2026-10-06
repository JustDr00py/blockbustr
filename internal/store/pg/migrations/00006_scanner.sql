-- Library scanner support (TASKS P1.14, DESIGN §6).

-- +goose Up
ALTER TABLE libraries ADD CONSTRAINT libraries_name_key UNIQUE (name);

-- Exactly one top-level CollectionFolder per library, and one Season per
-- (series, number), so rescans upsert instead of duplicating.
CREATE UNIQUE INDEX items_collection_folder_key ON items (library_id) WHERE type = 'CollectionFolder';
CREATE UNIQUE INDEX items_season_key ON items (parent_id, index_number) WHERE type = 'Season';

-- Not seen by the last scan since this time; deleted after scan.missing_grace.
ALTER TABLE items ADD COLUMN missing_since timestamptz;

-- etag of the item the source was built from (file size+mtime, or the .strm
-- URL), so unchanged files are never re-probed.
ALTER TABLE media_sources
    ADD COLUMN etag          text,
    ADD COLUMN runtime_ticks bigint;

-- The rest of media.Stream (P1.13).
ALTER TABLE media_streams
    ADD COLUMN bit_depth           int,
    ADD COLUMN aspect_ratio        text,
    ADD COLUMN average_frame_rate  real,
    ADD COLUMN real_frame_rate     real,
    ADD COLUMN is_interlaced       boolean NOT NULL DEFAULT false,
    ADD COLUMN color_transfer      text,
    ADD COLUMN color_primaries     text,
    ADD COLUMN color_space         text,
    ADD COLUMN color_range         text,
    ADD COLUMN video_range_type    text,
    ADD COLUMN dv_profile          int,
    ADD COLUMN dv_level            int,
    ADD COLUMN dv_bl_compat_id     int,
    ADD COLUMN sample_rate         int,
    ADD COLUMN is_hearing_impaired boolean NOT NULL DEFAULT false,
    ADD COLUMN is_original         boolean NOT NULL DEFAULT false;

CREATE TABLE chapters (
    item_id     uuid   NOT NULL REFERENCES items (id) ON DELETE CASCADE,
    idx         int    NOT NULL,
    start_ticks bigint NOT NULL,
    name        text   NOT NULL DEFAULT '',
    PRIMARY KEY (item_id, idx)
);

-- +goose Down
DROP TABLE IF EXISTS chapters;
ALTER TABLE media_streams
    DROP COLUMN IF EXISTS bit_depth, DROP COLUMN IF EXISTS aspect_ratio,
    DROP COLUMN IF EXISTS average_frame_rate, DROP COLUMN IF EXISTS real_frame_rate,
    DROP COLUMN IF EXISTS is_interlaced, DROP COLUMN IF EXISTS color_transfer,
    DROP COLUMN IF EXISTS color_primaries, DROP COLUMN IF EXISTS color_space,
    DROP COLUMN IF EXISTS color_range, DROP COLUMN IF EXISTS video_range_type,
    DROP COLUMN IF EXISTS dv_profile, DROP COLUMN IF EXISTS dv_level,
    DROP COLUMN IF EXISTS dv_bl_compat_id, DROP COLUMN IF EXISTS sample_rate,
    DROP COLUMN IF EXISTS is_hearing_impaired, DROP COLUMN IF EXISTS is_original;
ALTER TABLE media_sources DROP COLUMN IF EXISTS etag, DROP COLUMN IF EXISTS runtime_ticks;
ALTER TABLE items DROP COLUMN IF EXISTS missing_since;
DROP INDEX IF EXISTS items_season_key;
DROP INDEX IF EXISTS items_collection_folder_key;
ALTER TABLE libraries DROP CONSTRAINT IF EXISTS libraries_name_key;
