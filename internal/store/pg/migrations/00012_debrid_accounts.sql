-- Debrid accounts (TASKS P3.1, DESIGN §4): provider credentials for
-- Real-Debrid / TorBox. One account per provider; api_key_enc is sealed with
-- AES-256-GCM by internal/secret under config secret_key before it reaches
-- the database. The config bootstrap upserts on start; an admin API (P4.3)
-- can extend this later.

-- +goose Up
CREATE TABLE debrid_accounts (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  provider text NOT NULL UNIQUE CHECK (provider IN ('realdebrid', 'torbox')),
  api_key_enc bytea NOT NULL,
  enabled boolean NOT NULL DEFAULT true,
  priority int NOT NULL DEFAULT 0,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE debrid_accounts;
