-- Libraries removed from config.yaml are disabled rather than deleted, so a
-- typo or a temporarily removed entry doesn't cascade away items and users'
-- watch history (TASKS P1.18).

-- +goose Up
ALTER TABLE libraries ADD COLUMN enabled boolean NOT NULL DEFAULT true;

-- +goose Down
ALTER TABLE libraries DROP COLUMN IF EXISTS enabled;
