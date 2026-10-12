-- What ffprobe found in an addon version (handlers.choiceProbe as JSON),
-- by choice id: a UUIDv5 of the item and the version's target. Probes
-- lived only in Redis (probe:choice:*, 30 days), so a Redis restart or
-- eviction probed every version again. A version's file doesn't change:
-- its probe is kept until the version hasn't been offered for 30 days
-- (used_at, refreshed at most daily by reads).

-- +goose Up
CREATE TABLE choice_probes (
    choice_id uuid        PRIMARY KEY,
    item_id   uuid        NOT NULL REFERENCES items (id) ON DELETE CASCADE,
    probe     jsonb       NOT NULL,
    probed_at timestamptz NOT NULL DEFAULT now(),
    used_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX choice_probes_item_id ON choice_probes (item_id);
CREATE INDEX choice_probes_used_at ON choice_probes (used_at);

-- +goose Down
DROP TABLE choice_probes;
