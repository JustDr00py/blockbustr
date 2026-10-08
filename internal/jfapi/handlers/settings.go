package handlers

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/sysadmin/blockbustr/internal/auth"
	"github.com/sysadmin/blockbustr/internal/jfapi"
	"github.com/sysadmin/blockbustr/internal/settings"
)

// The admin UI's Settings page: GET /blockbustr/settings lists every
// setting with its value and where it comes from (a secret only says
// whether it's set); POST {"key": value, …} changes them at once, null
// resetting one to its default. A key config.yaml or the environment sets
// is locked: 409.

func (a *api) registerSettings(rt *jfapi.Router) {
	if a.Settings == nil {
		return
	}
	rt.Get("/blockbustr/settings", a.requireAdmin(a.getSettings))
	rt.Post("/blockbustr/settings", a.requireAdmin(a.setSettings))
}

func (a *api) getSettings(w http.ResponseWriter, r *http.Request, _ auth.Session) {
	jfapi.WriteJSON(w, r, http.StatusOK, a.Settings.Views())
}

func (a *api) setSettings(w http.ResponseWriter, r *http.Request, s auth.Session) {
	var changes map[string]json.RawMessage
	if err := jfapi.DecodeJSON(w, r, &changes); err != nil || len(changes) == 0 {
		jfapi.WriteJSON(w, r, http.StatusBadRequest, map[string]string{"Error": "a JSON object of settings"})
		return
	}
	err := a.Settings.Set(r.Context(), changes)
	switch {
	case errors.Is(err, settings.ErrLocked):
		jfapi.WriteJSON(w, r, http.StatusConflict, map[string]string{"Error": strings.TrimPrefix(err.Error(), "settings: ")})
		return
	case errors.Is(err, settings.ErrUnknown), errors.Is(err, settings.ErrInvalid), errors.Is(err, settings.ErrNoSecretKey):
		jfapi.WriteJSON(w, r, http.StatusBadRequest, map[string]string{"Error": strings.TrimPrefix(err.Error(), "settings: ")})
		return
	case err != nil:
		a.internalError(w, r, err)
		return
	}
	keys := make([]string, 0, len(changes))
	for k := range changes {
		keys = append(keys, k)
	}
	a.Log.InfoContext(r.Context(), "settings changed", "by", s.UserName, "keys", keys)
	noContent(w)
}
