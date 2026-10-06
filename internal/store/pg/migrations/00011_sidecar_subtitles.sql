-- Sidecar subtitles (TASKS P2.8): external subtitle files next to a video
-- are stored as Subtitle streams with is_external and their file's path.
-- delivery_url is sent to clients, so the path needs its own column.
-- Files with sidecars get a new source etag at the next scan, so nothing
-- needs re-probing here.

-- +goose Up
ALTER TABLE media_streams ADD COLUMN external_path text;

-- +goose Down
ALTER TABLE media_streams DROP COLUMN IF EXISTS external_path;
