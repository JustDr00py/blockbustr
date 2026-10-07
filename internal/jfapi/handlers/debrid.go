package handlers

import (
	"context"
	"errors"
	"net/http"

	"github.com/sysadmin/blockbustr/internal/auth"
	"github.com/sysadmin/blockbustr/internal/debrid"
	"github.com/sysadmin/blockbustr/internal/jfapi"
	"github.com/sysadmin/blockbustr/internal/provider"
)

// DebridManager manages the debrid accounts (debrid.Manager).
type DebridManager interface {
	List(ctx context.Context) ([]debrid.Account, error)
	Put(ctx context.Context, name provider.Name, apiKey *string, enabled *bool, priority *int) error
	Delete(ctx context.Context, name provider.Name) error
	Check(ctx context.Context, name provider.Name) debrid.Status
	CanStore() bool
}

// blockbustr's admin API for debrid accounts (TASKS P4.3), next to the
// addons one: JSON errors ({"Error": "..."}); keys are write-only.

func (a *api) registerDebrid(rt *jfapi.Router) {
	rt.Post("/blockbustr/catalogs/sync", a.requireAdmin(func(w http.ResponseWriter, r *http.Request, _ auth.Session) {
		a.syncCatalogs()
		w.WriteHeader(http.StatusNoContent)
	}))
	if a.Debrid == nil {
		return
	}
	rt.Get("/blockbustr/debrid", a.requireAdmin(a.listDebrid))
	rt.Post("/blockbustr/debrid/{provider}", a.requireAdmin(a.putDebrid))
	rt.Delete("/blockbustr/debrid/{provider}", a.requireAdmin(a.deleteDebrid))
	rt.Get("/blockbustr/debrid/{provider}/status", a.requireAdmin(a.debridStatus))
}

func (a *api) debridError(w http.ResponseWriter, r *http.Request, err error) {
	var status int
	switch {
	case errors.Is(err, debrid.ErrBadInput):
		status = http.StatusBadRequest
	case errors.Is(err, debrid.ErrNotFound):
		status = http.StatusNotFound
	case errors.Is(err, debrid.ErrNoSecretKey):
		status = http.StatusServiceUnavailable
	default:
		a.internalError(w, r, err)
		return
	}
	jfapi.WriteJSON(w, r, status, map[string]string{"Error": err.Error()})
}

func (a *api) listDebrid(w http.ResponseWriter, r *http.Request, _ auth.Session) {
	accounts, err := a.Debrid.List(r.Context())
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	jfapi.WriteJSON(w, r, http.StatusOK, map[string]any{
		"Accounts":  accounts,
		"Providers": []provider.Name{provider.RealDebrid, provider.TorBox},
		"CanStore":  a.Debrid.CanStore(),
	})
}

func (a *api) putDebrid(w http.ResponseWriter, r *http.Request, s auth.Session) {
	var body struct {
		ApiKey   *string
		Enabled  *bool
		Priority *int
	}
	if err := jfapi.DecodeJSON(w, r, &body); err != nil {
		jfapi.WriteJSON(w, r, http.StatusBadRequest, map[string]string{"Error": `body must be {"ApiKey": "...", "Enabled": bool, "Priority": int}`})
		return
	}
	name := provider.Name(jfapi.URLParam(r, "provider"))
	if err := a.Debrid.Put(r.Context(), name, body.ApiKey, body.Enabled, body.Priority); err != nil {
		a.debridError(w, r, err)
		return
	}
	a.Log.InfoContext(r.Context(), "debrid account changed", "provider", name, "key_changed", body.ApiKey != nil)
	a.listDebrid(w, r, s)
}

func (a *api) deleteDebrid(w http.ResponseWriter, r *http.Request, _ auth.Session) {
	name := provider.Name(jfapi.URLParam(r, "provider"))
	if err := a.Debrid.Delete(r.Context(), name); err != nil {
		a.debridError(w, r, err)
		return
	}
	a.Log.InfoContext(r.Context(), "debrid account deleted", "provider", name)
	w.WriteHeader(http.StatusNoContent)
}

func (a *api) debridStatus(w http.ResponseWriter, r *http.Request, _ auth.Session) {
	jfapi.WriteJSON(w, r, http.StatusOK, a.Debrid.Check(r.Context(), provider.Name(jfapi.URLParam(r, "provider"))))
}
