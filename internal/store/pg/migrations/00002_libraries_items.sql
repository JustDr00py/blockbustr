-- DESIGN §4. Item types are text + CHECK rather than enums so adding one later
-- is a one-line migration.

-- +goose Up

-- Full-text search vector for an item, used by an expression index rather than a
-- generated column, so `SELECT *` maps cleanly onto sqlc's Item model. Queries
-- must call this same function for the index to apply.
-- +goose StatementBegin
CREATE FUNCTION item_search_vector(name text, original_title text) RETURNS tsvector
    LANGUAGE sql IMMUTABLE PARALLEL SAFE
    RETURN to_tsvector('simple', f_unaccent(coalesce(name, '') || ' ' || coalesce(original_title, '')));
-- +goose StatementEnd

CREATE TABLE libraries (
    id         uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    name       text        NOT NULL,
    kind       text        NOT NULL CHECK (kind IN ('movies', 'tvshows', 'mixed', 'stremio')),
    paths      text[]      NOT NULL DEFAULT '{}',
    options    jsonb       NOT NULL DEFAULT '{}',
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE items (
    id                  uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    library_id          uuid        NOT NULL REFERENCES libraries (id) ON DELETE CASCADE,
    parent_id           uuid        REFERENCES items (id) ON DELETE CASCADE,
    top_parent_id       uuid        REFERENCES items (id) ON DELETE CASCADE,
    type                text        NOT NULL CHECK (type IN
                            ('CollectionFolder', 'Folder', 'Movie', 'Series', 'Season', 'Episode', 'BoxSet')),
    name                text        NOT NULL,
    original_title      text,
    sort_name           text        NOT NULL,
    forced_sort_name    text,
    index_number        int,        -- episode number
    index_number_end    int,        -- multi-episode files (S01E01-E02)
    parent_index_number int,        -- season number
    production_year     int,
    premiere_date       date,
    end_date            date,
    overview            text,
    tagline             text,
    official_rating     text,
    community_rating    real,
    critic_rating       real,
    runtime_ticks       bigint,
    provider_ids        jsonb       NOT NULL DEFAULT '{}',  -- {"Tmdb":"603","Imdb":"tt0133093"}
    source_kind         text        NOT NULL DEFAULT 'virtual'
                            CHECK (source_kind IN ('file', 'strm', 'stremio', 'virtual')),
    path                text,
    strm_url            text,
    stremio_ref         jsonb,      -- {"addonId":…, "type":…, "id":…}
    etag                text,
    date_created        timestamptz NOT NULL DEFAULT now(),
    date_modified       timestamptz NOT NULL DEFAULT now(),
    date_last_refreshed timestamptz,
    is_missing          boolean     NOT NULL DEFAULT false,
    CHECK (source_kind <> 'strm' OR strm_url IS NOT NULL),
    CHECK (source_kind <> 'stremio' OR stremio_ref IS NOT NULL)
);

CREATE INDEX items_parent_sort_idx      ON items (parent_id, sort_name);
CREATE INDEX items_library_latest_idx   ON items (library_id, type, date_created DESC);
CREATE INDEX items_top_parent_type_idx  ON items (top_parent_id, type);
CREATE INDEX items_search_idx           ON items USING gin (item_search_vector(name, original_title));
CREATE INDEX items_name_trgm_idx        ON items USING gin (f_unaccent(name) gin_trgm_ops);
CREATE INDEX items_provider_ids_idx     ON items USING gin (provider_ids jsonb_path_ops);
CREATE UNIQUE INDEX items_library_path_key ON items (library_id, path) WHERE path IS NOT NULL;

-- +goose Down
DROP TABLE IF EXISTS items;
DROP TABLE IF EXISTS libraries;
DROP FUNCTION IF EXISTS item_search_vector(text, text);
