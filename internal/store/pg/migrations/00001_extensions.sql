-- +goose Up
CREATE EXTENSION IF NOT EXISTS pgcrypto;  -- gen_random_uuid(), digest()
CREATE EXTENSION IF NOT EXISTS pg_trgm;   -- fuzzy name search
CREATE EXTENSION IF NOT EXISTS unaccent;  -- accent-insensitive search
CREATE EXTENSION IF NOT EXISTS citext;    -- case-insensitive user names

-- unaccent() is only STABLE (it depends on the dictionary search path), so it
-- can't be used in a generated column. Pin the dictionary to get an
-- IMMUTABLE wrapper.
-- +goose StatementBegin
CREATE FUNCTION f_unaccent(text) RETURNS text
    LANGUAGE sql IMMUTABLE PARALLEL SAFE STRICT
    RETURN public.unaccent('public.unaccent'::regdictionary, $1);
-- +goose StatementEnd

-- +goose Down
DROP FUNCTION IF EXISTS f_unaccent(text);
DROP EXTENSION IF EXISTS citext;
DROP EXTENSION IF EXISTS unaccent;
DROP EXTENSION IF EXISTS pg_trgm;
DROP EXTENSION IF EXISTS pgcrypto;
