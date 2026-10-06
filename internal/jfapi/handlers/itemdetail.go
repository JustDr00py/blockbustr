package handlers

import (
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/sysadmin/blockbustr/internal/auth"
	"github.com/sysadmin/blockbustr/internal/jfapi"
	"github.com/sysadmin/blockbustr/internal/jfapi/dto"
	"github.com/sysadmin/blockbustr/internal/store/pg"
	"github.com/sysadmin/blockbustr/internal/store/pg/db"
)

// Item details (TASKS P1.20): /Items/{id} answers with Jellyfin's full field
// set whatever `fields` says, including MediaSources/MediaStreams.

// detailFields is every ItemField item details include.
var detailFields = []string{
	"candelete", "candownload", "chapters", "childcount", "recursiveitemcount", "cumulativeruntimeticks",
	"datecreated", "datelastmediaadded", "displaypreferencesid", "etag", "externalurls", "genres", "studios",
	"seriesstudio", "originaltitle", "overview", "parentid", "path", "people", "playaccess", "productionlocations",
	"providerids", "primaryimageaspectratio", "sortname", "taglines", "tags", "remotetrailers", "settings",
	"enablemediasourcedisplay", "localtrailercount", "specialfeaturecount", "width", "height", "ishd",
	"trickplay", "mediasources", "mediastreams",
}

func detailOptions() dtoOptions {
	o := dtoOptions{fields: map[string]bool{}, enableImages: true, imageTypeLimit: -1, enableUserData: true}
	for _, f := range detailFields {
		o.fields[f] = true
	}
	return o
}

func (a *api) registerItemDetail(rt *jfapi.Router) {
	rt.Get("/Items/{itemId}", a.requireUser(a.getItem))
	rt.Get("/Users/{userId}/Items/{itemId}", a.requireUser(a.getItem)) // legacy route
	rt.Get("/Items/{itemId}/Ancestors", a.requireUser(a.getAncestors))
	rt.Get("/Items/{itemId}/Similar", a.requireUser(a.getSimilar))
}

// visibleItem loads an item the user may see: in an enabled library and not
// missing. ok is false otherwise.
func (a *api) visibleItem(r *http.Request, user, id uuid.UUID) (db.Item, bool, error) {
	res, err := pg.QueryItems(r.Context(), a.DB, pg.ItemQuery{UserID: user, IDs: []uuid.UUID{id}})
	if err != nil || len(res.Items) == 0 {
		return db.Item{}, false, err
	}
	return res.Items[0], true, nil
}

// itemDetail is the full dto of any item, library folder or the root folder.
func (a *api) itemDetail(r *http.Request, user uuid.UUID, it db.Item) (dto.BaseItemDto, error) {
	if it.Type == "CollectionFolder" {
		fs, err := a.folders(r, &user)
		if err != nil {
			return dto.BaseItemDto{}, err
		}
		for _, f := range fs {
			if f.row.ID == it.ID {
				return a.collectionFolderDto(f, true), nil
			}
		}
	}
	ds, err := a.itemDtos(r.Context(), &user, []db.Item{it}, detailOptions())
	if err != nil {
		return dto.BaseItemDto{}, err
	}
	return ds[0], nil
}

func (a *api) getItem(w http.ResponseWriter, r *http.Request, s auth.Session) {
	user, ok := queryUser(r, s)
	if !ok {
		errorText(w, http.StatusForbidden)
		return
	}
	id, err := dto.ParseID(jfapi.URLParam(r, "itemId"))
	if err != nil {
		errorText(w, http.StatusNotFound)
		return
	}
	if id == rootFolderID(a.ServerID) {
		jfapi.WriteJSON(w, r, http.StatusOK, a.rootFolderDto(user))
		return
	}
	it, ok, err := a.visibleItem(r, user, id.UUID())
	switch {
	case err != nil:
		a.internalError(w, r, err)
		return
	case !ok:
		errorText(w, http.StatusNotFound)
		return
	}
	d, err := a.itemDetail(r, user, it)
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	jfapi.WriteJSON(w, r, http.StatusOK, d)
}

// getAncestors lists an item's parents, nearest first, ending with the root
// folder ("Media Folders").
func (a *api) getAncestors(w http.ResponseWriter, r *http.Request, s auth.Session) {
	id, err := dto.ParseID(jfapi.URLParam(r, "itemId"))
	if err != nil {
		errorText(w, http.StatusNotFound)
		return
	}
	it, ok, err := a.visibleItem(r, s.UserID, id.UUID())
	switch {
	case err != nil:
		a.internalError(w, r, err)
		return
	case !ok:
		errorText(w, http.StatusNotFound)
		return
	}
	out := []dto.BaseItemDto{}
	for depth := 0; it.ParentID != nil && depth < 8; depth++ {
		p, err := a.Queries.GetItem(r.Context(), *it.ParentID)
		if err != nil {
			a.internalError(w, r, err)
			return
		}
		d, err := a.itemDetail(r, s.UserID, p)
		if err != nil {
			a.internalError(w, r, err)
			return
		}
		out = append(out, d)
		it = p
	}
	out = append(out, a.rootFolderDto(s.UserID))
	jfapi.WriteJSON(w, r, http.StatusOK, out)
}

// rootFolderDto is the user's root folder, the parent of every library.
func (a *api) rootFolderDto(user uuid.UUID) dto.BaseItemDto {
	id := rootFolderID(a.ServerID)
	ud := userDataDto(id.UUID(), nil)
	return dto.BaseItemDto{
		Name: ptr("Media Folders"), ServerId: ptr(a.ServerID.String()), Id: &id,
		// A constant date keeps the Etag stable across restarts.
		Etag: ptr(itemEtag(id.UUID(), time.Time{}, nil)), DateCreated: &dto.Time{},
		CanDelete: ptr(false), CanDownload: ptr(false), SortName: ptr("media folders"), ExternalUrls: &[]dto.ExternalUrl{},
		Path: ptr("/config/root/default"), EnableMediaSourceDisplay: ptr(true), Taglines: &[]string{}, Genres: &[]string{},
		PlayAccess: ptr(dto.PlayAccessFull), RemoteTrailers: &[]dto.MediaUrl{}, ProviderIds: &map[string]*string{},
		IsFolder: ptr(true), Type: ptr(dto.BaseItemKind("UserRootFolder")), People: &[]dto.BaseItemPerson{},
		Studios: &[]dto.NameGuidPair{}, GenreItems: &[]dto.NameGuidPair{}, LocalTrailerCount: ptr(int32(0)),
		UserData: &ud, ChildCount: ptr(int32(0)), SpecialFeatureCount: ptr(int32(0)), DisplayPreferencesId: ptr(id.String()),
		Tags: &[]string{}, ImageTags: &map[string]*string{}, BackdropImageTags: &[]string{},
		ImageBlurHashes: &map[string]map[string]*string{}, Chapters: &[]dto.ChapterInfo{},
		LocationType: ptr(dto.FileSystem), MediaType: ptr(dto.MediaTypeUnknown), LockedFields: &[]dto.MetadataField{}, LockData: ptr(false),
	}
}

// getSimilar serves /Items/{id}/Similar (TASKS P1.21): same-kind items that
// share genres, studios or people with the item, most-shared first. Movies
// and series only; anything else (episodes, seasons) has no similar items.
func (a *api) getSimilar(w http.ResponseWriter, r *http.Request, s auth.Session) {
	user, ok := queryUser(r, s)
	if !ok {
		errorText(w, http.StatusForbidden)
		return
	}
	id, err := dto.ParseID(jfapi.URLParam(r, "itemId"))
	if err != nil {
		errorText(w, http.StatusNotFound)
		return
	}
	it, ok, err := a.visibleItem(r, user, id.UUID())
	switch {
	case err != nil:
		a.internalError(w, r, err)
		return
	case !ok:
		errorText(w, http.StatusNotFound)
		return
	}
	if it.Type != "Movie" && it.Type != "Series" {
		jfapi.WriteJSON(w, r, http.StatusOK, queryResult([]dto.BaseItemDto{}))
		return
	}
	q := jfapi.QueryOf(r)
	// Jellyfin always sends ProviderIds on similar items, whatever `fields` says.
	o := dtoOptionsFrom(q)
	o.fields["providerids"] = true
	limit := int32(12)
	if n, ok := q.Int("limit"); ok && n > 0 {
		limit = int32(n)
	}
	ids, err := a.Queries.SimilarItemIDs(r.Context(), db.SimilarItemIDsParams{Typ: it.Type, ItemID: it.ID, Lim: limit})
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	items, err := a.itemsInIDOrder(r.Context(), ids)
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	a.writeItemPage(w, r, user, items, o, len(items), 0)
}
