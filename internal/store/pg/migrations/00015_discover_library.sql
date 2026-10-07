-- The hidden discover library (TASKS P3.13, DESIGN §7.4): titles found by
-- in-client search that aren't in any library are stored here, so clients
-- can open, play and favourite them by id. It has no CollectionFolder, so
-- it's never listed in /UserViews, and browsing queries leave its items
-- out (pg.ItemQuery).

-- +goose Up
ALTER TABLE libraries DROP CONSTRAINT libraries_kind_check;
ALTER TABLE libraries ADD CONSTRAINT libraries_kind_check
    CHECK (kind IN ('movies', 'tvshows', 'mixed', 'stremio', 'discover'));

-- +goose Down
DELETE FROM libraries WHERE kind = 'discover';
ALTER TABLE libraries DROP CONSTRAINT libraries_kind_check;
ALTER TABLE libraries ADD CONSTRAINT libraries_kind_check
    CHECK (kind IN ('movies', 'tvshows', 'mixed', 'stremio'));
