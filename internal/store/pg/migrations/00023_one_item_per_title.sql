-- One item per catalog title: a movie or series listed by several catalogs
-- was a row per catalog library, each with its own watch progress.
--
-- canonical_key names a title: "movie:tt…" (its IMDb id), else
-- "movie:tmdb:…", else the addon's id. item_libraries lists the catalog
-- libraries it's in; library_ids mirrors the live ones on the title and its
-- seasons and episodes, for the item queries' library filters. library_id
-- stays the title's home: the library whose catalog writes its episodes.
--
-- Existing copies are merged into one: the copy that's in a catalog (not
-- only found by search), has metadata and isn't missing is kept, with its
-- id. Users' data is merged (played and favourite if any copy was; resume
-- point and tracks of the copy played last), episodes' too, and plays move.

-- +goose Up
ALTER TABLE items ADD COLUMN canonical_key text;
ALTER TABLE items ADD COLUMN library_ids uuid[] NOT NULL DEFAULT '{}';

CREATE TABLE item_libraries (
    item_id       uuid        NOT NULL REFERENCES items (id) ON DELETE CASCADE,
    library_id    uuid        NOT NULL REFERENCES libraries (id) ON DELETE CASCADE,
    position      int         NOT NULL DEFAULT 0, -- in the catalog
    seen_at       timestamptz NOT NULL DEFAULT now(),
    missing_since timestamptz,
    PRIMARY KEY (item_id, library_id)
);
CREATE INDEX item_libraries_library_idx ON item_libraries (library_id);

UPDATE items i SET canonical_key = lower(i.type) || ':' || coalesce(
        nullif(i.provider_ids ->> 'Imdb', ''),
        'tmdb:' || nullif(i.provider_ids ->> 'Tmdb', ''),
        CASE WHEN i.stremio_ref ->> 'id' LIKE 'tt%' OR i.stremio_ref ->> 'id' LIKE 'tmdb:%'
             THEN i.stremio_ref ->> 'id' ELSE 'id:' || (i.stremio_ref ->> 'id') END)
WHERE i.source_kind = 'stremio' AND i.type IN ('Movie', 'Series');

-- Each copy and the copy it's merged into (itself for the one kept).
CREATE TEMP TABLE title_map ON COMMIT DROP AS
SELECT i.id AS old_id,
       first_value(i.id) OVER (PARTITION BY i.canonical_key
           ORDER BY (l.kind = 'discover'), (coalesce(i.metadata_source, 'none') = 'none'),
                    (i.missing_since IS NOT NULL), i.date_created, i.id) AS new_id
FROM items i JOIN libraries l ON l.id = i.library_id
WHERE i.canonical_key IS NOT NULL;

INSERT INTO item_libraries (item_id, library_id, seen_at, missing_since)
SELECT m.new_id, i.library_id, max(coalesce(i.date_last_refreshed, i.date_created)),
       CASE WHEN bool_or(i.missing_since IS NULL) THEN NULL ELSE min(i.missing_since) END
FROM title_map m
JOIN items i ON i.id = m.old_id
JOIN libraries l ON l.id = i.library_id AND l.kind = 'stremio'
GROUP BY m.new_id, i.library_id;

-- A merged copy's episodes and the kept copy's episode they match (same
-- season and number).
CREATE TEMP TABLE episode_map ON COMMIT DROP AS
SELECT DISTINCT ON (e.id) e.id AS old_id, ke.id AS new_id
FROM title_map m
JOIN items s  ON s.parent_id = m.old_id AND s.type = 'Season'
JOIN items e  ON e.parent_id = s.id AND e.type = 'Episode'
JOIN items ks ON ks.parent_id = m.new_id AND ks.type = 'Season' AND ks.index_number = s.index_number
JOIN items ke ON ke.parent_id = ks.id AND ke.type = 'Episode' AND ke.index_number = e.index_number
WHERE m.old_id <> m.new_id
ORDER BY e.id, ke.date_created, ke.id;

CREATE TEMP TABLE id_map ON COMMIT DROP AS
SELECT old_id, new_id FROM title_map WHERE old_id <> new_id
UNION ALL
SELECT old_id, new_id FROM episode_map;

-- Each user's data on a kept item and the copies merged into it, as one row.
CREATE TEMP TABLE merged_user_data ON COMMIT DROP AS
WITH src AS (
    SELECT ud.*, coalesce(x.new_id, ud.item_id) AS target
    FROM user_data ud
    LEFT JOIN id_map x ON x.old_id = ud.item_id
    WHERE ud.item_id IN (SELECT old_id FROM id_map UNION SELECT new_id FROM id_map)
)
SELECT user_id, target AS item_id,
       bool_or(played) AS played,
       sum(play_count)::int AS play_count,
       (array_agg(playback_position_ticks ORDER BY last_played_at DESC NULLS LAST, updated_at DESC))[1] AS playback_position_ticks,
       bool_or(is_favorite) AS is_favorite,
       (array_agg(rating ORDER BY rating IS NULL, updated_at DESC))[1] AS rating,
       max(last_played_at) AS last_played_at,
       (array_agg(audio_stream_idx ORDER BY last_played_at DESC NULLS LAST, updated_at DESC))[1] AS audio_stream_idx,
       (array_agg(subtitle_stream_idx ORDER BY last_played_at DESC NULLS LAST, updated_at DESC))[1] AS subtitle_stream_idx,
       max(updated_at) AS updated_at
FROM src
GROUP BY user_id, target;

DELETE FROM user_data WHERE item_id IN (SELECT old_id FROM id_map UNION SELECT new_id FROM id_map);
INSERT INTO user_data (user_id, item_id, played, play_count, playback_position_ticks, is_favorite, rating,
                       last_played_at, audio_stream_idx, subtitle_stream_idx, updated_at)
SELECT user_id, item_id, played, play_count, playback_position_ticks, is_favorite, rating,
       last_played_at, audio_stream_idx, subtitle_stream_idx, updated_at
FROM merged_user_data;

UPDATE playback_log p SET item_id = x.new_id FROM id_map x WHERE p.item_id = x.old_id;

DELETE FROM items WHERE id IN (SELECT old_id FROM title_map WHERE old_id <> new_id);

UPDATE items t SET
    library_ids   = coalesce((SELECT array_agg(il.library_id ORDER BY il.library_id) FROM item_libraries il
                              WHERE il.item_id = t.id AND il.missing_since IS NULL), '{}'),
    is_missing    = false,
    missing_since = NULL
WHERE t.canonical_key IS NOT NULL
  AND EXISTS (SELECT 1 FROM item_libraries il WHERE il.item_id = t.id AND il.missing_since IS NULL);
UPDATE items c SET library_ids = t.library_ids
FROM items t
WHERE t.canonical_key IS NOT NULL AND c.type IN ('Season', 'Episode')
  AND (c.parent_id = t.id OR c.parent_id IN (SELECT s.id FROM items s WHERE s.parent_id = t.id AND s.type = 'Season'));

CREATE UNIQUE INDEX items_canonical_key ON items (canonical_key) WHERE canonical_key IS NOT NULL;
CREATE INDEX items_library_ids_idx ON items USING gin (library_ids);

-- +goose Down
-- Merged copies aren't split again.
DROP INDEX items_library_ids_idx;
DROP INDEX items_canonical_key;
DROP TABLE item_libraries;
ALTER TABLE items DROP COLUMN library_ids;
ALTER TABLE items DROP COLUMN canonical_key;
