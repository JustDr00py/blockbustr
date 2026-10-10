-- name: UpsertPlayback :one
-- A play begins (the Playing report) or its stream is requested; the two
-- meet on the play session id when the client sends one. Non-empty values
-- replace stored ones, so the report adds the device and play method and
-- the stream request what was played and how. reopen (a Playing report)
-- clears an earlier stop. No row when the item or user doesn't exist.
INSERT INTO playback_log (user_id, user_name, device_name, client, item_id, item_name, runtime_ticks,
                          play_session_id, play_method, source_name, addon, delivery, link_host)
SELECT u.id, u.name, @device_name, @client, i.id,
       CASE WHEN i.type = 'Episode' THEN
              coalesce(CASE WHEN p.type = 'Series' THEN p.name WHEN g.type = 'Series' THEN g.name END || ' ', '')
              || 'S' || lpad(coalesce(i.parent_index_number, 0)::text, 2, '0')
              || 'E' || lpad(coalesce(i.index_number, 0)::text, 2, '0') || ' · ' || i.name
            ELSE i.name || coalesce(' (' || i.production_year || ')', '') END,
       i.runtime_ticks, sqlc.narg('play_session_id'), @play_method, @source_name, @addon, @delivery, @link_host
FROM items i
JOIN users u ON u.id = @user_id
LEFT JOIN items p ON p.id = i.parent_id
LEFT JOIN items g ON g.id = p.parent_id
WHERE i.id = @item_id
ON CONFLICT (play_session_id) WHERE play_session_id IS NOT NULL DO UPDATE
SET device_name  = coalesce(nullif(EXCLUDED.device_name, ''), playback_log.device_name),
    client       = coalesce(nullif(EXCLUDED.client, ''), playback_log.client),
    play_method  = coalesce(nullif(EXCLUDED.play_method, ''), playback_log.play_method),
    source_name  = coalesce(nullif(EXCLUDED.source_name, ''), playback_log.source_name),
    addon        = coalesce(nullif(EXCLUDED.addon, ''), playback_log.addon),
    delivery     = coalesce(nullif(EXCLUDED.delivery, ''), playback_log.delivery),
    link_host    = coalesce(nullif(EXCLUDED.link_host, ''), playback_log.link_host),
    last_seen_at = now(),
    stopped_at   = CASE WHEN @reopen::boolean THEN NULL ELSE playback_log.stopped_at END
RETURNING id;

-- name: UpdatePlaybackProgress :exec
-- Progress and stop reports of a logged play.
UPDATE playback_log
SET position_ticks = @position_ticks,
    play_method    = coalesce(nullif(@play_method::text, ''), play_method),
    last_seen_at   = now(),
    stopped_at     = CASE WHEN @stopped::boolean THEN now() ELSE stopped_at END
WHERE id = @id;

-- name: AddPlaybackBytes :exec
-- Bytes blockbustr proxied for a play session's stream.
UPDATE playback_log
SET bytes = bytes + @bytes, last_seen_at = now()
WHERE play_session_id = @play_session_id;

-- name: AttachPlaybackStream :one
-- What a stream request without a play session id served (Streamyfin's
-- direct play sends none): the user's latest play of the item that is
-- still reporting, or stopped in the last two minutes (the stream outlives
-- the Stopped report), gets its delivery and bytes. No row: the player's
-- Playing report hasn't come yet.
UPDATE playback_log
SET delivery     = @delivery,
    source_name  = coalesce(nullif(@source_name::text, ''), playback_log.source_name),
    addon        = coalesce(nullif(@addon::text, ''), playback_log.addon),
    link_host    = coalesce(nullif(@link_host::text, ''), playback_log.link_host),
    bytes        = playback_log.bytes + @bytes,
    last_seen_at = now()
WHERE playback_log.id = (
  SELECT p.id FROM playback_log p
  WHERE p.user_id = @user_id AND p.item_id = @item_id
    AND p.started_at > now() - interval '12 hours'
    AND (p.stopped_at IS NULL OR p.stopped_at > now() - interval '2 minutes')
  ORDER BY p.last_seen_at DESC, p.id DESC
  LIMIT 1)
RETURNING id;

-- name: ListPlaybackLog :many
-- The newest plays first, before the id given (0: from the newest), for
-- one user or (no user_id) everyone.
SELECT id, user_id, user_name, device_name, client, item_id, item_name, runtime_ticks, play_method,
       source_name, addon, delivery, link_host, bytes, position_ticks, started_at, last_seen_at, stopped_at
FROM playback_log
WHERE (sqlc.narg('user_id')::uuid IS NULL OR user_id = sqlc.narg('user_id'))
  AND (@before_id::bigint = 0 OR id < @before_id)
ORDER BY id DESC
LIMIT @lim;

-- name: PlaybackTotals :many
-- Per user since a time: plays, time spent playing (start to stop, or to
-- the last report of a play that never stopped) and bytes proxied.
SELECT user_id, user_name,
       count(*)::bigint AS plays,
       coalesce(sum(extract(epoch FROM coalesce(stopped_at, last_seen_at) - started_at)), 0)::bigint AS seconds,
       coalesce(sum(bytes), 0)::bigint AS bytes,
       max(started_at)::timestamptz AS last_played
FROM playback_log
WHERE started_at >= @since
GROUP BY user_id, user_name
ORDER BY plays DESC, user_name;

-- name: PrunePlaybackLog :execrows
DELETE FROM playback_log WHERE started_at < @before;
