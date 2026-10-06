-- Image serving (TASKS P1.17).

-- name: GetImage :one
SELECT * FROM images WHERE item_id = $1 AND type = $2 AND idx = $3;

-- name: SetImageInfo :exec
-- Dimensions and blurhash of the original, filled on first fetch.
UPDATE images SET width = $4, height = $5, blurhash = $6 WHERE item_id = $1 AND type = $2 AND idx = $3;

-- name: ImagesMissingInfo :many
SELECT im.item_id, im.type, im.idx, im.source_url, im.local_path, im.tag
FROM images im JOIN items i ON i.id = im.item_id
WHERE i.library_id = @library_id AND im.blurhash IS NULL
ORDER BY im.item_id, im.type, im.idx LIMIT @row_limit;

-- name: GetPerson :one
SELECT * FROM people WHERE id = $1;
