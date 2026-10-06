-- Library metadata: genres, studios, people (TASKS P1.16, DESIGN §4).

-- +goose Up
CREATE TABLE genres (
    id   uuid   PRIMARY KEY DEFAULT gen_random_uuid(),
    name citext NOT NULL UNIQUE
);
CREATE TABLE item_genres (
    item_id    uuid NOT NULL REFERENCES items (id) ON DELETE CASCADE,
    genre_id   uuid NOT NULL REFERENCES genres (id) ON DELETE CASCADE,
    sort_order int  NOT NULL DEFAULT 0,
    PRIMARY KEY (item_id, genre_id)
);
CREATE INDEX item_genres_genre_idx ON item_genres (genre_id);

CREATE TABLE studios (
    id   uuid   PRIMARY KEY DEFAULT gen_random_uuid(),
    name citext NOT NULL UNIQUE
);
CREATE TABLE item_studios (
    item_id    uuid NOT NULL REFERENCES items (id) ON DELETE CASCADE,
    studio_id  uuid NOT NULL REFERENCES studios (id) ON DELETE CASCADE,
    sort_order int  NOT NULL DEFAULT 0,
    PRIMARY KEY (item_id, studio_id)
);
CREATE INDEX item_studios_studio_idx ON item_studios (studio_id);

-- Person ids are deterministic (UUIDv5 of the TMDB id, or of the name for
-- nfo-only people), so the same person is one row across all titles.
CREATE TABLE people (
    id           uuid        PRIMARY KEY,
    name         text        NOT NULL,
    provider_ids jsonb       NOT NULL DEFAULT '{}',
    image_url    text,
    updated_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX people_name_trgm_idx ON people USING gin (f_unaccent(name) gin_trgm_ops);

CREATE TABLE item_people (
    item_id    uuid NOT NULL REFERENCES items (id) ON DELETE CASCADE,
    person_id  uuid NOT NULL REFERENCES people (id) ON DELETE CASCADE,
    kind       text NOT NULL CHECK (kind IN ('Actor', 'Director', 'Writer', 'Producer', 'GuestStar', 'Composer')),
    role       text,
    sort_order int  NOT NULL DEFAULT 0,
    PRIMARY KEY (item_id, person_id, kind)
);
CREATE INDEX item_people_person_idx ON item_people (person_id);

-- When metadata was last applied, and from where: 'nfo', 'tmdb', 'nfo+tmdb',
-- or 'none' (no match; retried later).
ALTER TABLE items
    ADD COLUMN metadata_refreshed_at timestamptz,
    ADD COLUMN metadata_source       text;

-- +goose Down
ALTER TABLE items DROP COLUMN IF EXISTS metadata_source, DROP COLUMN IF EXISTS metadata_refreshed_at;
DROP TABLE IF EXISTS item_people;
DROP TABLE IF EXISTS people;
DROP TABLE IF EXISTS item_studios;
DROP TABLE IF EXISTS studios;
DROP TABLE IF EXISTS item_genres;
DROP TABLE IF EXISTS genres;
