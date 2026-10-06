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

-- name: ListDebridTorrents :many
-- The torrents known for a hash, on any account (P3.8).
SELECT provider, info_hash, torrent_id, status
FROM debrid_torrents
WHERE info_hash = $1;

-- name: UpsertDebridTorrent :exec
INSERT INTO debrid_torrents (provider, info_hash, torrent_id, status)
VALUES ($1, $2, $3, $4)
ON CONFLICT (provider, info_hash) DO UPDATE
SET torrent_id = EXCLUDED.torrent_id,
    status     = EXCLUDED.status,
    checked_at = now();

-- name: DeleteDebridTorrent :exec
DELETE FROM debrid_torrents WHERE provider = $1 AND info_hash = $2;

-- name: ReadyDebridHashes :many
-- Which of the hashes are ready on some account, for the stream ranking.
SELECT DISTINCT info_hash
FROM debrid_torrents
WHERE info_hash = ANY(@hashes::text[]) AND status = 'ready';
