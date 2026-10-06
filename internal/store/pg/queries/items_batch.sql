-- Batch loads for building BaseItemDtos (TASKS P1.19): one query per kind of
-- data for a whole page of items.

-- name: GetItemsByIDs :many
SELECT * FROM items WHERE id = ANY(@ids::uuid[]);

-- name: ListImagesForItems :many
SELECT * FROM images WHERE item_id = ANY(@ids::uuid[]) ORDER BY item_id, type, idx;

-- name: MediaSummaryForItems :many
-- Per item: container, video size (0 when not probed yet, e.g. a .strm
-- before first play) and whether any subtitle stream exists.
SELECT ms.item_id, ms.container,
       coalesce((SELECT max(s.width)  FROM media_streams s WHERE s.media_source_id = ms.id AND s.type = 'Video'), 0)::int AS width,
       coalesce((SELECT max(s.height) FROM media_streams s WHERE s.media_source_id = ms.id AND s.type = 'Video'), 0)::int AS height,
       EXISTS (SELECT 1 FROM media_streams s WHERE s.media_source_id = ms.id AND s.type = 'Subtitle') AS has_subtitles
FROM media_sources ms WHERE ms.item_id = ANY(@ids::uuid[]);

-- name: ListGenresForItems :many
SELECT ig.item_id, g.id, g.name FROM item_genres ig JOIN genres g ON g.id = ig.genre_id
WHERE ig.item_id = ANY(@ids::uuid[]) ORDER BY ig.item_id, ig.sort_order, g.name;

-- name: ListStudiosForItems :many
SELECT ist.item_id, s.id, s.name FROM item_studios ist JOIN studios s ON s.id = ist.studio_id
WHERE ist.item_id = ANY(@ids::uuid[]) ORDER BY ist.item_id, ist.sort_order, s.name;

-- name: ListPeopleForItems :many
SELECT ip.item_id, p.id, p.name, p.image_url, ip.kind, ip.role, ip.sort_order
FROM item_people ip JOIN people p ON p.id = ip.person_id
WHERE ip.item_id = ANY(@ids::uuid[])
ORDER BY ip.item_id, CASE ip.kind WHEN 'Actor' THEN 0 WHEN 'GuestStar' THEN 1 WHEN 'Director' THEN 2 ELSE 3 END, ip.sort_order, p.name;

-- name: UnplayedEpisodeCounts :many
-- For Series/Season UserData.UnplayedItemCount and RecursiveItemCount.
-- Also the newest episode's date (DateLastMediaAdded) and the summed runtime
-- (CumulativeRunTimeTicks).
SELECT a.ancestor AS item_id,
       count(*) AS total,
       count(*) FILTER (WHERE NOT coalesce(ud.played, false)) AS unplayed,
       max(a.date_created)::timestamptz AS last_added,
       coalesce(sum(a.runtime_ticks), 0)::bigint AS runtime_ticks
FROM (
    SELECT e.id, e.parent_id AS ancestor, e.date_created, e.runtime_ticks FROM items e
    WHERE e.type = 'Episode' AND e.missing_since IS NULL AND e.parent_id = ANY(@ids::uuid[])
    UNION ALL
    SELECT e.id, s.parent_id, e.date_created, e.runtime_ticks FROM items e JOIN items s ON s.id = e.parent_id
    WHERE e.type = 'Episode' AND e.missing_since IS NULL AND s.parent_id = ANY(@ids::uuid[])
) a
LEFT JOIN user_data ud ON ud.item_id = a.id AND ud.user_id = @user_id
GROUP BY a.ancestor;

-- name: ListChaptersForItems :many
SELECT * FROM chapters WHERE item_id = ANY(@ids::uuid[]) ORDER BY item_id, idx;

-- name: ChildCountsForItems :many
SELECT parent_id::uuid AS item_id, count(*) AS n FROM items
WHERE parent_id = ANY(@ids::uuid[]) AND missing_since IS NULL GROUP BY parent_id;

-- name: ListMediaSourcesForItems :many
SELECT * FROM media_sources WHERE item_id = ANY(@ids::uuid[]) ORDER BY item_id, name, id;

-- name: ListMediaStreamsForSources :many
SELECT * FROM media_streams WHERE media_source_id = ANY(@ids::uuid[]) ORDER BY media_source_id, idx;
