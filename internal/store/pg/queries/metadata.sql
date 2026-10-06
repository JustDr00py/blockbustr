-- Metadata refresh queries (TASKS P1.16).

-- name: ItemsNeedingMetadata :many
-- Never refreshed, matched but stale, or unmatched and due for a retry.
-- Series before Seasons before Episodes, so parents are matched first.
SELECT id, type, name, production_year, path, parent_id, index_number, parent_index_number,
       provider_ids, metadata_source
FROM items
WHERE library_id = @library_id
  AND type IN ('Movie', 'Series', 'Season', 'Episode')
  AND missing_since IS NULL
  AND (metadata_refreshed_at IS NULL
       OR (metadata_source = 'none' AND metadata_refreshed_at < @unmatched_before)
       OR (metadata_source <> 'none' AND metadata_refreshed_at < @matched_before))
ORDER BY CASE type WHEN 'Movie' THEN 0 WHEN 'Series' THEN 1 WHEN 'Season' THEN 2 ELSE 3 END,
         parent_id NULLS FIRST, index_number NULLS FIRST, path;

-- name: ApplyItemMetadata :exec
-- NULL arguments keep the current value (e.g. episode sort names stay
-- number-based).
UPDATE items SET
    name                  = coalesce(sqlc.narg('name'), name),
    sort_name             = coalesce(sqlc.narg('sort_name'), sort_name),
    original_title        = sqlc.narg('original_title'),
    original_language     = sqlc.narg('original_language'),
    overview              = sqlc.narg('overview'),
    tagline               = sqlc.narg('tagline'),
    official_rating       = sqlc.narg('official_rating'),
    community_rating      = sqlc.narg('community_rating'),
    premiere_date         = sqlc.narg('premiere_date'),
    end_date              = sqlc.narg('end_date'),
    production_year       = coalesce(sqlc.narg('production_year'), production_year),
    runtime_ticks         = coalesce(runtime_ticks, sqlc.narg('runtime_ticks')),
    provider_ids          = @provider_ids,
    metadata_source       = @metadata_source,
    metadata_refreshed_at = now()
WHERE id = @id;

-- name: MarkMetadataUnmatched :exec
UPDATE items SET metadata_source = 'none', metadata_refreshed_at = now() WHERE id = $1;

-- name: UpsertGenre :one
INSERT INTO genres (name) VALUES ($1) ON CONFLICT (name) DO UPDATE SET name = genres.name RETURNING id;

-- name: DeleteItemGenres :exec
DELETE FROM item_genres WHERE item_id = $1;

-- name: InsertItemGenre :exec
INSERT INTO item_genres (item_id, genre_id, sort_order) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING;

-- name: UpsertStudio :one
INSERT INTO studios (name) VALUES ($1) ON CONFLICT (name) DO UPDATE SET name = studios.name RETURNING id;

-- name: DeleteItemStudios :exec
DELETE FROM item_studios WHERE item_id = $1;

-- name: InsertItemStudio :exec
INSERT INTO item_studios (item_id, studio_id, sort_order) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING;

-- name: UpsertPerson :exec
INSERT INTO people (id, name, provider_ids, image_url) VALUES ($1, $2, $3, $4)
ON CONFLICT (id) DO UPDATE SET name = EXCLUDED.name, provider_ids = EXCLUDED.provider_ids,
    image_url = coalesce(EXCLUDED.image_url, people.image_url), updated_at = now();

-- name: DeleteItemPeople :exec
DELETE FROM item_people WHERE item_id = $1;

-- name: InsertItemPerson :exec
INSERT INTO item_people (item_id, person_id, kind, role, sort_order) VALUES ($1, $2, $3, $4, $5)
ON CONFLICT DO NOTHING;

-- name: DeleteRemoteImages :exec
-- Artwork from metadata providers; locally found images are kept.
DELETE FROM images WHERE item_id = $1 AND local_path IS NULL;

-- name: UpsertImage :exec
INSERT INTO images (item_id, type, idx, source_url, tag) VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (item_id, type, idx) DO UPDATE SET source_url = EXCLUDED.source_url, tag = EXCLUDED.tag;

-- name: ListItemGenres :many
SELECT g.name FROM item_genres ig JOIN genres g ON g.id = ig.genre_id WHERE ig.item_id = $1 ORDER BY ig.sort_order, g.name;

-- name: ListItemStudios :many
SELECT s.name FROM item_studios ist JOIN studios s ON s.id = ist.studio_id WHERE ist.item_id = $1 ORDER BY ist.sort_order, s.name;

-- name: ListItemPeople :many
SELECT p.id, p.name, p.provider_ids, p.image_url, ip.kind, ip.role, ip.sort_order
FROM item_people ip JOIN people p ON p.id = ip.person_id
WHERE ip.item_id = $1
ORDER BY CASE ip.kind WHEN 'Actor' THEN 0 WHEN 'GuestStar' THEN 1 WHEN 'Director' THEN 2 ELSE 3 END, ip.sort_order, p.name;

-- name: ListItemImages :many
SELECT * FROM images WHERE item_id = $1 ORDER BY type, idx;
