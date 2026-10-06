-- Scanner queries (TASKS P1.14).

-- name: UpsertLibrary :one
INSERT INTO libraries (name, kind, paths) VALUES ($1, $2, $3)
ON CONFLICT (name) DO UPDATE SET kind = EXCLUDED.kind, paths = EXCLUDED.paths, enabled = true
RETURNING *;

-- name: DisableLibrariesExcept :execrows
-- Libraries no longer in config.yaml stop being listed and scanned.
UPDATE libraries SET enabled = false WHERE enabled AND NOT (id = ANY(@keep::uuid[]));

-- name: EnsureCollectionFolder :one
INSERT INTO items (library_id, type, name, sort_name)
VALUES (@library_id, 'CollectionFolder', @name, @sort_name)
ON CONFLICT (library_id) WHERE type = 'CollectionFolder'
DO UPDATE SET name = EXCLUDED.name, sort_name = EXCLUDED.sort_name
RETURNING id;

-- name: UpsertPathItem :one
-- Movies, Series and Episodes are keyed by path. Every upsert marks the item
-- as seen now (date_last_refreshed) and clears any missing flag.
INSERT INTO items (library_id, parent_id, top_parent_id, type, name, sort_name, source_kind,
                   path, strm_url, index_number, parent_index_number, production_year, etag,
                   date_last_refreshed)
VALUES (@library_id, @parent_id, @top_parent_id, @type, @name, @sort_name, @source_kind,
        @path, sqlc.narg('strm_url'), sqlc.narg('index_number'), sqlc.narg('parent_index_number'),
        sqlc.narg('production_year'), sqlc.narg('etag'), now())
ON CONFLICT (library_id, path) WHERE path IS NOT NULL DO UPDATE SET
    parent_id           = EXCLUDED.parent_id,
    top_parent_id       = EXCLUDED.top_parent_id,
    type                = EXCLUDED.type,
    -- Once metadata is applied, its name/sort name/year win over the file name.
    name                = CASE WHEN coalesce(items.metadata_source, 'none') = 'none' THEN EXCLUDED.name ELSE items.name END,
    sort_name           = CASE WHEN coalesce(items.metadata_source, 'none') = 'none' THEN EXCLUDED.sort_name ELSE items.sort_name END,
    source_kind         = EXCLUDED.source_kind,
    strm_url            = EXCLUDED.strm_url,
    index_number        = EXCLUDED.index_number,
    parent_index_number = EXCLUDED.parent_index_number,
    production_year     = CASE WHEN coalesce(items.metadata_source, 'none') = 'none' THEN EXCLUDED.production_year ELSE items.production_year END,
    date_modified       = CASE WHEN items.etag IS DISTINCT FROM EXCLUDED.etag THEN now() ELSE items.date_modified END,
    etag                = EXCLUDED.etag,
    date_last_refreshed = now(),
    is_missing          = false,
    missing_since       = NULL
RETURNING id;

-- name: UpsertSeason :one
INSERT INTO items (library_id, parent_id, top_parent_id, type, name, sort_name, index_number, date_last_refreshed)
VALUES (@library_id, @parent_id, @top_parent_id, 'Season', @name, @sort_name, @index_number, now())
ON CONFLICT (parent_id, index_number) WHERE type = 'Season' DO UPDATE SET
    name = CASE WHEN coalesce(items.metadata_source, 'none') = 'none' THEN EXCLUDED.name ELSE items.name END,
    sort_name = EXCLUDED.sort_name,
    date_last_refreshed = now(), is_missing = false, missing_since = NULL
RETURNING id;

-- name: ItemsNeedingSource :many
-- Playable items whose media source is missing or was built from another
-- version of the file/URL: local files need probing, .strm items an
-- (unprobed) remote source.
SELECT i.id, i.source_kind, i.path, i.strm_url, i.etag
FROM items i
LEFT JOIN media_sources ms ON ms.item_id = i.id
WHERE i.library_id = $1
  AND i.source_kind IN ('file', 'strm')
  AND i.missing_since IS NULL
  AND (ms.id IS NULL OR ms.etag IS DISTINCT FROM i.etag);

-- name: DeleteMediaSources :exec
DELETE FROM media_sources WHERE item_id = $1;

-- name: InsertMediaSource :one
INSERT INTO media_sources (item_id, name, container, size, bitrate, path_or_url, protocol, is_remote,
                           probed_at, probe_error, etag, runtime_ticks)
VALUES (@item_id, @name, sqlc.narg('container'), sqlc.narg('size'), sqlc.narg('bitrate'), @path_or_url,
        @protocol, @is_remote, sqlc.narg('probed_at'), sqlc.narg('probe_error'), sqlc.narg('etag'),
        sqlc.narg('runtime_ticks'))
RETURNING id;

-- name: InsertMediaStream :exec
INSERT INTO media_streams (media_source_id, idx, type, codec, language, title, is_default, is_forced,
                           is_hearing_impaired, is_original, is_external, width, height, bitrate,
                           channels, channel_layout, sample_rate, video_range, video_range_type,
                           profile, level, pixel_format, bit_depth, aspect_ratio, average_frame_rate,
                           real_frame_rate, is_interlaced, color_transfer, color_primaries, color_space,
                           color_range, dv_profile, dv_level, dv_bl_compat_id, time_base,
                           dv_version_major, dv_version_minor, dv_rpu_present, dv_el_present, dv_bl_present,
                           external_path)
VALUES (@media_source_id, @idx, @type, sqlc.narg('codec'), sqlc.narg('language'), sqlc.narg('title'),
        @is_default, @is_forced, @is_hearing_impaired, @is_original, sqlc.narg('external_path')::text IS NOT NULL,
        sqlc.narg('width'), sqlc.narg('height'), sqlc.narg('bitrate'), sqlc.narg('channels'),
        sqlc.narg('channel_layout'), sqlc.narg('sample_rate'), sqlc.narg('video_range'),
        sqlc.narg('video_range_type'), sqlc.narg('profile'), sqlc.narg('level'), sqlc.narg('pixel_format'),
        sqlc.narg('bit_depth'), sqlc.narg('aspect_ratio'), sqlc.narg('average_frame_rate'),
        sqlc.narg('real_frame_rate'), @is_interlaced, sqlc.narg('color_transfer'),
        sqlc.narg('color_primaries'), sqlc.narg('color_space'), sqlc.narg('color_range'),
        sqlc.narg('dv_profile'), sqlc.narg('dv_level'), sqlc.narg('dv_bl_compat_id'), sqlc.narg('time_base'),
        sqlc.narg('dv_version_major'), sqlc.narg('dv_version_minor'), sqlc.narg('dv_rpu_present'),
        sqlc.narg('dv_el_present'), sqlc.narg('dv_bl_present'),
        sqlc.narg('external_path'));

-- name: DeleteChapters :exec
DELETE FROM chapters WHERE item_id = $1;

-- name: InsertChapter :exec
INSERT INTO chapters (item_id, idx, start_ticks, name) VALUES ($1, $2, $3, $4);

-- name: SetItemRuntime :exec
UPDATE items SET runtime_ticks = $2 WHERE id = $1;

-- name: MarkUnseenMissing :execrows
-- Path items the scan that started at @scan_start didn't see.
UPDATE items SET is_missing = true, missing_since = coalesce(missing_since, now())
WHERE library_id = @library_id
  AND path IS NOT NULL
  AND type IN ('Movie', 'Series', 'Episode')
  AND (date_last_refreshed IS NULL OR date_last_refreshed < @scan_start);

-- name: DeleteMissingBefore :execrows
DELETE FROM items WHERE library_id = @library_id AND missing_since < @cutoff;

-- name: DeleteEmptySeasons :execrows
DELETE FROM items s
WHERE s.library_id = $1 AND s.type = 'Season'
  AND NOT EXISTS (SELECT 1 FROM items c WHERE c.parent_id = s.id);

-- name: DeleteEmptySeries :execrows
DELETE FROM items s
WHERE s.library_id = $1 AND s.type = 'Series'
  AND NOT EXISTS (SELECT 1 FROM items c WHERE c.parent_id = s.id);

-- name: CountItemsByType :many
SELECT type, count(*) AS n, count(*) FILTER (WHERE missing_since IS NOT NULL) AS missing
FROM items WHERE library_id = $1 GROUP BY type ORDER BY type;

-- name: ListLibraryItems :many
-- For tests and admin views: every item of a library in a stable order.
SELECT * FROM items WHERE library_id = $1 ORDER BY type, path NULLS FIRST, index_number NULLS FIRST;

-- name: GetMediaSourceByItem :one
SELECT * FROM media_sources WHERE item_id = $1;

-- name: ListMediaStreams :many
SELECT * FROM media_streams WHERE media_source_id = $1 ORDER BY idx;

-- name: ListChapters :many
SELECT * FROM chapters WHERE item_id = $1 ORDER BY idx;

-- name: DBNow :one
-- The database clock; scan bookkeeping compares against now() values set by
-- the database, so it must not use the app host's clock.
SELECT now()::timestamptz AS now;

-- name: GetStrmItem :one
-- A .strm item, for probing its remote source at first play (TASKS P2.4b).
SELECT id, source_kind, path, strm_url, etag FROM items
WHERE id = $1 AND source_kind = 'strm' AND strm_url IS NOT NULL AND missing_since IS NULL;
