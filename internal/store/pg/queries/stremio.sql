-- name: InsertStremioAddon :one
-- A configuration already added (same url_sha) returns no row.
INSERT INTO stremio_addons (url_enc, url_sha, host, manifest, priority)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (url_sha) DO NOTHING
RETURNING *;

-- name: ListStremioAddons :many
SELECT * FROM stremio_addons ORDER BY priority DESC, created_at;

-- name: GetStremioAddon :one
SELECT * FROM stremio_addons WHERE id = $1;

-- name: UpdateStremioAddon :one
UPDATE stremio_addons SET enabled = $2, priority = $3 WHERE id = $1 RETURNING *;

-- name: SetStremioAddonManifest :exec
UPDATE stremio_addons SET manifest = $2, last_fetched_at = now() WHERE id = $1;

-- name: DeleteStremioAddon :execrows
DELETE FROM stremio_addons WHERE id = $1;

-- name: UpsertStremioCatalog :exec
-- A refreshed manifest renames a catalog but keeps its enabled flag and
-- library.
INSERT INTO stremio_catalogs (addon_id, catalog_type, catalog_id, name)
VALUES ($1, $2, $3, $4)
ON CONFLICT (addon_id, catalog_type, catalog_id) DO UPDATE SET name = EXCLUDED.name;

-- name: DeleteStremioCatalogsExcept :exec
-- Drops catalogs the manifest no longer lists; keep holds "type/id" keys.
DELETE FROM stremio_catalogs
WHERE addon_id = $1 AND NOT (catalog_type || '/' || catalog_id = ANY (@keep::text[]));

-- name: ListStremioCatalogs :many
SELECT * FROM stremio_catalogs WHERE addon_id = $1 ORDER BY catalog_type, catalog_id;

-- name: SetStremioCatalogEnabled :execrows
UPDATE stremio_catalogs SET enabled = $4
WHERE addon_id = $1 AND catalog_type = $2 AND catalog_id = $3;

-- Catalog sync (P3.5).

-- name: ListSyncCatalogs :many
-- Every catalog of an enabled addon that is enabled or still has a library
-- (to switch that library off).
SELECT c.addon_id, c.catalog_type, c.catalog_id, c.name, c.library_id, c.enabled,
       a.enabled AS addon_enabled
FROM stremio_catalogs c JOIN stremio_addons a ON a.id = c.addon_id
WHERE (c.enabled AND a.enabled) OR c.library_id IS NOT NULL
ORDER BY a.priority DESC, a.created_at, c.catalog_type, c.catalog_id;

-- name: CreateStremioLibrary :one
-- A name already taken returns no row; the caller tries another.
INSERT INTO libraries (name, kind, options) VALUES (@name, 'stremio', @options)
ON CONFLICT (name) DO NOTHING
RETURNING *;

-- name: GetLibrary :one
SELECT * FROM libraries WHERE id = $1;

-- name: SetLibraryEnabled :exec
UPDATE libraries SET enabled = @enabled WHERE id = @id AND enabled IS DISTINCT FROM @enabled;

-- name: SetStremioCatalogLibrary :exec
UPDATE stremio_catalogs SET library_id = $4
WHERE addon_id = $1 AND catalog_type = $2 AND catalog_id = $3;

-- name: UpsertStremioItem :one
-- Catalog titles and their episodes, keyed by path ("stremio:{type}:{id}").
-- Like UpsertPathItem, metadata applied later (TMDB) wins over the
-- catalog's name/year/overview; provider ids from the catalog are merged in.
INSERT INTO items (library_id, parent_id, top_parent_id, type, name, sort_name, source_kind,
                   path, stremio_ref, index_number, parent_index_number, production_year,
                   premiere_date, overview, provider_ids, date_last_refreshed)
VALUES (@library_id, @parent_id, @top_parent_id, @type, @name, @sort_name, 'stremio',
        @path, @stremio_ref, sqlc.narg('index_number'), sqlc.narg('parent_index_number'),
        sqlc.narg('production_year'), sqlc.narg('premiere_date'), sqlc.narg('overview'), @provider_ids, now())
ON CONFLICT (library_id, path) WHERE path IS NOT NULL DO UPDATE SET
    parent_id           = EXCLUDED.parent_id,
    top_parent_id       = EXCLUDED.top_parent_id,
    type                = EXCLUDED.type,
    stremio_ref         = EXCLUDED.stremio_ref,
    index_number        = EXCLUDED.index_number,
    parent_index_number = EXCLUDED.parent_index_number,
    name            = CASE WHEN coalesce(items.metadata_source, 'none') = 'none' THEN EXCLUDED.name ELSE items.name END,
    sort_name       = CASE WHEN coalesce(items.metadata_source, 'none') = 'none' THEN EXCLUDED.sort_name ELSE items.sort_name END,
    production_year = CASE WHEN coalesce(items.metadata_source, 'none') = 'none' THEN EXCLUDED.production_year ELSE items.production_year END,
    premiere_date   = CASE WHEN coalesce(items.metadata_source, 'none') = 'none' THEN EXCLUDED.premiere_date ELSE items.premiere_date END,
    overview        = CASE WHEN coalesce(items.metadata_source, 'none') = 'none' THEN EXCLUDED.overview ELSE items.overview END,
    provider_ids        = items.provider_ids || EXCLUDED.provider_ids,
    date_last_refreshed = now(),
    is_missing          = false,
    missing_since       = NULL
RETURNING id, (coalesce(metadata_source, 'none') = 'none')::boolean AS catalog_owned;

-- name: TouchStremioEpisodes :exec
-- Keeps a series' episodes when its meta couldn't be fetched this sync, so
-- an addon hiccup doesn't mark them missing.
UPDATE items SET date_last_refreshed = now()
WHERE library_id = @library_id AND type = 'Episode' AND missing_since IS NULL
  AND starts_with(path, @prefix::text);

-- name: EnsureDiscoverLibrary :one
-- The hidden library search results are stored in (P3.13); its name can't
-- clash with a configured library's.
INSERT INTO libraries (name, kind) VALUES ('blockbustr:discover', 'discover')
ON CONFLICT (name) DO UPDATE SET kind = 'discover', enabled = true
RETURNING *;

-- name: UpsertDiscoverItem :one
-- A search result, keyed by path like a catalog title but with the id the
-- caller derives (UUIDv5 of its IMDb id), so the same title always has the
-- same id even after it was cleaned up and found again. Searching it again
-- keeps it (date_last_refreshed).
INSERT INTO items (id, library_id, type, name, sort_name, source_kind, path, stremio_ref,
                   production_year, premiere_date, overview, provider_ids, date_last_refreshed)
VALUES (@id, @library_id, @type, @name, @sort_name, 'stremio', @path, @stremio_ref,
        sqlc.narg('production_year'), sqlc.narg('premiere_date'), sqlc.narg('overview'), @provider_ids, now())
ON CONFLICT (library_id, path) WHERE path IS NOT NULL DO UPDATE SET
    stremio_ref     = EXCLUDED.stremio_ref,
    name            = CASE WHEN coalesce(items.metadata_source, 'none') = 'none' THEN EXCLUDED.name ELSE items.name END,
    sort_name       = CASE WHEN coalesce(items.metadata_source, 'none') = 'none' THEN EXCLUDED.sort_name ELSE items.sort_name END,
    production_year = CASE WHEN coalesce(items.metadata_source, 'none') = 'none' THEN EXCLUDED.production_year ELSE items.production_year END,
    overview        = CASE WHEN coalesce(items.metadata_source, 'none') = 'none' THEN coalesce(EXCLUDED.overview, items.overview) ELSE items.overview END,
    provider_ids    = items.provider_ids || EXCLUDED.provider_ids,
    date_last_refreshed = now(),
    is_missing      = false,
    missing_since   = NULL
RETURNING id, (coalesce(metadata_source, 'none') = 'none')::boolean AS catalog_owned;

-- name: LibraryItemsByImdb :many
-- Titles already in an enabled library (not discover) with these IMDb ids:
-- search shows those instead of a discover copy.
SELECT i.* FROM items i JOIN libraries l ON l.id = i.library_id
WHERE l.enabled AND l.kind <> 'discover' AND i.missing_since IS NULL
  AND i.type = ANY(@types::text[]) AND i.provider_ids ->> 'Imdb' = ANY(@imdb::text[]);

-- name: DeleteStaleDiscoverItems :execrows
-- Discover titles nobody kept: not found by a search since @before and with
-- no user data (played, favourite, in progress) on them or their episodes.
DELETE FROM items i
USING libraries l
WHERE l.id = i.library_id AND l.kind = 'discover' AND i.parent_id IS NULL
  AND coalesce(i.date_last_refreshed, i.date_created) < @before
  AND NOT EXISTS (
    SELECT 1 FROM user_data ud JOIN items d ON d.id = ud.item_id
    WHERE (ud.played OR ud.is_favorite OR ud.playback_position_ticks > 0)
      AND (d.id = i.id OR d.parent_id = i.id
           OR d.parent_id IN (SELECT s.id FROM items s WHERE s.parent_id = i.id)));

-- name: SetStremioAddonURL :one
-- A new configuration of the same addon (P4.3): its sealed URL, hash, host
-- and manifest change; its id, priority, flags and catalogs stay. A URL
-- another addon already has violates url_sha's uniqueness.
UPDATE stremio_addons
SET url_enc = @url_enc, url_sha = @url_sha, host = @host, manifest = @manifest, last_fetched_at = now()
WHERE id = @id
RETURNING *;
