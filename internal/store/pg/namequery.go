package pg

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/sysadmin/blockbustr/internal/store/pg/db"
)

// NameKind is a by-name item list: /Genres, /Studios or /Persons.
type NameKind int

const (
	Genres NameKind = iota
	Studios
	People
)

// NameQuery lists the genres, studios or people that appear on the items
// Items matches (TASKS P1.22), so names no visible item uses are never
// listed. Items' sort and paging are ignored; names sort by name.
type NameQuery struct {
	Kind  NameKind
	Items ItemQuery

	SearchTerm                                            string
	NameStartsWith, NameStartsWithOrGreater, NameLessThan string
	PersonTypes, ExcludePersonTypes                       []string // People only
	AppearsInItemID                                       *uuid.UUID

	Descending bool
	StartIndex int
	Limit      int // 0 = no limit
}

// NameRow is one genre, studio or person with its matching item counts.
type NameRow struct {
	ID          uuid.UUID
	Name        string
	ImageURL    *string   // people only
	DateCreated time.Time // of the earliest item it appears on
	Items       int
	Movies      int
	Series      int
	Episodes    int
}

// NameQueryResult is a page of names and the total match count.
type NameQueryResult struct {
	Rows  []NameRow
	Total int
}

// QueryNames runs q.
func QueryNames(ctx context.Context, conn db.DBTX, q NameQuery) (NameQueryResult, error) {
	b := &sqlBuilder{}
	from, _ := q.Items.from(b)
	table, link, fk, image := "genres", "item_genres", "genre_id", "NULL::text"
	switch q.Kind {
	case Studios:
		table, link, fk = "studios", "item_studios", "studio_id"
	case People:
		table, link, fk, image = "people", "item_people", "person_id", "n.image_url"
	}
	const name = "n.name::text"
	var where []string
	if s := strings.TrimSpace(q.SearchTerm); s != "" {
		where = append(where, "f_unaccent("+name+") ILIKE '%' || f_unaccent("+b.arg(likeEscape(s))+") || '%'")
	}
	if q.NameStartsWith != "" {
		where = append(where, "lower("+name+") LIKE "+b.arg(strings.ToLower(likeEscape(q.NameStartsWith))+"%"))
	}
	if q.NameStartsWithOrGreater != "" {
		where = append(where, "lower("+name+") >= "+b.arg(strings.ToLower(q.NameStartsWithOrGreater)))
	}
	if q.NameLessThan != "" {
		where = append(where, "lower("+name+") < "+b.arg(strings.ToLower(q.NameLessThan)))
	}
	if q.Kind == People {
		if len(q.PersonTypes) > 0 {
			where = append(where, "lower(x.kind) = ANY("+b.arg(lowerAll(q.PersonTypes))+")")
		}
		if len(q.ExcludePersonTypes) > 0 {
			where = append(where, "NOT (lower(x.kind) = ANY("+b.arg(lowerAll(q.ExcludePersonTypes))+"))")
		}
	}
	if q.AppearsInItemID != nil {
		where = append(where, fmt.Sprintf("EXISTS (SELECT 1 FROM %s a WHERE a.%s = n.id AND a.item_id = %s)", link, fk, b.arg(*q.AppearsInItemID)))
	}
	cond := ""
	if len(where) > 0 {
		cond = "\n WHERE " + strings.Join(where, "\n AND ")
	}
	dir := "ASC"
	if q.Descending {
		dir = "DESC"
	}
	// A person can be linked to one item twice (actor and director), so
	// every count is of distinct items.
	body := fmt.Sprintf(`WITH m AS (SELECT i.id, i.type, i.date_created%s)
SELECT n.id, %s, %s, min(m.date_created), count(DISTINCT m.id),
 count(DISTINCT m.id) FILTER (WHERE m.type = 'Movie'),
 count(DISTINCT m.id) FILTER (WHERE m.type = 'Series'),
 count(DISTINCT m.id) FILTER (WHERE m.type = 'Episode'),
 count(*) OVER ()
FROM %s n JOIN %s x ON x.%s = n.id JOIN m ON m.id = x.item_id%s
GROUP BY n.id
ORDER BY lower(%s) %s, n.id`, from, name, image, table, link, fk, cond, name, dir)
	sql := body
	if q.Limit > 0 {
		sql += " LIMIT " + strconv.Itoa(q.Limit)
	}
	if q.StartIndex > 0 {
		sql += " OFFSET " + strconv.Itoa(q.StartIndex)
	}
	rows, err := conn.Query(ctx, sql, b.args...)
	if err != nil {
		return NameQueryResult{}, fmt.Errorf("query names: %w", err)
	}
	defer rows.Close()
	var res NameQueryResult
	for rows.Next() {
		var r NameRow
		var items, movies, series, episodes, total int64
		if err := rows.Scan(&r.ID, &r.Name, &r.ImageURL, &r.DateCreated, &items, &movies, &series, &episodes, &total); err != nil {
			return NameQueryResult{}, fmt.Errorf("query names: %w", err)
		}
		r.Items, r.Movies, r.Series, r.Episodes = int(items), int(movies), int(series), int(episodes)
		res.Rows, res.Total = append(res.Rows, r), int(total)
	}
	if err := rows.Err(); err != nil {
		return NameQueryResult{}, fmt.Errorf("query names: %w", err)
	}
	if len(res.Rows) == 0 && q.StartIndex > 0 { // past the end: no row carried the window count
		if err := conn.QueryRow(ctx, "SELECT count(*) FROM ("+body+") t", b.args...).Scan(&res.Total); err != nil {
			return NameQueryResult{}, fmt.Errorf("count names: %w", err)
		}
	}
	return res, nil
}

// NameID is a genre's id and name.
type NameID struct {
	ID   uuid.UUID
	Name string
}

// ItemFilterValues is what /Items/Filters[2] offers to filter items by.
type ItemFilterValues struct {
	Genres            []NameID // by name
	OfficialRatings   []string // ordinal
	Years             []int32  // ascending
	AudioLanguages    []string // stream language codes as stored
	SubtitleLanguages []string
}

// ItemFilters collects the distinct genres, ratings, years and stream
// languages of the items q matches, in one round trip (q's sort and paging
// are ignored).
func ItemFilters(ctx context.Context, conn db.DBTX, q ItemQuery) (ItemFilterValues, error) {
	b := &sqlBuilder{}
	from, _ := q.from(b)
	// Each row is (kind, text, genre id, year), sorted within each kind.
	rows, err := conn.Query(ctx, `WITH m AS (SELECT i.id, i.production_year, i.official_rating`+from+`)
SELECT kind, txt, gid, year FROM (
 SELECT DISTINCT 'genre' AS kind, g.name::text AS txt, g.id AS gid, NULL::int AS year
  FROM genres g JOIN item_genres ig ON ig.genre_id = g.id JOIN m ON m.id = ig.item_id
 UNION SELECT DISTINCT 'rating', m.official_rating, NULL::uuid, NULL::int FROM m WHERE m.official_rating <> ''
 UNION SELECT DISTINCT 'year', NULL::text, NULL::uuid, m.production_year FROM m WHERE m.production_year IS NOT NULL
 UNION SELECT DISTINCT lower(ms.type), ms.language, NULL::uuid, NULL::int
  FROM media_streams ms JOIN media_sources s ON s.id = ms.media_source_id JOIN m ON m.id = s.item_id
  WHERE ms.type IN ('Audio', 'Subtitle') AND ms.language <> ''
) f ORDER BY kind, lower(txt) COLLATE "C", txt COLLATE "C", year`, b.args...)
	if err != nil {
		return ItemFilterValues{}, fmt.Errorf("item filters: %w", err)
	}
	defer rows.Close()
	var out ItemFilterValues
	for rows.Next() {
		var kind string
		var txt *string
		var gid *uuid.UUID
		var year *int32
		if err := rows.Scan(&kind, &txt, &gid, &year); err != nil {
			return ItemFilterValues{}, fmt.Errorf("item filters: %w", err)
		}
		switch {
		case kind == "genre" && txt != nil && gid != nil:
			out.Genres = append(out.Genres, NameID{ID: *gid, Name: *txt})
		case kind == "rating" && txt != nil:
			out.OfficialRatings = append(out.OfficialRatings, *txt)
		case kind == "year" && year != nil:
			out.Years = append(out.Years, *year)
		case kind == "audio" && txt != nil:
			out.AudioLanguages = append(out.AudioLanguages, *txt)
		case kind == "subtitle" && txt != nil:
			out.SubtitleLanguages = append(out.SubtitleLanguages, *txt)
		}
	}
	if err := rows.Err(); err != nil {
		return ItemFilterValues{}, fmt.Errorf("item filters: %w", err)
	}
	return out, nil
}
