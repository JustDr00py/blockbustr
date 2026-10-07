package handlers

import (
	"net/http"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/sysadmin/blockbustr/internal/auth"
	"github.com/sysadmin/blockbustr/internal/images"
	"github.com/sysadmin/blockbustr/internal/jfapi"
	"github.com/sysadmin/blockbustr/internal/jfapi/dto"
	"github.com/sysadmin/blockbustr/internal/media"
	"github.com/sysadmin/blockbustr/internal/store/pg"
)

// Browse aux endpoints (TASKS P1.22, DESIGN §3.9): /Genres, /Studios,
// /Persons, /Search/Hints and /Items/Filters[2]. Names are only listed when
// a visible item uses them, scoped by the same item filters as /Items.

func (a *api) registerNames(rt *jfapi.Router) {
	rt.Get("/Genres", a.requireUser(a.getNames(pg.Genres)))
	rt.Get("/Studios", a.requireUser(a.getNames(pg.Studios)))
	rt.Get("/Persons", a.requireUser(a.getNames(pg.People)))
	// No music yet: Jellyfin Android's search always asks for artists too.
	rt.Get("/Artists", a.requireUser(func(w http.ResponseWriter, r *http.Request, _ auth.Session) {
		jfapi.WriteJSON(w, r, http.StatusOK, queryResult([]dto.BaseItemDto{}))
	}))
	rt.Get("/Search/Hints", a.requireUser(a.getSearchHints))
	rt.Get("/Items/Filters", a.requireUser(a.getFiltersLegacy))
	rt.Get("/Items/Filters2", a.requireUser(a.getFilters))
}

var nameKinds = map[pg.NameKind]dto.BaseItemKind{
	pg.Genres: dto.BaseItemKindGenre, pg.Studios: dto.BaseItemKindStudio, pg.People: dto.BaseItemKindPerson,
}

// scopeQuery is the item scope of a by-name or filter request: recursive
// under parentId (the user root = everything), narrowed by item types. ok
// is false when nothing can match (an unparsable parentId).
func (a *api) scopeQuery(q jfapi.Query, user uuid.UUID) (pg.ItemQuery, bool) {
	iq := pg.ItemQuery{
		UserID: user, Recursive: true,
		IncludeTypes: q.List("includeItemTypes"), ExcludeTypes: q.List("excludeItemTypes"), MediaTypes: q.List("mediaTypes"),
	}
	if p := q.Get("parentId"); p != "" {
		id, err := dto.ParseID(p)
		if err != nil {
			return iq, false
		}
		if id != rootFolderID(a.ServerID) {
			iq.ParentID = ptr(id.UUID())
		}
	}
	return iq, true
}

func (a *api) getNames(kind pg.NameKind) sessionHandler {
	return func(w http.ResponseWriter, r *http.Request, s auth.Session) {
		user, ok := queryUser(r, s)
		if !ok {
			errorText(w, http.StatusForbidden)
			return
		}
		q := jfapi.QueryOf(r)
		iq, ok := a.scopeQuery(q, user)
		// Genres, studios and people can't be favourites (yet).
		if fav, _ := q.Bool("isFavorite"); !ok || fav {
			jfapi.WriteJSON(w, r, http.StatusOK, queryResult([]dto.BaseItemDto{}))
			return
		}
		nq := pg.NameQuery{
			Kind: kind, Items: iq, SearchTerm: q.Get("searchTerm"), NameStartsWith: q.Get("nameStartsWith"),
			NameStartsWithOrGreater: q.Get("nameStartsWithOrGreater"), NameLessThan: q.Get("nameLessThan"),
			PersonTypes: q.List("personTypes"), ExcludePersonTypes: q.List("excludePersonTypes"),
			Descending: strings.EqualFold(q.Get("sortOrder"), "Descending"),
		}
		if ids := idList(q, "appearsInItemId"); len(ids) > 0 {
			nq.AppearsInItemID = &ids[0]
		}
		if n, ok := q.Int("startIndex"); ok && n > 0 {
			nq.StartIndex = n
		}
		if n, ok := q.Int("limit"); ok && n > 0 {
			nq.Limit = n
		}
		res, err := pg.QueryNames(r.Context(), a.DB, nq)
		if err != nil {
			a.internalError(w, r, err)
			return
		}
		o := dtoOptionsFrom(q)
		dtos := make([]dto.BaseItemDto, len(res.Rows))
		for i, row := range res.Rows {
			dtos[i] = a.nameDto(nameKinds[kind], row, o)
		}
		// Observed on 12.1.0: without a count the total is 0, not the page
		// length as for /Items.
		total := res.Total
		if b, ok := q.Bool("enableTotalRecordCount"); ok && !b {
			total = 0
		}
		jfapi.WriteJSON(w, r, http.StatusOK, dto.BaseItemDtoQueryResult{
			Items: &dtos, TotalRecordCount: ptr(int32(total)), StartIndex: ptr(int32(nq.StartIndex)),
		})
	}
}

// nameDto is a Genre, Studio or Person as the by-name lists return it, with
// Jellyfin's per-kind item counts (no music, trailers or programs here).
func (a *api) nameDto(kind dto.BaseItemKind, row pg.NameRow, o dtoOptions) dto.BaseItemDto {
	id := dto.IDFromUUID(row.ID)
	d := dto.BaseItemDto{
		Name: ptr(row.Name), ServerId: ptr(a.ServerID.String()), Id: &id, Type: ptr(kind),
		LocationType: ptr(dto.FileSystem), MediaType: ptr(dto.MediaTypeUnknown),
		ImageTags: &map[string]*string{}, ImageBlurHashes: &map[string]map[string]*string{},
		ChildCount: ptr(int32(row.Items)), MovieCount: ptr(int32(row.Movies)), SeriesCount: ptr(int32(row.Series)),
		EpisodeCount: ptr(int32(row.Episodes)), TrailerCount: ptr(int32(0)), ProgramCount: ptr(int32(0)),
		SongCount: ptr(int32(0)), AlbumCount: ptr(int32(0)), ArtistCount: ptr(int32(0)), MusicVideoCount: ptr(int32(0)),
	}
	if o.enableUserData {
		ud := userDataDto(row.ID, nil)
		ud.Key = string(kind) + "-" + row.Name // Jellyfin keys by-name items by kind and name
		d.UserData = &ud
	}
	if o.has("datecreated") {
		d.DateCreated = ptr(dto.NewTime(row.DateCreated))
	}
	if o.has("candelete") {
		d.CanDelete = ptr(false)
	}
	if o.has("sortname") {
		d.SortName = ptr(strings.ToLower(row.Name))
	}
	// Only people have artwork (studio logos would need a provider).
	if row.ImageURL != nil && o.enableImages && o.imageTypeLimit != 0 && (o.imageTypes == nil || o.imageTypes["primary"]) {
		(*d.ImageTags)["Primary"] = ptr(images.Tag(*row.ImageURL))
		if o.has("primaryimageaspectratio") {
			d.PrimaryImageAspectRatio = ptr(2.0 / 3)
		}
	}
	return d
}

// getSearchHints serves /Search/Hints: library items first (ranked like
// /Items search, plus remote matches, DESIGN §7.4), then genres, studios
// and people whose names match. No captured client uses it yet (§3.5).
func (a *api) getSearchHints(w http.ResponseWriter, r *http.Request, s auth.Session) {
	user, ok := queryUser(r, s)
	if !ok {
		errorText(w, http.StatusForbidden)
		return
	}
	q := jfapi.QueryOf(r)
	term := strings.TrimSpace(q.Get("searchTerm"))
	iq, ok := a.scopeQuery(q, user)
	if term == "" || !ok {
		jfapi.WriteJSON(w, r, http.StatusOK, dto.SearchHintResult{SearchHints: &[]dto.SearchHint{}, TotalRecordCount: ptr(int32(0))})
		return
	}
	start, limit := 0, 0
	if n, ok := q.Int("startIndex"); ok && n > 0 {
		start = n
	}
	if n, ok := q.Int("limit"); ok && n > 0 {
		limit = n
	}
	want := 0 // rows needed from each source to fill the page
	if limit > 0 {
		want = start + limit
	}
	isName := func(t string) bool {
		return slices.ContainsFunc([]string{"Genre", "Studio", "Person"}, func(k string) bool { return strings.EqualFold(k, t) })
	}
	// includeItemTypes/excludeItemTypes also pick which name kinds to list;
	// the item scope keeps only the real item types.
	scope := iq
	scope.IncludeTypes = slices.DeleteFunc(slices.Clone(iq.IncludeTypes), isName)
	listed := func(kind dto.BaseItemKind, flag string) bool {
		if on, ok := q.Bool(flag); ok && !on {
			return false
		}
		has := func(list []string) bool {
			return slices.ContainsFunc(list, func(t string) bool { return strings.EqualFold(t, string(kind)) })
		}
		return (len(iq.IncludeTypes) == 0 || has(iq.IncludeTypes)) && !has(iq.ExcludeTypes)
	}

	var hints []dto.SearchHint
	total := 0
	if on, ok := q.Bool("includeMedia"); (!ok || on) && (len(iq.IncludeTypes) == 0 || len(scope.IncludeTypes) > 0) {
		mq := scope
		mq.SearchTerm, mq.Limit, mq.Count = term, want, true
		res, err := a.queryItems(r.Context(), mq)
		if err != nil {
			a.internalError(w, r, err)
			return
		}
		items := a.withRemoteMatches(r, mq, res.Items)
		total += res.Total + len(items) - len(res.Items)
		dtos, err := a.itemDtos(r.Context(), &user, items, dtoOptions{fields: map[string]bool{"primaryimageaspectratio": true}, enableImages: true, imageTypeLimit: 1})
		if err != nil {
			a.internalError(w, r, err)
			return
		}
		for _, d := range dtos {
			hints = append(hints, searchHint(d))
		}
	}
	for _, k := range []struct {
		kind pg.NameKind
		flag string
	}{{pg.Genres, "includeGenres"}, {pg.Studios, "includeStudios"}, {pg.People, "includePeople"}} {
		if !listed(nameKinds[k.kind], k.flag) {
			continue
		}
		res, err := pg.QueryNames(r.Context(), a.DB, pg.NameQuery{Kind: k.kind, Items: scope, SearchTerm: term, Limit: want})
		if err != nil {
			a.internalError(w, r, err)
			return
		}
		total += res.Total
		for _, row := range res.Rows {
			id := dto.IDFromUUID(row.ID)
			h := dto.SearchHint{
				ItemId: &id, Id: &id, Name: ptr(row.Name), Type: ptr(nameKinds[k.kind]), IsFolder: ptr(true),
				MediaType: ptr(dto.MediaTypeUnknown), Artists: &[]string{},
			}
			if row.ImageURL != nil {
				h.PrimaryImageTag, h.PrimaryImageAspectRatio = ptr(images.Tag(*row.ImageURL)), ptr(2.0/3)
			}
			hints = append(hints, h)
		}
	}
	page := []dto.SearchHint{}
	if start < len(hints) {
		page = hints[start:]
		if limit > 0 && len(page) > limit {
			page = page[:limit]
		}
	}
	jfapi.WriteJSON(w, r, http.StatusOK, dto.SearchHintResult{SearchHints: &page, TotalRecordCount: ptr(int32(total))})
}

// searchHint condenses an item's DTO into a SearchHint; thumb and backdrop
// fall back to the series' artwork for episodes and seasons.
func searchHint(d dto.BaseItemDto) dto.SearchHint {
	h := dto.SearchHint{
		ItemId: d.Id, Id: d.Id, Name: d.Name, Type: d.Type, IsFolder: d.IsFolder, MediaType: d.MediaType,
		IndexNumber: d.IndexNumber, ParentIndexNumber: d.ParentIndexNumber, ProductionYear: d.ProductionYear,
		RunTimeTicks: d.RunTimeTicks, Series: d.SeriesName, Status: d.Status, StartDate: d.PremiereDate, EndDate: d.EndDate,
		PrimaryImageAspectRatio: d.PrimaryImageAspectRatio, Artists: &[]string{},
	}
	own := d.Id.String()
	if d.ImageTags != nil {
		h.PrimaryImageTag = (*d.ImageTags)["Primary"]
		if t := (*d.ImageTags)["Thumb"]; t != nil {
			h.ThumbImageTag, h.ThumbImageItemId = t, &own
		}
	}
	if h.ThumbImageTag == nil && d.ParentThumbImageTag != nil {
		h.ThumbImageTag, h.ThumbImageItemId = d.ParentThumbImageTag, ptr(d.ParentThumbItemId.String())
	}
	switch {
	case d.BackdropImageTags != nil && len(*d.BackdropImageTags) > 0:
		h.BackdropImageTag, h.BackdropImageItemId = &(*d.BackdropImageTags)[0], &own
	case d.ParentBackdropImageTags != nil && len(*d.ParentBackdropImageTags) > 0:
		h.BackdropImageTag, h.BackdropImageItemId = &(*d.ParentBackdropImageTags)[0], ptr(d.ParentBackdropItemId.String())
	}
	return h
}

// itemFilters answers the shared part of /Items/Filters[2]: the filter
// values of the scoped items (recursive unless Filters2 says otherwise).
// ok is false when a response was already written.
func (a *api) itemFilters(w http.ResponseWriter, r *http.Request, s auth.Session) (pg.ItemFilterValues, bool) {
	user, ok := queryUser(r, s)
	if !ok {
		errorText(w, http.StatusForbidden)
		return pg.ItemFilterValues{}, false
	}
	q := jfapi.QueryOf(r)
	iq, ok := a.scopeQuery(q, user)
	if !ok {
		return pg.ItemFilterValues{}, true
	}
	if rec, ok := q.Bool("recursive"); ok && !rec && iq.ParentID != nil {
		iq.Recursive = false
	}
	f, err := pg.ItemFilters(r.Context(), a.DB, iq)
	if err != nil {
		a.internalError(w, r, err)
		return pg.ItemFilterValues{}, false
	}
	return f, true
}

func (a *api) getFiltersLegacy(w http.ResponseWriter, r *http.Request, s auth.Session) {
	f, ok := a.itemFilters(w, r, s)
	if !ok {
		return
	}
	genres := make([]string, len(f.Genres))
	for i, g := range f.Genres {
		genres[i] = g.Name
	}
	jfapi.WriteJSON(w, r, http.StatusOK, dto.QueryFiltersLegacy{
		Genres: &genres, Tags: &[]string{}, OfficialRatings: nonNil(f.OfficialRatings), Years: nonNil(f.Years),
	})
}

func (a *api) getFilters(w http.ResponseWriter, r *http.Request, s auth.Session) {
	f, ok := a.itemFilters(w, r, s)
	if !ok {
		return
	}
	genres := make([]dto.NameGuidPair, len(f.Genres))
	for i, g := range f.Genres {
		genres[i] = dto.NameGuidPair{Name: ptr(g.Name), Id: ptr(dto.IDFromUUID(g.ID))}
	}
	jfapi.WriteJSON(w, r, http.StatusOK, dto.QueryFilters{
		Genres: &genres, Tags: &[]string{},
		AudioLanguages: languagePairs(f.AudioLanguages), SubtitleLanguages: languagePairs(f.SubtitleLanguages),
	})
}

// languagePairs labels stream languages like Jellyfin, "English (eng)",
// sorted by label.
func languagePairs(codes []string) *[]dto.NameValuePair {
	out := make([]dto.NameValuePair, 0, len(codes))
	for _, c := range codes {
		out = append(out, dto.NameValuePair{Name: ptr(media.LanguageName(c) + " (" + c + ")"), Value: ptr(c)})
	}
	slices.SortStableFunc(out, func(a, b dto.NameValuePair) int { return strings.Compare(*a.Name, *b.Name) })
	return &out
}

func nonNil[T any](s []T) *[]T {
	if s == nil {
		s = []T{}
	}
	return &s
}
