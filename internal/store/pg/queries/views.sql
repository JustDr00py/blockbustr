-- Library views (TASKS P1.18).

-- name: ListCollectionFolders :many
-- The top-level folder of every enabled library, in browse order.
SELECT i.id, i.name, i.sort_name, i.date_created, i.date_modified, i.etag,
       l.id AS library_id, l.kind, l.paths
FROM items i JOIN libraries l ON l.id = i.library_id
WHERE i.type = 'CollectionFolder' AND l.enabled
ORDER BY i.sort_name, i.id;

-- name: CountVisibleChildren :one
SELECT count(*) FROM items WHERE parent_id = $1 AND missing_since IS NULL;

-- name: ListUserData :many
SELECT * FROM user_data WHERE user_id = @user_id AND item_id = ANY(@item_ids::uuid[]);
