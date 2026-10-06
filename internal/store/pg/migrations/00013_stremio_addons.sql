-- Stremio addons and their catalogs (TASKS P3.4, DESIGN §4, §7).
--
-- An addon's URL often embeds its configuration, including debrid keys
-- (Torrentio, Comet…), so it is stored sealed like debrid_accounts
-- (url_enc, AES-256-GCM under secret_key). url_sha (sha256 of the base URL)
-- keeps one row per addon configuration; host is for display only.
-- Catalogs start disabled: enabling one makes a stremio library (P3.5).

-- +goose Up
CREATE TABLE stremio_addons (
  id              uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
  url_enc         bytea       NOT NULL,
  url_sha         bytea       NOT NULL UNIQUE CHECK (octet_length(url_sha) = 32),
  host            text        NOT NULL,
  manifest        jsonb       NOT NULL,
  enabled         boolean     NOT NULL DEFAULT true,
  priority        int         NOT NULL DEFAULT 0,
  last_fetched_at timestamptz NOT NULL DEFAULT now(),
  created_at      timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE stremio_catalogs (
  addon_id     uuid    NOT NULL REFERENCES stremio_addons (id) ON DELETE CASCADE,
  catalog_type text    NOT NULL,
  catalog_id   text    NOT NULL,
  name         text    NOT NULL DEFAULT '',
  library_id   uuid    REFERENCES libraries (id) ON DELETE SET NULL,
  enabled      boolean NOT NULL DEFAULT false,
  PRIMARY KEY (addon_id, catalog_type, catalog_id)
);

-- +goose Down
DROP TABLE stremio_catalogs;
DROP TABLE stremio_addons;
