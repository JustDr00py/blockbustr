-- Shows and home-screen queries (TASKS P1.21).

-- name: NextUpEpisodeIDs :many
-- The next episode to watch per series (TASKS P1.21, observed on 12.1.0):
-- the episode being rewatched (the earliest half-watched one) when
-- @include_resumable is set, else the earliest unplayed episode that isn't
-- half-watched — but only for series the user has started (some episode
-- played, begun or re-watched). Ordered by the series' most recent
-- activity, then the series' sort name. total (a window count over the
-- whole result) fills TotalRecordCount when the page is full.
-- Started series are found first and one episode is picked per series
-- (DISTINCT ON), so the cost grows with the library, not its square
-- (TASKS P4.5: 9 s at 19.5k episodes before).
WITH episodes AS (
    SELECT i.id, coalesce(s.parent_id, i.parent_id) AS series_id,
           i.parent_index_number AS season, i.index_number AS number, i.premiere_date
    FROM items i
    JOIN libraries l ON l.id = i.library_id
    LEFT JOIN items s ON s.id = i.parent_id AND s.type = 'Season'
    WHERE i.type = 'Episode' AND i.missing_since IS NULL AND l.enabled
), started AS (
    SELECT e.series_id, max(ud.last_played_at) AS last_played
    FROM user_data ud
    JOIN episodes e ON e.id = ud.item_id
    WHERE ud.user_id = @user_id
    GROUP BY e.series_id
    HAVING bool_or(ud.played OR ud.play_count > 0 OR ud.playback_position_ticks > 0)
), picked AS (
    SELECT DISTINCT ON (e.series_id) e.id, e.series_id, st.last_played
    FROM episodes e
    JOIN started st ON st.series_id = e.series_id
    LEFT JOIN user_data ud ON ud.item_id = e.id AND ud.user_id = @user_id
    WHERE NOT coalesce(ud.played, false)
      AND (sqlc.narg('cutoff')::date IS NULL OR e.premiere_date IS NULL OR e.premiere_date <= @cutoff)
      AND (sqlc.narg('series_id')::uuid IS NULL OR e.series_id = @series_id)
      -- without @include_resumable, half-watched episodes are skipped
      AND (@include_resumable::boolean OR coalesce(ud.playback_position_ticks, 0) = 0)
    -- with it, the earliest half-watched episode wins over unwatched ones
    ORDER BY e.series_id, coalesce(ud.playback_position_ticks, 0) > 0 DESC,
             coalesce(e.season, -1), coalesce(e.number, -1), e.id
)
SELECT p.id, count(*) OVER () AS total
FROM picked p
ORDER BY p.last_played DESC NULLS LAST,
         (SELECT lower(coalesce(sr.forced_sort_name, sr.sort_name)) FROM items sr WHERE sr.id = p.series_id),
         p.series_id
LIMIT @lim OFFSET @start_idx;

-- name: SimilarItemIDs :many
-- Every other item of the same kind, most similar first (TASKS P1.21;
-- Jellyfin's /Items/{id}/Similar also lists items that share nothing).
-- Scored 3 points per shared genre, 2 per studio, 1 per person, ties broken
-- by the closer production year, then the sort name. The handler only asks
-- for Movie and Series; other kinds have no similar items.
SELECT i.id
FROM items i
JOIN libraries l ON l.id = i.library_id
WHERE l.enabled AND i.missing_since IS NULL
  AND i.type = @typ AND i.id <> @item_id
ORDER BY (3 * (SELECT count(*) FROM item_genres a JOIN item_genres b ON b.genre_id = a.genre_id
               WHERE a.item_id = @item_id AND b.item_id = i.id)
        + 2 * (SELECT count(*) FROM item_studios a JOIN item_studios b ON b.studio_id = a.studio_id
               WHERE a.item_id = @item_id AND b.item_id = i.id)
        + (SELECT count(*) FROM item_people a JOIN item_people b ON b.person_id = a.person_id
           WHERE a.item_id = @item_id AND b.item_id = i.id)) DESC,
         abs(coalesce(i.production_year, 0) - coalesce((SELECT s.production_year FROM items s WHERE s.id = @item_id), 0)),
         coalesce(i.forced_sort_name, i.sort_name)
LIMIT @lim;
