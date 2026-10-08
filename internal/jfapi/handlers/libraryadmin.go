package handlers

import (
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sysadmin/blockbustr/internal/auth"
	"github.com/sysadmin/blockbustr/internal/events"
	"github.com/sysadmin/blockbustr/internal/jfapi"
	"github.com/sysadmin/blockbustr/internal/jfapi/dto"
	"github.com/sysadmin/blockbustr/internal/library"
	"github.com/sysadmin/blockbustr/internal/store/pg/db"
)

// The admin's Libraries page: every enabled library in the order users
// get them, which the admin changes, hides from all users, or (a Stremio
// catalog library) renames. Libraries are named by their folder's item id,
// what clients and the admin UI know.

func (a *api) registerLibraryAdmin(rt *jfapi.Router) {
	rt.Get("/blockbustr/libraries", a.requireAdmin(a.listLibraries))
	rt.Post("/blockbustr/libraries/order", a.requireAdmin(a.orderLibraries))
	rt.Post("/blockbustr/libraries/{folderId}", a.requireAdmin(a.updateLibrary))
}

// adminLibrary is one row of GET /blockbustr/libraries.
type adminLibrary struct {
	Id             string
	Name           string
	CollectionType string
	Locations      []string
	Catalog        bool // a Stremio catalog library: renamable here
	Hidden         bool
}

func (a *api) listLibraries(w http.ResponseWriter, r *http.Request, _ auth.Session) {
	fs, err := a.folders(r, nil)
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	out := make([]adminLibrary, len(fs))
	for i, f := range fs {
		out[i] = adminLibrary{
			Id: dto.IDFromUUID(f.row.ID).String(), Name: f.row.Name, CollectionType: f.row.Kind,
			Locations: append([]string{}, f.row.Paths...), Catalog: len(f.row.Paths) == 0, Hidden: f.row.Hidden,
		}
	}
	jfapi.WriteJSON(w, r, http.StatusOK, out)
}

// orderLibraries serves POST /blockbustr/libraries/order {"Ids": [...]}:
// the libraries in that order, first to last (a user's own order, from
// their app's settings, still comes first for them).
func (a *api) orderLibraries(w http.ResponseWriter, r *http.Request, _ auth.Session) {
	var body struct{ Ids []string }
	if err := jfapi.DecodeJSON(w, r, &body); err != nil || len(body.Ids) == 0 {
		jfapi.WriteJSON(w, r, http.StatusBadRequest, map[string]string{"Error": "a JSON body with Ids"})
		return
	}
	ids := make([]uuid.UUID, len(body.Ids))
	for i, s := range body.Ids {
		id, err := dto.ParseID(s)
		if err != nil {
			jfapi.WriteJSON(w, r, http.StatusBadRequest, map[string]string{"Error": "not a library id: " + s})
			return
		}
		ids[i] = id.UUID()
	}
	libs, err := a.Queries.SetLibraryOrder(r.Context(), ids)
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	a.librariesChanged(r, libs, ids)
	noContent(w)
}

// updateLibrary serves POST /blockbustr/libraries/{folderId} with Name
// and/or Hidden. Hidden takes any library out of every user's views and
// Latest rows (it keeps syncing, and stays searchable). Name renames a
// Stremio catalog library, the name clients show (its folder is renamed
// with it); libraries from config.yaml are named there: 404. A name
// another library has: 409.
func (a *api) updateLibrary(w http.ResponseWriter, r *http.Request, _ auth.Session) {
	id, err := dto.ParseID(jfapi.URLParam(r, "folderId"))
	if err != nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	var body struct {
		Name   *string
		Hidden *bool
	}
	if err := jfapi.DecodeJSON(w, r, &body); err != nil || (body.Name == nil && body.Hidden == nil) {
		jfapi.WriteJSON(w, r, http.StatusBadRequest, map[string]string{"Error": "a JSON body with Name or Hidden"})
		return
	}
	var name string
	if body.Name != nil {
		name = strings.TrimSpace(*body.Name)
		if name == "" || len(name) > 100 {
			jfapi.WriteJSON(w, r, http.StatusBadRequest, map[string]string{"Error": "Name must be 1–100 characters"})
			return
		}
	}
	var lib uuid.UUID
	if body.Hidden != nil {
		lib, err = a.Queries.SetLibraryHidden(r.Context(), db.SetLibraryHiddenParams{Hidden: *body.Hidden, FolderID: id.UUID()})
		if errors.Is(err, pgx.ErrNoRows) {
			jfapi.WriteJSON(w, r, http.StatusNotFound, map[string]string{"Error": "no such library"})
			return
		}
		if err != nil {
			a.internalError(w, r, err)
			return
		}
	}
	if body.Name != nil {
		lib, err = a.Queries.RenameCatalogLibrary(r.Context(), db.RenameCatalogLibraryParams{
			Name: name, SortName: library.SortName(name), FolderID: id.UUID(),
		})
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			jfapi.WriteJSON(w, r, http.StatusNotFound, map[string]string{"Error": "not an addon catalog library (file libraries are named in config.yaml)"})
			return
		case isUniqueViolation(err):
			jfapi.WriteJSON(w, r, http.StatusConflict, map[string]string{"Error": "another library already has that name"})
			return
		case err != nil:
			a.internalError(w, r, err)
			return
		}
	}
	a.librariesChanged(r, []uuid.UUID{lib}, []uuid.UUID{id.UUID()})
	noContent(w)
}

// librariesChanged tells listening apps to refresh their library list.
func (a *api) librariesChanged(r *http.Request, libs, folders []uuid.UUID) {
	if a.Events != nil {
		a.Events.Publish(r.Context(), events.Event{Kind: events.LibraryChanged, Libraries: libs, Updated: folders})
	}
}
