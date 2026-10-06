-- name: CreateLibrary :one
INSERT INTO libraries (name, kind, paths)
VALUES ($1, $2, $3)
RETURNING *;

-- name: ListLibraries :many
SELECT * FROM libraries ORDER BY name;

-- name: CreateItem :one
-- Minimal insert used by tests and the scanner bootstrap; the scanner (P1.14)
-- adds a full upsert.
INSERT INTO items (library_id, parent_id, top_parent_id, type, name, sort_name,
                   source_kind, path, strm_url, index_number, parent_index_number, production_year)
VALUES (@library_id, sqlc.narg('parent_id'), sqlc.narg('top_parent_id'), @type, @name, @sort_name,
        @source_kind, sqlc.narg('path'), sqlc.narg('strm_url'),
        sqlc.narg('index_number'), sqlc.narg('parent_index_number'), sqlc.narg('production_year'))
RETURNING *;

-- name: GetItem :one
SELECT * FROM items WHERE id = $1;

-- name: ListChildren :many
-- Browse order: forced sort name wins, id breaks ties so paging is stable.
SELECT * FROM items
WHERE parent_id = @parent_id
ORDER BY coalesce(forced_sort_name, sort_name), id
LIMIT sqlc.arg('row_limit') OFFSET sqlc.arg('row_offset');

-- name: CountChildren :one
SELECT count(*) FROM items WHERE parent_id = $1;
