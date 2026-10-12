-- The release a user keeps to in a series (stremio.Prefs.Sticky).

-- name: GetSeriesRelease :one
SELECT release_group, binge_group FROM user_series_release
WHERE user_id = @user_id AND series_id = @series_id;

-- name: SetSeriesRelease :exec
-- A play of an episode ran long enough: its release is the series' from
-- now on, for this user.
INSERT INTO user_series_release (user_id, series_id, release_group, binge_group, set_at)
VALUES (@user_id, @series_id, @release_group, @binge_group, now())
ON CONFLICT (user_id, series_id) DO UPDATE SET
    release_group = EXCLUDED.release_group, binge_group = EXCLUDED.binge_group, set_at = now();

-- name: EpisodeSeries :one
-- The series an episode belongs to (through its season, or directly).
SELECT coalesce(s.parent_id, e.parent_id)::uuid AS series_id
FROM items e
LEFT JOIN items s ON s.id = e.parent_id AND s.type = 'Season'
WHERE e.id = @episode_id AND e.type = 'Episode';
