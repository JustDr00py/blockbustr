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

// renameLibrary serves POST /blockbustr/libraries/{folderId} {"Name"}: an
// admin renames a Stremio catalog library, the name clients show for it
// (its folder is renamed with it). Libraries from config.yaml are named
// there: 404. A name another library has: 409.
func (a *api) renameLibrary(w http.ResponseWriter, r *http.Request, _ auth.Session) {
	id, err := dto.ParseID(jfapi.URLParam(r, "folderId"))
	if err != nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	var body struct{ Name string }
	if err := jfapi.DecodeJSON(w, r, &body); err != nil {
		jfapi.WriteJSON(w, r, http.StatusBadRequest, map[string]string{"Error": "a JSON body with Name"})
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" || len(name) > 100 {
		jfapi.WriteJSON(w, r, http.StatusBadRequest, map[string]string{"Error": "Name must be 1–100 characters"})
		return
	}
	lib, err := a.Queries.RenameCatalogLibrary(r.Context(), db.RenameCatalogLibraryParams{
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
	if a.Events != nil { // apps listening refresh their library list
		a.Events.Publish(r.Context(), events.Event{Kind: events.LibraryChanged, Libraries: []uuid.UUID{lib}, Updated: []uuid.UUID{id.UUID()}})
	}
	noContent(w)
}
