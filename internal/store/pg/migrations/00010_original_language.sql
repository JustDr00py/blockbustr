-- OriginalLanguage (TASKS P1.21): Jellyfin sends BaseItemDto.OriginalLanguage
-- for Movies and Series on the home-screen endpoints (Latest, Resume,
-- Similar, …), sourced from TMDB's original_language.

-- +goose Up
ALTER TABLE items ADD COLUMN original_language text;

-- Matched movies and series were refreshed without the language: let the
-- next metadata run fill it in (matched items otherwise wait 30 days).
UPDATE items SET metadata_refreshed_at = NULL
WHERE type IN ('Movie', 'Series') AND metadata_source <> 'none';

-- +goose Down
ALTER TABLE items DROP COLUMN IF EXISTS original_language;
