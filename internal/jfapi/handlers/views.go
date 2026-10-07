package handlers

import (
	_ "embed"
	"encoding/json"
	"net/http"
	"slices"

	"github.com/google/uuid"

	"github.com/sysadmin/blockbustr/internal/auth"
	"github.com/sysadmin/blockbustr/internal/jfapi"
	"github.com/sysadmin/blockbustr/internal/jfapi/dto"
	"github.com/sysadmin/blockbustr/internal/store/pg"
	"github.com/sysadmin/blockbustr/internal/store/pg/db"
)

// Jellyfin 12.1.0's default LibraryOptions per collection type, copied from
// /Library/VirtualFolders; PathInfos is filled from the library's paths.
var (
	//go:embed defaults/library_options_movies.json
	libraryOptionsMovies []byte
	//go:embed defaults/library_options_tvshows.json
	libraryOptionsTVShows []byte
)

func (a *api) registerViews(rt *jfapi.Router) {
	rt.Get("/UserViews", a.requireUser(a.userViews))
	rt.Get("/Users/{userId}/Views", a.requireUser(a.userViews)) // legacy route
	rt.Get("/UserViews/GroupingOptions", a.requireUser(a.groupingOptions))
	rt.Get("/Library/MediaFolders", a.requireAdmin(a.mediaFolders))
	rt.Get("/Library/VirtualFolders", a.requireAdmin(a.virtualFolders))
}

// folders loads every enabled library's folder, with child counts and (for
// a user) user data.
func (a *api) folders(r *http.Request, user *uuid.UUID) ([]folderView, error) {
	ctx := r.Context()
	rows, err := a.Queries.ListCollectionFolders(ctx)
	if err != nil {
		return nil, err
	}
	if allowed := pg.AccessFrom(ctx).Folders; allowed != nil {
		rows = slices.DeleteFunc(rows, func(row db.ListCollectionFoldersRow) bool { return !slices.Contains(allowed, row.ID) })
	}
	out := make([]folderView, len(rows))
	ids := make([]uuid.UUID, len(rows))
	for i, row := range rows {
		n, err := a.Queries.CountVisibleChildren(ctx, &row.ID)
		if err != nil {
			return nil, err
		}
		out[i] = folderView{row: row, childCount: n}
		ids[i] = row.ID
	}
	if user != nil && len(ids) > 0 {
		uds, err := a.Queries.ListUserData(ctx, db.ListUserDataParams{UserID: *user, ItemIds: ids})
		if err != nil {
			return nil, err
		}
		byItem := map[uuid.UUID]*db.UserDatum{}
		for i := range uds {
			byItem[uds[i].ItemID] = &uds[i]
		}
		for i := range out {
			out[i].userData = byItem[out[i].row.ID]
		}
	}
	return out, nil
}

// userViews lists the libraries the user can see (all of them until per-user
// library access lands in P4.1).
func (a *api) userViews(w http.ResponseWriter, r *http.Request, s auth.Session) {
	fs, err := a.folders(r, &s.UserID)
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	items := make([]dto.BaseItemDto, len(fs))
	for i, f := range fs {
		items[i] = a.collectionFolderDto(f, true)
	}
	jfapi.WriteJSON(w, r, http.StatusOK, queryResult(items))
}

func (a *api) mediaFolders(w http.ResponseWriter, r *http.Request, _ auth.Session) {
	fs, err := a.folders(r, nil)
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	items := make([]dto.BaseItemDto, len(fs))
	for i, f := range fs {
		items[i] = a.collectionFolderDto(f, false)
	}
	jfapi.WriteJSON(w, r, http.StatusOK, queryResult(items))
}

func (a *api) groupingOptions(w http.ResponseWriter, r *http.Request, _ auth.Session) {
	fs, err := a.folders(r, nil)
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	out := make([]dto.SpecialViewOptionDto, len(fs))
	for i, f := range fs {
		out[i] = dto.SpecialViewOptionDto{Name: ptr(f.row.Name), Id: ptr(dto.IDFromUUID(f.row.ID).String())}
	}
	jfapi.WriteJSON(w, r, http.StatusOK, out)
}

func (a *api) virtualFolders(w http.ResponseWriter, r *http.Request, _ auth.Session) {
	fs, err := a.folders(r, nil)
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	out := make([]dto.VirtualFolderInfo, 0, len(fs))
	for _, f := range fs {
		raw := libraryOptionsMovies
		if f.row.Kind == "tvshows" {
			raw = libraryOptionsTVShows
		}
		var opts dto.LibraryOptions
		if err := json.Unmarshal(raw, &opts); err != nil {
			a.internalError(w, r, err)
			return
		}
		infos := make([]dto.MediaPathInfo, len(f.row.Paths))
		for i := range f.row.Paths {
			infos[i] = dto.MediaPathInfo{Path: &f.row.Paths[i]}
		}
		opts.PathInfos = &infos
		id := dto.IDFromUUID(f.row.ID).String()
		paths := append([]string{}, f.row.Paths...)
		out = append(out, dto.VirtualFolderInfo{
			Name: ptr(f.row.Name), Locations: &paths, CollectionType: ptr(dto.CollectionTypeOptions(f.row.Kind)),
			LibraryOptions: &opts, ItemId: &id, PrimaryImageItemId: &id, RefreshStatus: ptr("Idle"),
		})
	}
	jfapi.WriteJSON(w, r, http.StatusOK, out)
}

// queryResult wraps items as Jellyfin's BaseItemDtoQueryResult (unpaged).
func queryResult(items []dto.BaseItemDto) dto.BaseItemDtoQueryResult {
	return dto.BaseItemDtoQueryResult{Items: &items, TotalRecordCount: ptr(int32(len(items))), StartIndex: ptr(int32(0))}
}
