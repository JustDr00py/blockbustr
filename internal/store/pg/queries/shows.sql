-- Shows and home-screen queries (TASKS P1.21).

-- name: NextUpEpisodeIDs :many
-- The next episode to watch per series (TASKS P1.21, observed on 12.1.0):
-- the episode being rewatched (the earliest half-watched one) when
-- @include_resumable is set, else the earliest unplayed episode that isn't
-- half-watched — but only for series the user has started (some episode
-- played, begun or re-watched). Ordered by the series' most recent
-- activity, then the series' sort name. total (a window count over the
-- whole result) fills TotalRecordCount when the page is full.
WITH candidates AS (
    SELECT i.id,
           coalesce((SELECT s.parent_id FROM items s WHERE s.id = i.parent_id AND s.type = 'Season'), i.parent_id) AS series_id,
           i.parent_index_number AS season, i.index_number AS number,
           coalesce(ud.playback_position_ticks, 0) > 0 AS resumable
    FROM items i
    JOIN libraries l ON l.id = i.library_id
    LEFT JOIN user_data ud ON ud.item_id = i.id AND ud.user_id = @user_id
    WHERE i.type = 'Episode' AND i.missing_since IS NULL AND l.enabled
      AND NOT coalesce(ud.played, false)
      AND (sqlc.narg('cutoff')::date IS NULL OR i.premiere_date IS NULL OR i.premiere_date <= @cutoff)
), ranked AS (
    SELECT c.*, count(*) OVER () AS total
    FROM candidates c
    WHERE (sqlc.narg('series_id')::uuid IS NULL OR c.series_id = @series_id)
      AND (
          -- the earliest half-watched episode of the series
          (@include_resumable::boolean AND c.resumable AND NOT EXISTS (
              SELECT 1 FROM candidates e
              WHERE e.series_id = c.series_id AND e.resumable
                AND (coalesce(e.season, -1), coalesce(e.number, -1)) < (coalesce(c.season, -1), coalesce(c.number, -1))))
          OR
          -- else the earliest unplayed episode nobody is halfway through,
          -- unless @include_resumable already picked the resumed one above
          (NOT c.resumable
           AND NOT (@include_resumable::boolean AND EXISTS (SELECT 1 FROM candidates e WHERE e.series_id = c.series_id AND e.resumable))
           AND NOT EXISTS (
              SELECT 1 FROM candidates e
              WHERE e.series_id = c.series_id AND NOT e.resumable
                AND (coalesce(e.season, -1), coalesce(e.number, -1)) < (coalesce(c.season, -1), coalesce(c.number, -1)))))
      -- only series the user has started
      AND EXISTS (
          SELECT 1 FROM items p2
          JOIN user_data pu2 ON pu2.item_id = p2.id AND pu2.user_id = @user_id
          WHERE p2.type = 'Episode' AND p2.missing_since IS NULL
            AND coalesce((SELECT s3.parent_id FROM items s3 WHERE s3.id = p2.parent_id AND s3.type = 'Season'), p2.parent_id) = c.series_id
            AND (pu2.played OR pu2.play_count > 0 OR pu2.playback_position_ticks > 0))
)
SELECT r.id, r.total
FROM ranked r
ORDER BY (SELECT max(pu3.last_played_at)
          FROM user_data pu3 JOIN items p3 ON p3.id = pu3.item_id
          WHERE pu3.user_id = @user_id AND p3.type = 'Episode' AND p3.missing_since IS NULL
            AND coalesce((SELECT s4.parent_id FROM items s4 WHERE s4.id = p3.parent_id AND s4.type = 'Season'), p3.parent_id) = r.series_id) DESC NULLS LAST,
         (SELECT lower(coalesce(sr.forced_sort_name, sr.sort_name)) FROM items sr WHERE sr.id = r.series_id),
         r.series_id, r.season NULLS FIRST, r.number NULLS FIRST
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
