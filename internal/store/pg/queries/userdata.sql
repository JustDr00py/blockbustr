-- User data writes (TASKS P1.24, DESIGN §3.11). Played state applies to the
-- playable items at or below an item: itself (Movie/Episode), a season's or
-- series' episodes, or everything in a library.

-- name: MarkPlayed :exec
-- PlayCount becomes at least 1 and only grows when a play date is given;
-- the last played date is the given one, else the stored one, else now.
INSERT INTO user_data (user_id, item_id, played, play_count, playback_position_ticks, last_played_at)
SELECT @user_id, i.id, true, 1, 0, coalesce(sqlc.narg('date_played')::timestamptz, now())
FROM items i
WHERE i.type IN ('Movie', 'Episode') AND i.missing_since IS NULL
  AND (i.id = @item_id OR i.parent_id = @item_id OR i.top_parent_id = @item_id
       OR i.parent_id IN (SELECT s.id FROM items s WHERE s.parent_id = @item_id))
ON CONFLICT (user_id, item_id) DO UPDATE SET
    played                  = true,
    play_count              = CASE WHEN sqlc.narg('date_played')::timestamptz IS NULL THEN greatest(user_data.play_count, 1)
                                   ELSE user_data.play_count + 1 END,
    playback_position_ticks = 0,
    last_played_at          = coalesce(sqlc.narg('date_played')::timestamptz, user_data.last_played_at, now()),
    updated_at              = now();

-- name: MarkUnplayed :exec
UPDATE user_data SET played = false, play_count = 0, playback_position_ticks = 0, last_played_at = NULL, updated_at = now()
WHERE user_id = @user_id AND item_id IN (
    SELECT i.id FROM items i
    WHERE i.type IN ('Movie', 'Episode')
      AND (i.id = @item_id OR i.parent_id = @item_id OR i.top_parent_id = @item_id
           OR i.parent_id IN (SELECT s.id FROM items s WHERE s.parent_id = @item_id)));

-- name: SetFavorite :exec
INSERT INTO user_data (user_id, item_id, is_favorite) VALUES (@user_id, @item_id, @is_favorite)
ON CONFLICT (user_id, item_id) DO UPDATE SET is_favorite = EXCLUDED.is_favorite, updated_at = now();

-- name: UpdateUserData :exec
-- NULL arguments keep the stored value.
INSERT INTO user_data (user_id, item_id, played, play_count, playback_position_ticks, is_favorite, rating, last_played_at)
VALUES (@user_id, @item_id, coalesce(sqlc.narg('played')::boolean, false), coalesce(sqlc.narg('play_count')::int, 0),
        coalesce(sqlc.narg('position_ticks')::bigint, 0), coalesce(sqlc.narg('is_favorite')::boolean, false),
        sqlc.narg('rating')::real, sqlc.narg('last_played_at')::timestamptz)
ON CONFLICT (user_id, item_id) DO UPDATE SET
    played                  = coalesce(sqlc.narg('played')::boolean, user_data.played),
    play_count              = coalesce(sqlc.narg('play_count')::int, user_data.play_count),
    playback_position_ticks = coalesce(sqlc.narg('position_ticks')::bigint, user_data.playback_position_ticks),
    is_favorite             = coalesce(sqlc.narg('is_favorite')::boolean, user_data.is_favorite),
    rating                  = coalesce(sqlc.narg('rating')::real, user_data.rating),
    last_played_at          = coalesce(sqlc.narg('last_played_at')::timestamptz, user_data.last_played_at),
    updated_at              = now();

-- name: RecordPlayback :exec
-- One playback report (TASKS P2.9, DESIGN §8.4): the resume position,
-- whether this play finished the item (played, one more play, position
-- reset by the caller), the chosen tracks, and, when touch is set, the
-- last played date.
INSERT INTO user_data (user_id, item_id, playback_position_ticks, played, play_count, last_played_at,
                       audio_stream_idx, subtitle_stream_idx)
VALUES (@user_id, @item_id, @position_ticks, @finished, CASE WHEN @finished::boolean THEN 1 ELSE 0 END,
        CASE WHEN @touch::boolean THEN now() END, sqlc.narg('audio_stream_idx'), sqlc.narg('subtitle_stream_idx'))
ON CONFLICT (user_id, item_id) DO UPDATE SET
    playback_position_ticks = EXCLUDED.playback_position_ticks,
    played                  = user_data.played OR @finished::boolean,
    play_count              = user_data.play_count + CASE WHEN @finished::boolean THEN 1 ELSE 0 END,
    last_played_at          = CASE WHEN @touch::boolean THEN now() ELSE user_data.last_played_at END,
    audio_stream_idx        = coalesce(sqlc.narg('audio_stream_idx'), user_data.audio_stream_idx),
    subtitle_stream_idx     = coalesce(sqlc.narg('subtitle_stream_idx'), user_data.subtitle_stream_idx),
    updated_at              = now();
