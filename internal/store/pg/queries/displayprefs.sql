-- Display preferences per user, key and client (TASKS P1.23).

-- name: GetDisplayPreferences :one
SELECT data FROM display_preferences WHERE user_id = $1 AND pref_id = $2 AND client = $3;

-- name: UpsertDisplayPreferences :exec
INSERT INTO display_preferences (user_id, pref_id, client, data) VALUES ($1, $2, $3, $4)
ON CONFLICT (user_id, pref_id, client) DO UPDATE SET data = EXCLUDED.data, updated_at = now();
