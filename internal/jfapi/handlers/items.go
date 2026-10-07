package handlers

import (
	"context"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/sysadmin/blockbustr/internal/auth"
	"github.com/sysadmin/blockbustr/internal/jfapi"
	"github.com/sysadmin/blockbustr/internal/jfapi/dto"
	"github.com/sysadmin/blockbustr/internal/store/pg"
	"github.com/sysadmin/blockbustr/internal/store/pg/db"
)

// RemoteSearch finds titles outside the library for in-client search
// (DESIGN §7.4, TASKS P3.12+). It returns items it has already stored (in
// the hidden discover library), so clients can address them like any other.
// types is a subset of {Movie, Series}.
type RemoteSearch interface {
	SearchItems(ctx context.Context, term string, types []string, limit int) ([]db.Item, error)
	// EnsureEpisodes gives a series found by search its seasons and
	// episodes before they're listed; it leaves other items alone.
	EnsureEpisodes(ctx context.Context, series db.Item) error
}

// ensureEpisodes fills a search-found series' episodes before they're
// listed (DESIGN §7.4 step 4). A failure only leaves it empty for now.
func (a *api) ensureEpisodes(r *http.Request, it db.Item) {
	if a.RemoteSearch == nil || it.Type != "Series" {
		return
	}
	if err := a.RemoteSearch.EnsureEpisodes(r.Context(), it); err != nil {
		a.Log.WarnContext(r.Context(), "series episodes unavailable", "series", it.ID, "err", err)
	}
}

func (a *api) registerItems(rt *jfapi.Router) {
	rt.Get("/Items", a.requireUser(a.getItems))
	rt.Get("/Users/{userId}/Items", a.requireUser(a.getItems)) // legacy route
	rt.Get("/Items/Latest", a.requireUser(a.getLatestMedia))
	rt.Get("/UserItems/Resume", a.requireUser(a.getResume))
	// Legacy forms Infuse uses for its home rows; without them the legacy
	// item route takes "Latest"/"Resume" for an item id and answers 404.
	rt.Get("/Users/{userId}/Items/Latest", a.requireUser(a.getLatestMedia))
	rt.Get("/Users/{userId}/Items/Resume", a.requireUser(a.getResume))
	rt.Get("/Items/Suggestions", a.requireUser(a.getSuggestions))
	rt.Get("/Users/{userId}/Suggestions", a.requireUser(a.getSuggestions)) // legacy route
}

// getSuggestions serves /Items/Suggestions (Findroid's and Streamyfin's
// home row): random items of the `type` list (Findroid: Movie,Series),
// narrowed by `mediaType` (Streamyfin: Video), with the full detail field
// set Jellyfin 12.1.0 sends there.
func (a *api) getSuggestions(w http.ResponseWriter, r *http.Request, s auth.Session) {
	user, ok := queryUser(r, s)
	if !ok {
		errorText(w, http.StatusForbidden)
		return
	}
	q := jfapi.QueryOf(r)
	iq := pg.ItemQuery{
		UserID: user, Recursive: true, IncludeTypes: q.List("type"), MediaTypes: q.List("mediaType"),
		SortBy: []pg.SortKey{{By: "Random"}}, Count: true,
	}
	pageParams(q, &iq)
	res, err := pg.QueryItems(r.Context(), a.DB, iq)
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	a.writeItemPage(w, r, user, res.Items, detailOptions(), res.Total, iq.StartIndex)
}

// queryUser is the user a request acts for: the session's, or for an admin,
// the userId it names. ok is false when a non-admin names someone else.
func queryUser(r *http.Request, s auth.Session) (uuid.UUID, bool) {
	raw := jfapi.URLParam(r, "userId")
	if raw == "" {
		raw = jfapi.QueryOf(r).Get("userId")
	}
	if raw == "" {
		return s.UserID, true
	}
	id, err := dto.ParseID(raw)
	if err != nil || id.UUID() == s.UserID {
		return s.UserID, err == nil
	}
	return id.UUID(), s.IsAdmin
}

// idList parses a comma-separated GUID list; unparsable entries are skipped
// (clients send leading commas: "excludeItemIds=,d48b…").
func idList(q jfapi.Query, name string) []uuid.UUID {
	var out []uuid.UUID
	for _, s := range q.List(name) {
		if id, err := dto.ParseID(s); err == nil {
			out = append(out, id.UUID())
		}
	}
	return out
}

// pipeList splits Jellyfin's "|"-delimited name lists (genres, officialRatings).
func pipeList(q jfapi.Query, name string) []string {
	var out []string
	for _, v := range strings.Split(q.Get(name), "|") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func optBool(q jfapi.Query, name string) *bool {
	if b, ok := q.Bool(name); ok {
		return &b
	}
	return nil
}

// itemQueryFrom maps /Items query parameters onto pg.ItemQuery. ok is false
// when the query can't match anything (e.g. isMissing=true: blockbustr never
// lists missing items).
func (a *api) itemQueryFrom(q jfapi.Query, user uuid.UUID) (pg.ItemQuery, bool) {
	iq := pg.ItemQuery{
		UserID: user, IncludeTypes: q.List("includeItemTypes"), ExcludeTypes: q.List("excludeItemTypes"),
		MediaTypes: q.List("mediaTypes"), IDs: idList(q, "ids"), ExcludeIDs: idList(q, "excludeItemIds"),
		PersonIDs: idList(q, "personIds"), GenreIDs: idList(q, "genreIds"), StudioIDs: idList(q, "studioIds"),
		Genres: pipeList(q, "genres"), OfficialRatings: pipeList(q, "officialRatings"),
		SearchTerm: q.Get("searchTerm"), NameStartsWith: q.Get("nameStartsWith"),
		NameStartsWithOrGreater: q.Get("nameStartsWithOrGreater"), NameLessThan: q.Get("nameLessThan"),
		IsPlayed: optBool(q, "isPlayed"), IsFavorite: optBool(q, "isFavorite"), Count: true,
	}
	if b, ok := q.Bool("isMissing"); ok && b {
		return iq, false
	}
	if b, ok := q.Bool("enableTotalRecordCount"); ok {
		iq.Count = b
	}
	iq.Recursive, _ = q.Bool("recursive")
	if p := q.Get("parentId"); p != "" {
		id, err := dto.ParseID(p)
		if err != nil {
			return iq, false
		}
		if id != rootFolderID(a.ServerID) { // the user root: same as no parent
			iq.ParentID = ptr(id.UUID())
		}
	}
	for _, y := range q.List("years") {
		if n, err := strconv.Atoi(y); err == nil {
			iq.Years = append(iq.Years, int32(n))
		}
	}
	for _, pair := range q.List("anyProviderIdEquals") {
		if prov, id, ok := strings.Cut(pair, "."); ok && prov != "" && id != "" {
			if iq.ProviderIDs == nil {
				iq.ProviderIDs = map[string][]string{}
			}
			iq.ProviderIDs[prov] = append(iq.ProviderIDs[prov], id)
		}
	}
	if v, err := strconv.ParseFloat(q.Get("minCommunityRating"), 64); err == nil {
		iq.MinCommunityRating = &v
	}
	t := true
	f := false
	for _, flt := range q.List("filters") {
		switch strings.ToLower(flt) {
		case "isfolder":
			iq.IsFolder = &t
		case "isnotfolder":
			iq.IsFolder = &f
		case "isplayed":
			iq.IsPlayed = &t
		case "isunplayed":
			iq.IsPlayed = &f
		case "isfavorite", "isfavoriteorlikes", "likes":
			iq.IsFavorite = &t
		case "isresumable":
			iq.IsResumable = &t
		}
	}
	orders := q.List("sortOrder")
	for i, by := range q.List("sortBy") {
		ord := "Ascending"
		if i < len(orders) {
			ord = orders[i]
		} else if len(orders) > 0 {
			ord = orders[0]
		}
		iq.SortBy = append(iq.SortBy, pg.SortKey{By: by, Descending: strings.EqualFold(ord, "Descending")})
	}
	if n, ok := q.Int("startIndex"); ok && n > 0 {
		iq.StartIndex = n
	}
	if n, ok := q.Int("limit"); ok && n > 0 {
		iq.Limit = n
	}
	return iq, true
}

// itemsInIDOrder loads ids with GetItemsByIDs and returns them in the given
// order, skipping ids that no longer exist.
func (a *api) itemsInIDOrder(ctx context.Context, ids []uuid.UUID) ([]db.Item, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := a.Queries.GetItemsByIDs(ctx, ids)
	if err != nil {
		return nil, err
	}
	byID := make(map[uuid.UUID]db.Item, len(rows))
	for _, it := range rows {
		byID[it.ID] = it
	}
	out := make([]db.Item, 0, len(ids))
	for _, id := range ids {
		if it, ok := byID[id]; ok {
			out = append(out, it)
		}
	}
	return out, nil
}

// remoteTypes is which remote kinds a search may return given its type
// filters (DESIGN §7.4): remote results are only ever Movies and Series.
func remoteTypes(iq pg.ItemQuery) []string {
	types := []string{"Movie", "Series"}
	keep := func(t string) bool {
		if len(iq.IncludeTypes) > 0 && !slices.ContainsFunc(iq.IncludeTypes, func(s string) bool { return strings.EqualFold(s, t) }) {
			return false
		}
		if slices.ContainsFunc(iq.ExcludeTypes, func(s string) bool { return strings.EqualFold(s, t) }) {
			return false
		}
		if len(iq.MediaTypes) > 0 && t == "Series" { // Series aren't Video
			return false
		}
		if len(iq.MediaTypes) > 0 && !slices.ContainsFunc(iq.MediaTypes, func(s string) bool { return strings.EqualFold(s, "Video") }) {
			return false
		}
		return true
	}
	return slices.DeleteFunc(types, func(t string) bool { return !keep(t) })
}

func (a *api) getItems(w http.ResponseWriter, r *http.Request, s auth.Session) {
	user, ok := queryUser(r, s)
	if !ok {
		errorText(w, http.StatusForbidden)
		return
	}
	q := jfapi.QueryOf(r)
	iq, ok := a.itemQueryFrom(q, user)
	if !ok {
		jfapi.WriteJSON(w, r, http.StatusOK, queryResult([]dto.BaseItemDto{}))
		return
	}
	if iq.ParentID != nil && a.RemoteSearch != nil {
		if parent, ok, err := a.visibleItem(r, user, *iq.ParentID); err == nil && ok {
			a.ensureEpisodes(r, parent)
		}
	}
	res, err := pg.QueryItems(r.Context(), a.DB, iq)
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	items := a.withRemoteMatches(r, iq, res.Items)
	if added := len(items) - len(res.Items); added > 0 {
		res.Total += added
	}
	dtos, err := a.itemDtos(r.Context(), &user, items, dtoOptionsFrom(q))
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	jfapi.WriteJSON(w, r, http.StatusOK, dto.BaseItemDtoQueryResult{
		Items: &dtos, TotalRecordCount: ptr(int32(res.Total)), StartIndex: ptr(int32(iq.StartIndex)),
	})
}

// withRemoteMatches appends remote search results after the library's on a
// search's first page (DESIGN §7.4). A failing source never fails the search.
func (a *api) withRemoteMatches(r *http.Request, iq pg.ItemQuery, local []db.Item) []db.Item {
	term := strings.TrimSpace(iq.SearchTerm)
	if a.RemoteSearch == nil || term == "" || iq.StartIndex > 0 || len(iq.IDs) > 0 || iq.ParentID != nil {
		return local
	}
	types := remoteTypes(iq)
	room := iq.Limit - len(local)
	if len(types) == 0 || (iq.Limit > 0 && room <= 0) {
		return local
	}
	remote, err := a.RemoteSearch.SearchItems(r.Context(), term, types, room)
	if err != nil {
		a.Log.WarnContext(r.Context(), "remote search failed", "err", err)
		return local
	}
	seen := make(map[uuid.UUID]bool, len(local))
	for _, it := range local {
		seen[it.ID] = true
	}
	for _, it := range remote {
		if !seen[it.ID] && (iq.Limit <= 0 || len(local) < iq.Limit) {
			local = append(local, it)
			seen[it.ID] = true
		}
	}
	return local
}

// pageParams reads startIndex/limit into an ItemQuery (limit 0 = all) and
// applies enableTotalRecordCount to Count (Jellyfin counts by default).
func pageParams(q jfapi.Query, iq *pg.ItemQuery) {
	if b, ok := q.Bool("enableTotalRecordCount"); ok {
		iq.Count = b
	}
	if n, ok := q.Int("startIndex"); ok && n > 0 {
		iq.StartIndex = n
	}
	if n, ok := q.Int("limit"); ok && n > 0 {
		iq.Limit = n
	}
}

// getResume serves /UserItems/Resume (TASKS P1.21): items the user stopped
// part-way, most recently played first. excludeActiveSessions waits for
// live sessions (P2.9).
func (a *api) getResume(w http.ResponseWriter, r *http.Request, s auth.Session) {
	user, ok := queryUser(r, s)
	if !ok {
		errorText(w, http.StatusForbidden)
		return
	}
	q := jfapi.QueryOf(r)
	iq := pg.ItemQuery{
		UserID: user, Recursive: true, IncludeTypes: q.List("includeItemTypes"), MediaTypes: q.List("mediaTypes"),
		IsPlayed: ptr(false), IsResumable: ptr(true), Count: true,
		SortBy: []pg.SortKey{{By: "DatePlayed", Descending: true}},
	}
	pageParams(q, &iq)
	res, err := pg.QueryItems(r.Context(), a.DB, iq)
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	a.writeItemPage(w, r, user, res.Items, dtoOptionsFrom(q), res.Total, iq.StartIndex)
}

// getLatestMedia serves /Items/Latest (TASKS P1.21): the newest items per
// library as a bare array. Observed on 12.1.0: with no includeItemTypes a
// movies library contributes Movies and a TV library Series; with
// groupItems=true and Episode among the types, each episode is replaced by
// its series (which then carries ChildCount), one per series, at the
// episode's position in date order.
func (a *api) getLatestMedia(w http.ResponseWriter, r *http.Request, s auth.Session) {
	user, ok := queryUser(r, s)
	if !ok {
		errorText(w, http.StatusForbidden)
		return
	}
	q := jfapi.QueryOf(r)
	limit := 20
	if n, ok := q.Int("limit"); ok && n > 0 {
		limit = n
	}
	fs, err := a.folders(r, nil)
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	if p := q.Get("parentId"); p != "" {
		id, err := dto.ParseID(p)
		if err != nil {
			jfapi.WriteJSON(w, r, http.StatusOK, []dto.BaseItemDto{})
			return
		}
		if id != rootFolderID(a.ServerID) {
			kept := fs[:0]
			for _, f := range fs {
				if f.row.ID == id.UUID() {
					kept = append(kept, f)
				}
			}
			fs = kept
		}
	}
	include := q.List("includeItemTypes")
	group, _ := q.Bool("groupItems")
	var out []db.Item
	fromEpisode := map[int]bool{} // out positions holding a series that stands in for its latest episode
	for _, f := range fs {
		if limit > 0 && len(out) >= limit {
			break
		}
		types := include
		if len(types) == 0 {
			types = []string{"Movie", "Series"}
			switch f.row.Kind {
			case "movies":
				types = []string{"Movie"}
			case "tvshows":
				types = []string{"Series"}
			}
		}
		room := limit
		if limit > 0 {
			room = limit - len(out)
		}
		res, err := pg.QueryItems(r.Context(), a.DB, pg.ItemQuery{
			UserID: user, ParentID: &f.row.ID, Recursive: true, IncludeTypes: types,
			SortBy: []pg.SortKey{{By: "DateCreated", Descending: true}}, Limit: room,
		})
		if err != nil {
			a.internalError(w, r, err)
			return
		}
		grouped, groupedAt, err := a.groupLatest(r.Context(), res.Items, group, include)
		if err != nil {
			a.internalError(w, r, err)
			return
		}
		for _, pos := range groupedAt {
			fromEpisode[len(out)+pos] = true
		}
		out = append(out, grouped...)
	}
	dtos, err := a.itemDtos(r.Context(), &user, out, dtoOptionsFrom(q))
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	if err := a.setGroupedChildCounts(r.Context(), user, out, fromEpisode, dtos); err != nil {
		a.internalError(w, r, err)
		return
	}
	jfapi.WriteJSON(w, r, http.StatusOK, dtos)
}

// groupLatest walks one library's latest items, replacing episodes with
// their series (first per series) when groupItems asked for it. groupedAt
// lists the positions in the returned slice that hold such a series entry.
func (a *api) groupLatest(ctx context.Context, items []db.Item, group bool, include []string) ([]db.Item, []int, error) {
	if !group || !slices.ContainsFunc(include, func(t string) bool { return strings.EqualFold(t, "Episode") }) {
		return items, nil, nil
	}
	byID, err := a.loadParents(ctx, items)
	if err != nil {
		return nil, nil, err
	}
	seriesOf := func(it db.Item) (db.Item, bool) {
		if it.ParentID == nil {
			return db.Item{}, false
		}
		p, ok := byID[*it.ParentID]
		if !ok {
			return db.Item{}, false
		}
		if p.Type == "Series" {
			return p, true
		}
		if p.Type == "Season" && p.ParentID != nil {
			s, ok := byID[*p.ParentID]
			return s, ok && s.Type == "Series"
		}
		return db.Item{}, false
	}
	out := make([]db.Item, 0, len(items))
	var groupedAt []int
	seenSeries := map[uuid.UUID]bool{}
	for _, it := range items {
		if it.Type != "Episode" {
			out = append(out, it)
			continue
		}
		series, ok := seriesOf(it)
		if !ok || seenSeries[series.ID] { // one series per library
			continue
		}
		seenSeries[series.ID] = true
		groupedAt = append(groupedAt, len(out))
		out = append(out, series)
	}
	return out, groupedAt, nil
}

// loadParents loads the direct parents (and their parents) of items in one
// batch, keyed by each loaded row's own id.
func (a *api) loadParents(ctx context.Context, items []db.Item) (map[uuid.UUID]db.Item, error) {
	byID := map[uuid.UUID]db.Item{}
	var want []uuid.UUID
	for _, it := range items {
		if it.ParentID != nil {
			want = append(want, *it.ParentID)
		}
	}
	if len(want) == 0 {
		return byID, nil
	}
	rows, err := a.Queries.GetItemsByIDs(ctx, want)
	if err != nil {
		return nil, err
	}
	var grand []uuid.UUID
	for _, p := range rows {
		byID[p.ID] = p
		if p.ParentID != nil {
			grand = append(grand, *p.ParentID)
		}
	}
	if len(grand) > 0 {
		more, err := a.Queries.GetItemsByIDs(ctx, grand)
		if err != nil {
			return nil, err
		}
		for _, g := range more {
			byID[g.ID] = g
		}
	}
	return byID, nil
}

// setGroupedChildCounts gives series entries that stand in for their latest
// episode a ChildCount of the series' episode total (Jellyfin does the same
// when grouping /Items/Latest).
func (a *api) setGroupedChildCounts(ctx context.Context, user uuid.UUID, items []db.Item, fromEpisode map[int]bool, dtos []dto.BaseItemDto) error {
	var ids []uuid.UUID
	for pos := range fromEpisode {
		ids = append(ids, items[pos].ID)
	}
	if len(ids) == 0 {
		return nil
	}
	counts, err := a.Queries.UnplayedEpisodeCounts(ctx, db.UnplayedEpisodeCountsParams{Ids: ids, UserID: user})
	if err != nil {
		return err
	}
	total := map[uuid.UUID]int64{}
	for _, c := range counts {
		if c.ItemID != nil {
			total[*c.ItemID] = c.Total
		}
	}
	for pos := range fromEpisode {
		dtos[pos].ChildCount = ptr(int32(total[items[pos].ID]))
	}
	return nil
}

// writeItemPage renders one page of items as Jellyfin's QueryResult.
func (a *api) writeItemPage(w http.ResponseWriter, r *http.Request, user uuid.UUID, items []db.Item, o dtoOptions, total, startIndex int) {
	dtos, err := a.itemDtos(r.Context(), &user, items, o)
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	jfapi.WriteJSON(w, r, http.StatusOK, dto.BaseItemDtoQueryResult{
		Items: &dtos, TotalRecordCount: ptr(int32(total)), StartIndex: ptr(int32(startIndex)),
	})
}
