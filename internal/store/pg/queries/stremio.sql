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
