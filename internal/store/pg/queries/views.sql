-- Library views (TASKS P1.18).

-- name: ListCollectionFolders :many
-- The top-level folder of every enabled library, in the admin's order
-- (position, then name). A Stremio catalog library reports the collection
-- type of its catalog (movies or tvshows), which is what clients and
-- handlers switch on.
SELECT i.id, i.name, i.sort_name, i.date_created, i.date_modified, i.etag,
       l.id AS library_id,
       (CASE WHEN l.kind = 'stremio' THEN coalesce(l.options ->> 'collectionType', 'movies') ELSE l.kind END)::text AS kind,
       l.paths, l.hidden
FROM items i JOIN libraries l ON l.id = i.library_id
WHERE i.type = 'CollectionFolder' AND l.enabled
ORDER BY l.position, i.sort_name, i.id;

-- name: SetLibraryHidden :one
-- Hides a library from every user's views (or shows it again), found by its
-- folder's item id.
UPDATE libraries SET hidden = @hidden
WHERE id = (SELECT f.library_id FROM items f WHERE f.id = @folder_id AND f.type = 'CollectionFolder')
RETURNING id;

-- name: SetLibraryOrder :many
-- Puts libraries in the order of their folder ids; libraries not listed
-- keep their position.
UPDATE libraries l SET position = o.pos
FROM (
    SELECT f.library_id, t.ord::integer AS pos
    FROM unnest(@folder_ids::uuid[]) WITH ORDINALITY AS t(fid, ord)
    JOIN items f ON f.id = t.fid AND f.type = 'CollectionFolder'
) o
WHERE l.id = o.library_id
RETURNING l.id;

-- name: CountVisibleChildren :one
SELECT count(*) FROM items WHERE parent_id = $1 AND missing_since IS NULL;

-- name: ListUserData :many
SELECT * FROM user_data WHERE user_id = @user_id AND item_id = ANY(@item_ids::uuid[]);
