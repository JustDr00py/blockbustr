package handlers

import (
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/sysadmin/blockbustr/internal/auth"
	"github.com/sysadmin/blockbustr/internal/jfapi"
	"github.com/sysadmin/blockbustr/internal/jfapi/dto"
	"github.com/sysadmin/blockbustr/internal/store/pg/db"
)

// User data writes (TASKS P1.24, DESIGN §3.11): played/unplayed, favourite
// and /UserItems/{id}/UserData. Every write answers with the item's updated
// UserItemDataDto (200), as 12.1.0 does, including the legacy routes.

func (a *api) registerUserData(rt *jfapi.Router) {
	for _, p := range []string{"/UserPlayedItems/{itemId}", "/Users/{userId}/PlayedItems/{itemId}"} {
		rt.Post(p, a.requireUser(a.markPlayed))
		rt.Delete(p, a.requireUser(a.markUnplayed))
	}
	for _, p := range []string{"/UserFavoriteItems/{itemId}", "/Users/{userId}/FavoriteItems/{itemId}"} {
		rt.Post(p, a.requireUser(a.setFavorite(true)))
		rt.Delete(p, a.requireUser(a.setFavorite(false)))
	}
	rt.Get("/UserItems/{itemId}/UserData", a.requireUser(a.getUserData))
	rt.Post("/UserItems/{itemId}/UserData", a.requireUser(a.updateUserData))
}

// userDataTarget resolves the user and the visible item a request names.
// ok is false when a response (403/404) was already written.
func (a *api) userDataTarget(w http.ResponseWriter, r *http.Request, s auth.Session) (uuid.UUID, db.Item, bool) {
	user, ok := queryUser(r, s)
	if !ok {
		errorText(w, http.StatusForbidden)
		return uuid.UUID{}, db.Item{}, false
	}
	id, err := dto.ParseID(jfapi.URLParam(r, "itemId"))
	if err != nil {
		w.WriteHeader(http.StatusNotFound)
		return uuid.UUID{}, db.Item{}, false
	}
	it, found, err := a.visibleItem(r, user, id.UUID())
	if err != nil {
		a.internalError(w, r, err)
		return uuid.UUID{}, db.Item{}, false
	}
	if !found {
		w.WriteHeader(http.StatusNotFound)
		return uuid.UUID{}, db.Item{}, false
	}
	return user, it, true
}

// writeUserData answers with the item's user data as listings show it
// (folders include their played/unplayed episode counts).
func (a *api) writeUserData(w http.ResponseWriter, r *http.Request, user uuid.UUID, it db.Item) {
	ds, err := a.itemDtos(r.Context(), &user, []db.Item{it}, dtoOptions{fields: map[string]bool{}, imageTypeLimit: -1, enableUserData: true})
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	jfapi.WriteJSON(w, r, http.StatusOK, ds[0].UserData)
}

// markPlayed marks the item, or every episode/movie below it, played.
// datePlayed (optional) counts as a new play on that date.
func (a *api) markPlayed(w http.ResponseWriter, r *http.Request, s auth.Session) {
	user, it, ok := a.userDataTarget(w, r, s)
	if !ok {
		return
	}
	var played *time.Time
	if raw := jfapi.QueryOf(r).Get("datePlayed"); raw != "" {
		var t dto.Time
		if err := t.UnmarshalText([]byte(raw)); err != nil {
			errorText(w, http.StatusBadRequest)
			return
		}
		played = &t.Time
	}
	if err := a.Queries.MarkPlayed(r.Context(), db.MarkPlayedParams{UserID: user, ItemID: it.ID, DatePlayed: played}); err != nil {
		a.internalError(w, r, err)
		return
	}
	a.writeUserData(w, r, user, it)
}

// markUnplayed resets the item, or everything below it: not played, no
// plays, no resume point, no last played date.
func (a *api) markUnplayed(w http.ResponseWriter, r *http.Request, s auth.Session) {
	user, it, ok := a.userDataTarget(w, r, s)
	if !ok {
		return
	}
	if err := a.Queries.MarkUnplayed(r.Context(), db.MarkUnplayedParams{UserID: user, ItemID: it.ID}); err != nil {
		a.internalError(w, r, err)
		return
	}
	a.writeUserData(w, r, user, it)
}

// setFavorite favourites the item itself (a series, not its episodes).
func (a *api) setFavorite(fav bool) sessionHandler {
	return func(w http.ResponseWriter, r *http.Request, s auth.Session) {
		user, it, ok := a.userDataTarget(w, r, s)
		if !ok {
			return
		}
		if err := a.Queries.SetFavorite(r.Context(), db.SetFavoriteParams{UserID: user, ItemID: it.ID, IsFavorite: fav}); err != nil {
			a.internalError(w, r, err)
			return
		}
		a.writeUserData(w, r, user, it)
	}
}

func (a *api) getUserData(w http.ResponseWriter, r *http.Request, s auth.Session) {
	if user, it, ok := a.userDataTarget(w, r, s); ok {
		a.writeUserData(w, r, user, it)
	}
}

// updateUserData stores the fields the body sets on the item itself; the
// rest keep their values. Likes, Key, ItemId and the derived
// PlayedPercentage/UnplayedItemCount are ignored.
func (a *api) updateUserData(w http.ResponseWriter, r *http.Request, s auth.Session) {
	user, it, ok := a.userDataTarget(w, r, s)
	if !ok {
		return
	}
	var body dto.UpdateUserItemDataDto
	if err := jfapi.DecodeJSON(w, r, &body); err != nil {
		errorText(w, http.StatusBadRequest)
		return
	}
	p := db.UpdateUserDataParams{
		UserID: user, ItemID: it.ID, Played: body.Played, PlayCount: body.PlayCount,
		PositionTicks: body.PlaybackPositionTicks, IsFavorite: body.IsFavorite,
	}
	if body.Rating != nil {
		p.Rating = ptr(float32(*body.Rating))
	}
	if body.LastPlayedDate != nil {
		p.LastPlayedAt = &body.LastPlayedDate.Time
	}
	if err := a.Queries.UpdateUserData(r.Context(), p); err != nil {
		a.internalError(w, r, err)
		return
	}
	a.writeUserData(w, r, user, it)
}
