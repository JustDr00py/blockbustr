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

-- name: AdoptStremioLibrary :one
-- An addon removed and added again (or a catalog dropped from its manifest
-- and back) left its library behind, linked to no catalog. The returning
-- catalog takes the oldest such library of the same type and id, with its
-- name, items and watch history, instead of making a "(2)".
UPDATE libraries SET options = @options
WHERE id = (
    SELECT l.id FROM libraries l
    WHERE l.kind = 'stremio'
      AND l.options->>'catalogType' = @catalog_type::text
      AND l.options->>'catalogId' = @catalog_id::text
      AND NOT EXISTS (SELECT 1 FROM stremio_catalogs c WHERE c.library_id = l.id)
    ORDER BY l.created_at
    LIMIT 1
    FOR UPDATE
)
RETURNING *;

-- name: DisableOrphanStremioLibraries :many
-- Switches off catalog libraries no catalog links to any more (their
-- addon was removed, or the manifest dropped the catalog), so they leave
-- clients' views. They stay to be adopted if the catalog comes back.
UPDATE libraries l SET enabled = false
WHERE l.kind = 'stremio' AND l.enabled
  AND NOT EXISTS (SELECT 1 FROM stremio_catalogs c WHERE c.library_id = l.id)
RETURNING l.name;

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

-- name: UpsertStremioTitle :one
-- A catalog movie or series: one item per title (canonical_key), whichever
-- catalogs list it. Its home library's catalog (or the first catalog to
-- list a title found by search) sets its path, addon ref and, until
-- metadata is applied (TMDB), its name, year and overview; any catalog
-- merges in provider ids and keeps it fresh. home reports whether this is
-- its home: that catalog writes its episodes.
INSERT INTO items (id, library_id, parent_id, top_parent_id, type, name, sort_name, source_kind,
                   path, stremio_ref, canonical_key, production_year, premiere_date, overview,
                   provider_ids, date_last_refreshed)
VALUES (@id, @library_id, @folder_id, @folder_id, @type, @name, @sort_name, 'stremio',
        @path, @stremio_ref, @canonical_key, sqlc.narg('production_year'), sqlc.narg('premiere_date'),
        sqlc.narg('overview'), @provider_ids, now())
ON CONFLICT (canonical_key) WHERE canonical_key IS NOT NULL DO UPDATE SET
    library_id    = CASE WHEN items.library_id IN (SELECT d.id FROM libraries d WHERE d.kind = 'discover')
                         THEN EXCLUDED.library_id ELSE items.library_id END,
    parent_id     = CASE WHEN items.library_id IN (SELECT d.id FROM libraries d WHERE d.kind = 'discover')
                         THEN EXCLUDED.parent_id ELSE items.parent_id END,
    top_parent_id = CASE WHEN items.library_id IN (SELECT d.id FROM libraries d WHERE d.kind = 'discover')
                         THEN EXCLUDED.top_parent_id ELSE items.top_parent_id END,
    path          = CASE WHEN items.library_id = EXCLUDED.library_id
                           OR items.library_id IN (SELECT d.id FROM libraries d WHERE d.kind = 'discover')
                         THEN EXCLUDED.path ELSE items.path END,
    stremio_ref   = CASE WHEN items.library_id = EXCLUDED.library_id
                           OR items.library_id IN (SELECT d.id FROM libraries d WHERE d.kind = 'discover')
                         THEN EXCLUDED.stremio_ref ELSE items.stremio_ref END,
    name            = CASE WHEN coalesce(items.metadata_source, 'none') = 'none' AND items.library_id = EXCLUDED.library_id
                           THEN EXCLUDED.name ELSE items.name END,
    sort_name       = CASE WHEN coalesce(items.metadata_source, 'none') = 'none' AND items.library_id = EXCLUDED.library_id
                           THEN EXCLUDED.sort_name ELSE items.sort_name END,
    production_year = CASE WHEN coalesce(items.metadata_source, 'none') = 'none' AND items.library_id = EXCLUDED.library_id
                           THEN EXCLUDED.production_year ELSE items.production_year END,
    premiere_date   = CASE WHEN coalesce(items.metadata_source, 'none') = 'none' AND items.library_id = EXCLUDED.library_id
                           THEN EXCLUDED.premiere_date ELSE items.premiere_date END,
    overview        = CASE WHEN coalesce(items.metadata_source, 'none') = 'none' AND items.library_id = EXCLUDED.library_id
                           THEN EXCLUDED.overview ELSE items.overview END,
    provider_ids        = items.provider_ids || EXCLUDED.provider_ids,
    date_last_refreshed = now(),
    is_missing          = false,
    missing_since       = NULL
RETURNING id, (coalesce(metadata_source, 'none') = 'none' AND library_id = @library_id)::boolean AS catalog_owned,
          (library_id = @library_id)::boolean AS home;

-- name: RekeyTitle :exec
-- A title first keyed by its TMDB id, now that its IMDb id is known; left
-- alone if another item already has that key.
UPDATE items i SET canonical_key = @new_key::text
WHERE i.canonical_key = @old_key::text
  AND NOT EXISTS (SELECT 1 FROM items o WHERE o.canonical_key = @new_key::text);

-- name: UpsertItemLibrary :exec
-- A catalog sync saw the title: it's (still) in the library.
INSERT INTO item_libraries (item_id, library_id, position, seen_at)
VALUES (@item_id, @library_id, @position, now())
ON CONFLICT (item_id, library_id) DO UPDATE SET
    position = EXCLUDED.position, seen_at = now(), missing_since = NULL;

-- name: MarkUnseenMemberships :execrows
-- Titles the catalog sync that started at @scan_start didn't see: they left
-- the catalog (the library), not necessarily the others.
UPDATE item_libraries SET missing_since = coalesce(missing_since, now())
WHERE library_id = @library_id AND seen_at < @scan_start;

-- name: DeleteMissingMemberships :exec
DELETE FROM item_libraries WHERE library_id = @library_id AND missing_since < @cutoff;

-- name: RehomeTitles :exec
-- Titles of the library whose home isn't a live, enabled library of theirs
-- any more move to one that is (the most recently seen): its catalog writes
-- their episodes from now on. Titles in no such library stay put.
UPDATE items i SET library_id = n.library_id, parent_id = n.folder_id, top_parent_id = n.folder_id
FROM (
    SELECT DISTINCT ON (t.id) t.id, il.library_id, f.id AS folder_id
    FROM items t
    JOIN item_libraries il ON il.item_id = t.id AND il.missing_since IS NULL
    JOIN libraries l ON l.id = il.library_id AND l.enabled
    JOIN items f ON f.library_id = il.library_id AND f.type = 'CollectionFolder'
    WHERE t.canonical_key IS NOT NULL
      AND (t.library_id = @library_id
           OR EXISTS (SELECT 1 FROM item_libraries m WHERE m.item_id = t.id AND m.library_id = @library_id))
      AND NOT EXISTS (SELECT 1 FROM item_libraries h JOIN libraries hl ON hl.id = h.library_id AND hl.enabled
                      WHERE h.item_id = t.id AND h.library_id = t.library_id AND h.missing_since IS NULL)
    ORDER BY t.id, il.seen_at DESC, il.library_id
) n
WHERE i.id = n.id;

-- name: RefreshTitleLibraries :exec
-- The library's titles' library_ids are the libraries they're live in; a
-- title live in none is missing (deleted after the grace, like a file).
-- Titles found by search (in no catalog) aren't the library's.
WITH titles AS (
    SELECT c.id
    FROM items c
    WHERE c.canonical_key IS NOT NULL
      AND (c.library_id = @library_id
           OR c.id IN (SELECT m.item_id FROM item_libraries m WHERE m.library_id = @library_id))
), live AS (
    SELECT il.item_id, array_agg(il.library_id ORDER BY il.library_id) AS ids
    FROM item_libraries il JOIN titles x ON x.id = il.item_id
    WHERE il.missing_since IS NULL
    GROUP BY il.item_id
)
UPDATE items t SET
    library_ids   = coalesce(live.ids, '{}'),
    is_missing    = live.ids IS NULL,
    missing_since = CASE WHEN live.ids IS NULL THEN coalesce(t.missing_since, now()) END
FROM titles x LEFT JOIN live ON live.item_id = x.id
WHERE t.id = x.id
  AND (t.library_ids, t.is_missing) IS DISTINCT FROM (coalesce(live.ids, '{}'), live.ids IS NULL);

-- name: SyncTitleDescendants :exec
-- Seasons and episodes follow their title: its home and its libraries.
-- The children are found by joins (title → season → episode), not an OR'd
-- correlated subquery, which ran a subplan per (item, title) pair.
WITH titles AS (
    SELECT t.id, t.library_id, t.top_parent_id, t.library_ids
    FROM items t
    WHERE t.canonical_key IS NOT NULL
      AND (t.library_id = @library_id
           OR t.id IN (SELECT m.item_id FROM item_libraries m WHERE m.library_id = @library_id))
), children AS (
    SELECT c.id, t.library_id, t.top_parent_id, t.library_ids
    FROM titles t JOIN items c ON c.parent_id = t.id AND c.type IN ('Season', 'Episode')
    UNION ALL
    SELECT e.id, t.library_id, t.top_parent_id, t.library_ids
    FROM titles t
    JOIN items s ON s.parent_id = t.id AND s.type = 'Season'
    JOIN items e ON e.parent_id = s.id AND e.type = 'Episode'
)
UPDATE items c SET library_id = k.library_id, top_parent_id = k.top_parent_id, library_ids = k.library_ids
FROM children k
WHERE c.id = k.id
  AND (c.library_id, c.top_parent_id, c.library_ids) IS DISTINCT FROM (k.library_id, k.top_parent_id, k.library_ids);

-- name: MarkUnseenEpisodesMissing :execrows
-- Episodes the sync that started at @scan_start didn't write (titles are
-- missing by their libraries: RefreshTitleLibraries).
UPDATE items SET is_missing = true, missing_since = coalesce(missing_since, now())
WHERE library_id = @library_id AND type = 'Episode' AND path IS NOT NULL
  AND (date_last_refreshed IS NULL OR date_last_refreshed < @scan_start);

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
-- keeps it (date_last_refreshed). A title a catalog has is that item (one
-- per canonical_key), left to its catalogs; one that left them all is
-- kept here instead.
INSERT INTO items (id, library_id, type, name, sort_name, source_kind, path, stremio_ref, canonical_key,
                   production_year, premiere_date, overview, provider_ids, date_last_refreshed)
VALUES (@id, @library_id, @type, @name, @sort_name, 'stremio', @path, @stremio_ref, @canonical_key,
        sqlc.narg('production_year'), sqlc.narg('premiere_date'), sqlc.narg('overview'), @provider_ids, now())
ON CONFLICT (canonical_key) WHERE canonical_key IS NOT NULL DO UPDATE SET
    library_id      = CASE WHEN items.library_ids = '{}' THEN EXCLUDED.library_id ELSE items.library_id END,
    parent_id       = CASE WHEN items.library_ids = '{}' THEN NULL ELSE items.parent_id END,
    top_parent_id   = CASE WHEN items.library_ids = '{}' THEN NULL ELSE items.top_parent_id END,
    stremio_ref     = CASE WHEN items.library_ids = '{}' THEN EXCLUDED.stremio_ref ELSE items.stremio_ref END,
    name            = CASE WHEN coalesce(items.metadata_source, 'none') = 'none' AND items.library_ids = '{}' THEN EXCLUDED.name ELSE items.name END,
    sort_name       = CASE WHEN coalesce(items.metadata_source, 'none') = 'none' AND items.library_ids = '{}' THEN EXCLUDED.sort_name ELSE items.sort_name END,
    production_year = CASE WHEN coalesce(items.metadata_source, 'none') = 'none' AND items.library_ids = '{}' THEN EXCLUDED.production_year ELSE items.production_year END,
    overview        = CASE WHEN coalesce(items.metadata_source, 'none') = 'none' AND items.library_ids = '{}' THEN coalesce(EXCLUDED.overview, items.overview) ELSE items.overview END,
    provider_ids    = items.provider_ids || EXCLUDED.provider_ids,
    date_last_refreshed = now(),
    is_missing      = false,
    missing_since   = NULL
RETURNING id, (coalesce(metadata_source, 'none') = 'none' AND library_id = @library_id)::boolean AS catalog_owned;

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

-- name: RenameCatalogLibrary :one
-- Renames a Stremio catalog library and its folder, found by the folder's
-- item id (what clients and the admin UI know). Libraries from config.yaml
-- are named there, so they don't match: no row.
WITH lib AS (
    UPDATE libraries SET name = @name
    WHERE kind = 'stremio'
      AND id = (SELECT f.library_id FROM items f WHERE f.id = @folder_id AND f.type = 'CollectionFolder')
    RETURNING id
)
UPDATE items SET name = @name, sort_name = @sort_name
WHERE type = 'CollectionFolder' AND library_id = (SELECT id FROM lib)
RETURNING library_id;
