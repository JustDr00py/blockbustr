-- name: UpsertDebridAccount :exec
-- Config bootstrap (P3.1): one account per provider. The key is sealed by
-- internal/secret before it gets here; a changed key re-seals on the next
-- start, and enabling again is implicit.
INSERT INTO debrid_accounts (provider, api_key_enc)
VALUES ($1, $2)
ON CONFLICT (provider) DO UPDATE
SET api_key_enc = EXCLUDED.api_key_enc,
    enabled     = true,
    updated_at  = now();

-- name: ListDebridAccounts :many
SELECT id, provider, api_key_enc, enabled, priority
FROM debrid_accounts
ORDER BY priority DESC, provider;

-- name: DeleteDebridAccountByProvider :exec
DELETE FROM debrid_accounts WHERE provider = $1;
