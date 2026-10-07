package handlers

import (
	"encoding/json"
	"math"
	"net/http"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/sysadmin/blockbustr/internal/auth"
	"github.com/sysadmin/blockbustr/internal/jfapi"
	"github.com/sysadmin/blockbustr/internal/jfapi/dto"
	"github.com/sysadmin/blockbustr/internal/store/pg"
	"github.com/sysadmin/blockbustr/internal/store/pg/db"
)

// TV endpoints (TASKS P1.21): /Shows/NextUp, /Shows/{id}/Seasons and
// /Shows/{id}/Episodes. Behaviour matched against the 12.1.0 captures.

func (a *api) registerShows(rt *jfapi.Router) {
	rt.Get("/Shows/NextUp", a.requireUser(a.getNextUp))
	rt.Get("/Shows/{seriesId}/Seasons", a.requireUser(a.getSeasons))
	rt.Get("/Shows/{seriesId}/Episodes", a.requireUser(a.getEpisodes))
}

// visibleSeries resolves and authorises the {seriesId} of a /Shows route.
func (a *api) visibleSeries(w http.ResponseWriter, r *http.Request, user uuid.UUID) (uuid.UUID, bool) {
	id, err := dto.ParseID(jfapi.URLParam(r, "seriesId"))
	if err != nil {
		errorText(w, http.StatusNotFound)
		return uuid.Nil, false
	}
	it, ok, err := a.visibleItem(r, user, id.UUID())
	if err != nil {
		a.internalError(w, r, err)
		return uuid.Nil, false
	} else if !ok {
		errorText(w, http.StatusNotFound)
		return uuid.Nil, false
	}
	a.ensureEpisodes(r, it)
	return id.UUID(), true
}

// getNextUp serves /Shows/NextUp: the next episode of every series the user
// has started. enableRewatching is accepted but has no effect yet (rewatching
// next-up lists wait for P2.9 playback state).
func (a *api) getNextUp(w http.ResponseWriter, r *http.Request, s auth.Session) {
	user, ok := queryUser(r, s)
	if !ok {
		errorText(w, http.StatusForbidden)
		return
	}
	q := jfapi.QueryOf(r)
	p := db.NextUpEpisodeIDsParams{UserID: user, Lim: math.MaxInt32}
	p.IncludeResumable, _ = q.Bool("enableResumable")
	if d, err := time.Parse("2006-01-02", q.Get("nextUpDateCutoff")); err == nil {
		p.Cutoff = &d
	}
	if v := q.Get("seriesId"); v != "" {
		if id, err := dto.ParseID(v); err == nil {
			sid := id.UUID()
			p.SeriesID = &sid
		}
	}
	if n, ok := q.Int("limit"); ok && n > 0 {
		p.Lim = int32(n)
	}
	if n, ok := q.Int("startIndex"); ok && n > 0 {
		p.StartIdx = int32(n)
	}
	// Next Up is a whole-library query (about 60 ms at 19.5k episodes,
	// TASKS P4.5); its ids are kept until the next library or user-data event.
	key, _ := json.Marshal(p) // pointers are encoded by value
	rows, err := cachedQuery(r.Context(), a, "nextup", string(key), func() ([]db.NextUpEpisodeIDsRow, error) {
		return a.Queries.NextUpEpisodeIDs(r.Context(), p)
	})
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	ids := make([]uuid.UUID, len(rows))
	for i, row := range rows {
		ids[i] = row.ID
	}
	items, err := a.itemsInIDOrder(r.Context(), ids)
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	total := len(rows)
	if count, ok := q.Bool("enableTotalRecordCount"); !ok || count {
		if len(rows) > 0 { // the window count covers the whole result
			total = int(rows[0].Total)
		}
	}
	a.writeItemPage(w, r, user, items, dtoOptionsFrom(q), total, int(p.StartIdx))
}

// getSeasons serves /Shows/{id}/Seasons: a series' seasons, by number.
func (a *api) getSeasons(w http.ResponseWriter, r *http.Request, s auth.Session) {
	user, ok := queryUser(r, s)
	if !ok {
		errorText(w, http.StatusForbidden)
		return
	}
	series, ok := a.visibleSeries(w, r, user)
	if !ok {
		return
	}
	iq := pg.ItemQuery{
		UserID: user, ParentID: &series, IncludeTypes: []string{"Season"},
		SortBy: []pg.SortKey{{By: "IndexNumber"}}, Count: true,
	}
	pageParams(jfapi.QueryOf(r), &iq)
	res, err := a.queryItems(r.Context(), iq)
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	a.writeItemPage(w, r, user, res.Items, dtoOptionsFrom(jfapi.QueryOf(r)), res.Total, iq.StartIndex)
}

// getEpisodes serves /Shows/{id}/Episodes: a series' episodes in aired
// order, optionally narrowed to one season. With adjacentTo it returns the
// episode before, the named one and the one after (as many as exist) —
// what clients ask for around autoplay — capped by limit.
func (a *api) getEpisodes(w http.ResponseWriter, r *http.Request, s auth.Session) {
	user, ok := queryUser(r, s)
	if !ok {
		errorText(w, http.StatusForbidden)
		return
	}
	series, ok := a.visibleSeries(w, r, user)
	if !ok {
		return
	}
	q := jfapi.QueryOf(r)
	iq := pg.ItemQuery{
		UserID: user, IncludeTypes: []string{"Episode"},
		SortBy: []pg.SortKey{{By: "AiredEpisodeOrder"}}, Count: true,
	}
	if sid := q.Get("seasonId"); sid != "" {
		season, err := dto.ParseID(sid)
		if err != nil {
			a.writeItemPage(w, r, user, nil, dtoOptionsFrom(q), 0, 0)
			return
		}
		iq.ParentID = ptr(season.UUID())
	} else {
		iq.ParentID, iq.Recursive = &series, true
	}
	pageParams(q, &iq)
	if adjacent := q.Get("adjacentTo"); adjacent != "" {
		if aid, err := dto.ParseID(adjacent); err == nil {
			a.writeAdjacentEpisodes(w, r, user, iq, aid.UUID(), dtoOptionsFrom(q))
			return
		}
	}
	res, err := a.queryItems(r.Context(), iq)
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	a.writeItemPage(w, r, user, res.Items, dtoOptionsFrom(q), res.Total, iq.StartIndex)
}

// writeAdjacentEpisodes is the adjacentTo branch of getEpisodes.
func (a *api) writeAdjacentEpisodes(w http.ResponseWriter, r *http.Request, user uuid.UUID, iq pg.ItemQuery, adjacent uuid.UUID, o dtoOptions) {
	limit := iq.Limit
	iq.Limit, iq.StartIndex, iq.Count = 0, 0, false
	res, err := a.queryItems(r.Context(), iq)
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	var window []db.Item
	if idx := slices.IndexFunc(res.Items, func(it db.Item) bool { return it.ID == adjacent }); idx >= 0 {
		lo, hi := max(idx-1, 0), min(idx+2, len(res.Items))
		window = res.Items[lo:hi]
		if limit > 0 && len(window) > limit {
			window = window[:limit]
		}
	}
	a.writeItemPage(w, r, user, window, o, len(window), 0)
}
