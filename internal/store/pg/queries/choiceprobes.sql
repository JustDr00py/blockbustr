-- Addon versions' probes (handlers/choiceprobe.go).

-- name: GetChoiceProbes :many
SELECT choice_id, probe, used_at FROM choice_probes
WHERE choice_id = ANY(@ids::uuid[]);

-- name: SaveChoiceProbe :exec
INSERT INTO choice_probes (choice_id, item_id, probe, probed_at, used_at)
VALUES (@choice_id, @item_id, @probe, now(), now())
ON CONFLICT (choice_id) DO UPDATE SET
    probe = EXCLUDED.probe, probed_at = now(), used_at = now();

-- name: TouchChoiceProbes :exec
-- The versions are still offered: their probes are kept.
UPDATE choice_probes SET used_at = now()
WHERE choice_id = ANY(@ids::uuid[]) AND used_at < @before;

-- name: PruneChoiceProbes :execrows
DELETE FROM choice_probes WHERE used_at < @before;
