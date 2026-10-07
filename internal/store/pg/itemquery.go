package pg

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"unicode"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sysadmin/blockbustr/internal/store/pg/db"
)

// ItemQuery is the filter/sort/page set behind Jellyfin's /Items (TASKS
// P1.19, DESIGN §3.6). The filter space is too large for static sqlc queries,
// so this builds parameterised SQL; every value goes in as a bind argument.
//
// Always applied: only items of enabled libraries, never missing items.
type ItemQuery struct {
	UserID uuid.UUID // whose played/favourite state filters and sorts use

	ParentID  *uuid.UUID
	Recursive bool

	IncludeTypes, ExcludeTypes []string
	MediaTypes                 []string // Jellyfin MediaType: Video, Audio, …
	IDs, ExcludeIDs            []uuid.UUID
	PersonIDs, GenreIDs        []uuid.UUID
	StudioIDs                  []uuid.UUID
	Genres                     []string // by name
	Years                      []int32
	OfficialRatings            []string
	ProviderIDs                map[string][]string // AnyProviderIdEquals: provider → ids (any matches)

	SearchTerm                                            string
	NameStartsWith, NameStartsWithOrGreater, NameLessThan string

	IsPlayed, IsFavorite, IsResumable, IsFolder *bool
	MinCommunityRating                          *float64

	SortBy     []SortKey
	StartIndex int
	Limit      int // 0 = no limit
	Count      bool
	// Counts, when set, supplies the total from a cache (search totals
	// are always counted: found titles join the discover library).
	Counts Counter

	// Access limits the result to what a user may see; QueryItems takes it
	// from the context (WithAccess) when it's unset.
	Access Access
}

// SortKey is one Jellyfin ItemSortBy value and its direction.
type SortKey struct {
	By         string
	Descending bool
}

// ItemQueryResult is a page of items and the total match count (the page
// length when the query didn't ask for a count, as Jellyfin reports it).
// Counter returns the total of the count query identified by key, calling
// count when it doesn't have it.
type Counter func(ctx context.Context, key string, count func() (int, error)) (int, error)

type ItemQueryResult struct {
	Items []db.Item
	Total int
}

// itemColumns matches db.Item field order (see scanItem).
const itemColumns = `i.id, i.library_id, i.parent_id, i.top_parent_id, i.type, i.name, i.original_title, i.sort_name,
 i.forced_sort_name, i.index_number, i.index_number_end, i.parent_index_number, i.production_year, i.premiere_date,
 i.end_date, i.overview, i.tagline, i.official_rating, i.community_rating, i.critic_rating, i.runtime_ticks,
 i.provider_ids, i.source_kind, i.path, i.strm_url, i.stremio_ref, i.etag, i.date_created, i.date_modified,
 i.date_last_refreshed, i.is_missing, i.missing_since, i.metadata_refreshed_at, i.metadata_source, i.original_language`

func scanItem(row pgx.CollectableRow) (db.Item, error) {
	var i db.Item
	err := row.Scan(&i.ID, &i.LibraryID, &i.ParentID, &i.TopParentID, &i.Type, &i.Name, &i.OriginalTitle, &i.SortName,
		&i.ForcedSortName, &i.IndexNumber, &i.IndexNumberEnd, &i.ParentIndexNumber, &i.ProductionYear, &i.PremiereDate,
		&i.EndDate, &i.Overview, &i.Tagline, &i.OfficialRating, &i.CommunityRating, &i.CriticRating, &i.RuntimeTicks,
		&i.ProviderIds, &i.SourceKind, &i.Path, &i.StrmUrl, &i.StremioRef, &i.Etag, &i.DateCreated, &i.DateModified,
		&i.DateLastRefreshed, &i.IsMissing, &i.MissingSince, &i.MetadataRefreshedAt, &i.MetadataSource, &i.OriginalLanguage)
	return i, err
}

// Episodes under item i (a Series via its seasons, or a Season directly).
const descendantEpisodes = `SELECT 1 FROM items e WHERE e.type = 'Episode' AND e.missing_since IS NULL
 AND (e.parent_id = i.id OR e.parent_id IN (SELECT s.id FROM items s WHERE s.parent_id = i.id))`

// playedExpr is Jellyfin's Played: a Series/Season is played when it has
// episodes and the user played all of them.
func playedExpr(user string) string {
	return fmt.Sprintf(`(CASE WHEN i.type IN ('Series','Season') THEN
 EXISTS (%[1]s) AND NOT EXISTS (%[1]s AND NOT EXISTS (SELECT 1 FROM user_data pu WHERE pu.item_id = e.id AND pu.user_id = %[2]s AND pu.played))
 ELSE coalesce(ud.played, false) END)`, descendantEpisodes, user)
}

// Jellyfin ItemSortBy (lower-cased) → SQL. Unknown keys are ignored, as in Jellyfin.
var sortExprs = map[string]string{
	"sortname":             "coalesce(i.forced_sort_name, i.sort_name)",
	"name":                 "lower(i.name)",
	"premieredate":         "i.premiere_date",
	"productionyear":       "i.production_year",
	"datecreated":          "i.date_created",
	"dateadded":            "i.date_created",
	"datelastcontentadded": "coalesce((SELECT max(c.date_created) FROM items c WHERE c.parent_id = i.id OR c.parent_id IN (SELECT s.id FROM items s WHERE s.parent_id = i.id)), i.date_created)",
	"dateplayed":           "ud.last_played_at",
	"communityrating":      "i.community_rating",
	"criticrating":         "i.critic_rating",
	"runtime":              "i.runtime_ticks",
	"playcount":            "coalesce(ud.play_count, 0)",
	"isfavoriteorliked":    "coalesce(ud.is_favorite, false)",
	"officialrating":       "i.official_rating",
	"indexnumber":          "i.index_number",
	"parentindexnumber":    "i.parent_index_number",
	"airedepisodeorder":    "i.parent_index_number, i.index_number",
	"seriessortname":       "(SELECT coalesce(sr.forced_sort_name, sr.sort_name) FROM items sr WHERE sr.id = (SELECT s.parent_id FROM items s WHERE s.id = i.parent_id))",
	"random":               "random()",
	"isplayed":             "", // need the user argument; built in build()
	"isunplayed":           "",
}

type sqlBuilder struct {
	args  []any
	where []string
}

func (b *sqlBuilder) arg(v any) string {
	b.args = append(b.args, v)
	return "$" + strconv.Itoa(len(b.args))
}

func (b *sqlBuilder) and(cond string) { b.where = append(b.where, cond) }

// tsPrefixQuery turns free text into a prefix tsquery ("mort komb" →
// 'mort':* & 'komb':*), or "" when there are no word characters. Only
// letters and digits survive, so the result is always valid tsquery syntax.
func tsPrefixQuery(s string) string {
	var words []string
	for _, w := range strings.FieldsFunc(s, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
		words = append(words, "'"+w+"':*")
	}
	return strings.Join(words, " & ")
}

func likeEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

// canonicalTypes maps a lower-cased item type to its stored spelling.
var canonicalTypes = map[string]string{
	"collectionfolder": "CollectionFolder", "folder": "Folder", "movie": "Movie", "series": "Series",
	"season": "Season", "episode": "Episode", "boxset": "BoxSet",
}

// itemTypes spells client item types as stored, so a filter compares i.type
// itself: lower(i.type) hid the type's row count from the planner, which
// then picked plans for a few hundred rows instead of tens of thousands
// (TASKS P4.5). Types items never have (LiveTvProgram…) are kept and match
// nothing.
func itemTypes(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		if c, ok := canonicalTypes[strings.ToLower(s)]; ok {
			s = c
		}
		out[i] = s
	}
	return out
}

func lowerAll(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = strings.ToLower(s)
	}
	return out
}

// withAccess fills q's Access from ctx unless it was set.
func (q ItemQuery) withAccess(ctx context.Context) ItemQuery {
	if !q.Access.Restricted() {
		q.Access = AccessFrom(ctx)
	}
	return q
}

// reachesDiscover reports whether q may return items of the hidden
// discover library (search results, DESIGN §7.4): by id, as a parent's
// children, or by the user's own state (played, favourite, resumable).
// Browsing (Latest, all movies, genres…) never shows them, and nor does
// the library part of a search: remote matches come after the library's
// (handlers.withRemoteMatches), found titles included.
func (q ItemQuery) reachesDiscover() bool {
	return len(q.IDs) > 0 || q.ParentID != nil || q.IsPlayed != nil || q.IsFavorite != nil || q.IsResumable != nil
}

// mediaTypeItems maps Jellyfin MediaType to the item types blockbustr has.
var mediaTypeItems = map[string][]string{"video": {"Movie", "Episode"}}

// folderTypes are the item types Jellyfin reports with IsFolder true.
const folderTypes = "('CollectionFolder','Folder','Series','Season','BoxSet')"

// from adds q's filters to b and returns the FROM … WHERE clause over items
// i (with libraries l and the user's user_data ud), plus the placeholder of
// the user argument. Other queries reuse it to range over the same items.
func (q ItemQuery) from(b *sqlBuilder) (from, user string) {
	user = b.arg(q.UserID)
	// Libraries are a filter (an array of ids), not a join: the join
	// halved the planner's row estimates, so deep pages sorted every row
	// instead of reading the sort index (TASKS P4.5).
	libs := "l.enabled"
	if !q.reachesDiscover() {
		libs += " AND l.kind <> 'discover'"
	}
	b.and("i.library_id = ANY(ARRAY(SELECT l.id FROM libraries l WHERE " + libs + "))")
	b.and("i.missing_since IS NULL")
	q.Access.apply(b)

	switch {
	case q.ParentID != nil && q.Recursive:
		p := b.arg(*q.ParentID)
		// top_parent_id is the library folder for every descendant; below a
		// library the hierarchy is at most Series → Season → Episode.
		// The seasons' ids are an array (= ANY), not a hashed subplan, so
		// every branch can use an index (TASKS P4.5).
		b.and(fmt.Sprintf("(i.top_parent_id = %[1]s OR i.parent_id = %[1]s OR i.parent_id = ANY(ARRAY(SELECT s.id FROM items s WHERE s.parent_id = %[1]s)))", p))
	case q.ParentID != nil:
		b.and("i.parent_id = " + b.arg(*q.ParentID))
	case !q.Recursive && len(q.IDs) == 0:
		b.and("i.type = 'CollectionFolder'") // the user root's children are the libraries
	}
	if len(q.IncludeTypes) > 0 {
		b.and("i.type = ANY(" + b.arg(itemTypes(q.IncludeTypes)) + ")")
	} else if q.Recursive {
		b.and("i.type <> 'CollectionFolder'")
	}
	if len(q.ExcludeTypes) > 0 {
		b.and("NOT (i.type = ANY(" + b.arg(itemTypes(q.ExcludeTypes)) + "))")
	}
	if len(q.MediaTypes) > 0 {
		types := []string{}
		for _, m := range q.MediaTypes {
			types = append(types, mediaTypeItems[strings.ToLower(m)]...)
		}
		b.and("i.type = ANY(" + b.arg(types) + ")")
	}
	if len(q.IDs) > 0 {
		b.and("i.id = ANY(" + b.arg(q.IDs) + ")")
	}
	if len(q.ExcludeIDs) > 0 {
		b.and("NOT (i.id = ANY(" + b.arg(q.ExcludeIDs) + "))")
	}
	if len(q.PersonIDs) > 0 {
		b.and("EXISTS (SELECT 1 FROM item_people ip WHERE ip.item_id = i.id AND ip.person_id = ANY(" + b.arg(q.PersonIDs) + "))")
	}
	if len(q.GenreIDs) > 0 {
		b.and("EXISTS (SELECT 1 FROM item_genres ig WHERE ig.item_id = i.id AND ig.genre_id = ANY(" + b.arg(q.GenreIDs) + "))")
	}
	if len(q.Genres) > 0 {
		b.and("EXISTS (SELECT 1 FROM item_genres ig JOIN genres g ON g.id = ig.genre_id WHERE ig.item_id = i.id AND lower(g.name) = ANY(" + b.arg(lowerAll(q.Genres)) + "))")
	}
	if len(q.StudioIDs) > 0 {
		b.and("EXISTS (SELECT 1 FROM item_studios st WHERE st.item_id = i.id AND st.studio_id = ANY(" + b.arg(q.StudioIDs) + "))")
	}
	if len(q.Years) > 0 {
		b.and("i.production_year = ANY(" + b.arg(q.Years) + ")")
	}
	if len(q.OfficialRatings) > 0 {
		b.and("i.official_rating = ANY(" + b.arg(q.OfficialRatings) + ")")
	}
	if len(q.ProviderIDs) > 0 {
		var ors []string
		for prov, ids := range q.ProviderIDs {
			ors = append(ors, fmt.Sprintf("EXISTS (SELECT 1 FROM jsonb_each_text(i.provider_ids) pv WHERE lower(pv.key) = %s AND pv.value = ANY(%s))",
				b.arg(strings.ToLower(prov)), b.arg(ids)))
		}
		b.and("(" + strings.Join(ors, " OR ") + ")")
	}
	search := strings.TrimSpace(q.SearchTerm)
	if search != "" {
		cond := "f_unaccent(i.name) ILIKE '%' || f_unaccent(" + b.arg(likeEscape(search)) + ") || '%'"
		if ts := tsPrefixQuery(search); ts != "" {
			cond = "(item_search_vector(i.name, i.original_title) @@ to_tsquery('simple', f_unaccent(" + b.arg(ts) + ")) OR " + cond + ")"
		}
		b.and(cond)
	}
	sortName := "lower(coalesce(i.forced_sort_name, i.sort_name))"
	if q.NameStartsWith != "" {
		b.and(sortName + " LIKE " + b.arg(strings.ToLower(likeEscape(q.NameStartsWith))+"%"))
	}
	if q.NameStartsWithOrGreater != "" {
		b.and(sortName + " >= " + b.arg(strings.ToLower(q.NameStartsWithOrGreater)))
	}
	if q.NameLessThan != "" {
		b.and(sortName + " < " + b.arg(strings.ToLower(q.NameLessThan)))
	}
	played := playedExpr(user)
	if q.IsPlayed != nil {
		b.and(played + " = " + b.arg(*q.IsPlayed))
	}
	if q.IsFavorite != nil {
		b.and("coalesce(ud.is_favorite, false) = " + b.arg(*q.IsFavorite))
	}
	if q.IsResumable != nil {
		b.and("(coalesce(ud.playback_position_ticks, 0) > 0) = " + b.arg(*q.IsResumable))
	}
	if q.IsFolder != nil {
		b.and("(i.type IN " + folderTypes + ") = " + b.arg(*q.IsFolder))
	}
	if q.MinCommunityRating != nil {
		b.and("i.community_rating >= " + b.arg(*q.MinCommunityRating))
	}
	return ` FROM items i
 LEFT JOIN user_data ud ON ud.item_id = i.id AND ud.user_id = ` + user + `
 WHERE ` + strings.Join(b.where, "\n AND "), user
}

func (q ItemQuery) build() (sql string, args []any, countSQL string, countArgs []any) {
	b := &sqlBuilder{}
	from, user := q.from(b)
	played := playedExpr(user)
	search := strings.TrimSpace(q.SearchTerm)

	// Arguments from here on are ORDER BY-only, so the count query (WHERE
	// only) must not receive them.
	whereArgs := len(b.args)
	var order []string
	for _, k := range q.SortBy {
		key := strings.ToLower(k.By)
		expr, ok := sortExprs[key]
		if !ok {
			continue
		}
		switch key {
		case "isplayed":
			expr = played
		case "isunplayed":
			expr = "NOT " + played
		}
		// Jellyfin compares nulls as the minimum value.
		dir := " ASC NULLS FIRST"
		if k.Descending {
			dir = " DESC NULLS LAST"
		}
		for _, e := range splitTopLevel(expr) {
			order = append(order, e+dir)
		}
	}
	if len(order) == 0 {
		if search != "" {
			// Exact title, then title prefix, then closeness.
			term, like := b.arg(search), b.arg(likeEscape(search))
			order = append(order, "(lower(f_unaccent(i.name)) = lower(f_unaccent("+term+"::text))) DESC",
				"(f_unaccent(i.name) ILIKE f_unaccent("+like+"::text) || '%') DESC",
				"similarity(f_unaccent(i.name), f_unaccent("+term+"::text)) DESC")
		}
		order = append(order, "coalesce(i.forced_sort_name, i.sort_name)")
	}
	order = append(order, "i.id")

	// The page's ids are picked first and only those rows read: sorting
	// ids is cheap, while sorting whole rows to skip a deep offset spilled
	// to disk (TASKS P4.5).
	page := "SELECT i.id" + from + "\n ORDER BY " + strings.Join(order, ", ")
	if q.Limit > 0 {
		page += " LIMIT " + strconv.Itoa(q.Limit)
	}
	if q.StartIndex > 0 {
		page += " OFFSET " + strconv.Itoa(q.StartIndex)
	}
	sql = "SELECT " + itemColumns + " FROM unnest(ARRAY(" + page + ")) WITH ORDINALITY AS page(id, n)" +
		"\n JOIN items i ON i.id = page.id ORDER BY page.n"
	return sql, b.args, "SELECT count(*)" + from, b.args[:whereArgs]
}

// splitTopLevel splits "a, b" sort expressions at commas outside parentheses.
func splitTopLevel(s string) []string {
	var out []string
	depth, start := 0, 0
	for i, r := range s {
		switch r {
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				out = append(out, strings.TrimSpace(s[start:i]))
				start = i + 1
			}
		}
	}
	return append(out, strings.TrimSpace(s[start:]))
}

// QueryItems runs q. The count query only runs when the page can't tell the
// total by itself (a full page, or a page past the end).
func QueryItems(ctx context.Context, conn db.DBTX, q ItemQuery) (ItemQueryResult, error) {
	q = q.withAccess(ctx)
	sql, args, countSQL, countArgs := q.build()
	rows, err := conn.Query(ctx, sql, args...)
	if err != nil {
		return ItemQueryResult{}, fmt.Errorf("query items: %w", err)
	}
	items, err := pgx.CollectRows(rows, scanItem)
	if err != nil {
		return ItemQueryResult{}, fmt.Errorf("query items: %w", err)
	}
	res := ItemQueryResult{Items: items, Total: q.StartIndex + len(items)}
	if !q.Count {
		res.Total = len(items)
		return res, nil
	}
	if (q.Limit > 0 && len(items) == q.Limit) || (len(items) == 0 && q.StartIndex > 0) {
		count := func() (n int, err error) {
			err = conn.QueryRow(ctx, countSQL, countArgs...).Scan(&n)
			return n, err
		}
		var err error
		if q.Counts != nil && strings.TrimSpace(q.SearchTerm) == "" {
			res.Total, err = q.Counts(ctx, countSQL+"\x00"+fmt.Sprint(countArgs...), count)
		} else {
			res.Total, err = count()
		}
		if err != nil {
			return ItemQueryResult{}, fmt.Errorf("count items: %w", err)
		}
	}
	return res, nil
}
