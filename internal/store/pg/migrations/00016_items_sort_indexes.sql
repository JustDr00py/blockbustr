-- TASKS P4.5: /Items pages sorted by name, coalesce(forced_sort_name,
-- sort_name) ASC NULLS FIRST, id, or newest first (DateCreated,SortName
-- descending), as itemquery.go orders them. Without an index every page
-- sorted every matching row (about 100 ms at 30k movies). These are read
-- in order and stop at the page; they cover the usual filters (type,
-- library, parent) so skipping a deep offset never touches the table.
-- +goose Up
CREATE INDEX items_sort_idx ON items ((coalesce(forced_sort_name, sort_name)) NULLS FIRST, id)
    INCLUDE (type, library_id, top_parent_id, parent_id, forced_sort_name, sort_name)
    WHERE missing_since IS NULL;
CREATE INDEX items_newest_idx ON items (date_created DESC NULLS LAST, (coalesce(forced_sort_name, sort_name)) DESC NULLS LAST, id)
    INCLUDE (type, library_id, top_parent_id, parent_id, forced_sort_name, sort_name)
    WHERE missing_since IS NULL;

-- +goose Down
DROP INDEX IF EXISTS items_newest_idx;
DROP INDEX IF EXISTS items_sort_idx;
