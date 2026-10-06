package handlers

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/sysadmin/blockbustr/internal/auth"
	"github.com/sysadmin/blockbustr/internal/jfapi"
	"github.com/sysadmin/blockbustr/internal/jfapi/dto"
	"github.com/sysadmin/blockbustr/internal/stremio"
)

// registerAddons mounts blockbustr's own admin API for Stremio addons
// (TASKS P3.4, DESIGN §7). These routes aren't Jellyfin's, so they live
// under /blockbustr and answer JSON errors ({"Error": "..."}). An addon's
// URL is write-only: no response ever carries it.
func (a *api) registerAddons(rt *jfapi.Router) {
	if a.Addons == nil {
		return
	}
	rt.Get("/blockbustr/addons", a.requireAdmin(a.listAddons))
	rt.Post("/blockbustr/addons", a.requireAdmin(a.addAddon))
	rt.Get("/blockbustr/addons/{addonId}", a.requireAdmin(a.getAddon))
	rt.Post("/blockbustr/addons/{addonId}", a.requireAdmin(a.updateAddon))
	rt.Delete("/blockbustr/addons/{addonId}", a.requireAdmin(a.deleteAddon))
	rt.Post("/blockbustr/addons/{addonId}/refresh", a.requireAdmin(a.refreshAddon))
	rt.Post("/blockbustr/addons/{addonId}/catalogs/{type}/{catalogId}", a.requireAdmin(a.setCatalog))
}

// addonDto is an addon as the API shows it.
type addonDto struct {
	Id            dto.ID
	Host          string
	Name          string
	Version       string
	Description   string
	ManifestId    string
	Types         []string
	Resources     []string
	Enabled       bool
	Priority      int
	LastFetchedAt time.Time
	Catalogs      []catalogDto
}

type catalogDto struct {
	Type      string
	Id        string
	Name      string
	Enabled   bool
	LibraryId *dto.ID
	// Requires lists extras the catalog can't be fetched without; such a
	// catalog can't be enabled.
	Requires []string
}

func toAddonDto(ad stremio.Addon) addonDto {
	out := addonDto{
		Id: dto.IDFromUUID(ad.ID), Host: ad.Host, Name: ad.Manifest.Name, Version: ad.Manifest.Version,
		Description: ad.Manifest.Description, ManifestId: ad.Manifest.ID, Types: ad.Manifest.Types,
		Enabled: ad.Enabled, Priority: ad.Priority, LastFetchedAt: ad.LastFetchedAt.UTC(),
		Resources: []string{}, Catalogs: []catalogDto{},
	}
	for _, r := range ad.Manifest.Resources {
		out.Resources = append(out.Resources, r.Name)
	}
	for _, c := range ad.Catalogs {
		cd := catalogDto{Type: c.Type, Id: c.ID, Name: c.Name, Enabled: c.Enabled, Requires: c.Requires}
		if cd.Requires == nil {
			cd.Requires = []string{}
		}
		if c.LibraryID != nil {
			cd.LibraryId = ptr(dto.IDFromUUID(*c.LibraryID))
		}
		out.Catalogs = append(out.Catalogs, cd)
	}
	return out
}

// addonError answers a registry error with its status and message. The
// addon's own failures (fetch, decode) are 502; their messages name only
// its host. Anything else is a server fault.
func (a *api) addonError(w http.ResponseWriter, r *http.Request, err error) {
	var status int
	switch {
	case errors.Is(err, stremio.ErrNotFound):
		status = http.StatusNotFound
	case errors.Is(err, stremio.ErrDuplicate):
		status = http.StatusConflict
	case errors.Is(err, stremio.ErrBadURL), errors.Is(err, stremio.ErrCatalogNeedsArg):
		status = http.StatusBadRequest
	case errors.Is(err, stremio.ErrNoSecretKey):
		status = http.StatusServiceUnavailable
	case strings.HasPrefix(err.Error(), "stremio "):
		status = http.StatusBadGateway
	default:
		a.internalError(w, r, err)
		return
	}
	jfapi.WriteJSON(w, r, status, map[string]string{"Error": err.Error()})
}

func badBody(w http.ResponseWriter, r *http.Request, want string) {
	jfapi.WriteJSON(w, r, http.StatusBadRequest, map[string]string{"Error": "body must be " + want})
}

func (a *api) addonID(w http.ResponseWriter, r *http.Request) (dto.ID, bool) {
	id, err := dto.ParseID(jfapi.URLParam(r, "addonId"))
	if err != nil {
		w.WriteHeader(http.StatusNotFound)
		return id, false
	}
	return id, true
}

func (a *api) listAddons(w http.ResponseWriter, r *http.Request, _ auth.Session) {
	list, err := a.Addons.List(r.Context())
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	out := make([]addonDto, 0, len(list))
	for _, ad := range list {
		out = append(out, toAddonDto(ad))
	}
	jfapi.WriteJSON(w, r, http.StatusOK, out)
}

func (a *api) addAddon(w http.ResponseWriter, r *http.Request, _ auth.Session) {
	var body struct {
		Url      string
		Priority int
	}
	if err := jfapi.DecodeJSON(w, r, &body); err != nil || body.Url == "" {
		badBody(w, r, `{"Url": "https://…/manifest.json", "Priority": int}`)
		return
	}
	ad, err := a.Addons.Add(r.Context(), body.Url, body.Priority)
	if err != nil {
		a.addonError(w, r, err)
		return
	}
	a.Log.InfoContext(r.Context(), "stremio addon added", "addon", ad.Manifest.ID, "host", ad.Host, "catalogs", len(ad.Catalogs))
	jfapi.WriteJSON(w, r, http.StatusOK, toAddonDto(ad))
}

func (a *api) getAddon(w http.ResponseWriter, r *http.Request, _ auth.Session) {
	id, ok := a.addonID(w, r)
	if !ok {
		return
	}
	ad, err := a.Addons.Get(r.Context(), id.UUID())
	if err != nil {
		a.addonError(w, r, err)
		return
	}
	jfapi.WriteJSON(w, r, http.StatusOK, toAddonDto(ad))
}

func (a *api) updateAddon(w http.ResponseWriter, r *http.Request, _ auth.Session) {
	id, ok := a.addonID(w, r)
	if !ok {
		return
	}
	var body struct {
		Enabled  *bool
		Priority *int
	}
	if err := jfapi.DecodeJSON(w, r, &body); err != nil {
		badBody(w, r, `{"Enabled": bool, "Priority": int}`)
		return
	}
	ad, err := a.Addons.Update(r.Context(), id.UUID(), body.Enabled, body.Priority)
	if err != nil {
		a.addonError(w, r, err)
		return
	}
	jfapi.WriteJSON(w, r, http.StatusOK, toAddonDto(ad))
}

func (a *api) deleteAddon(w http.ResponseWriter, r *http.Request, _ auth.Session) {
	id, ok := a.addonID(w, r)
	if !ok {
		return
	}
	if err := a.Addons.Delete(r.Context(), id.UUID()); err != nil {
		a.addonError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *api) refreshAddon(w http.ResponseWriter, r *http.Request, _ auth.Session) {
	id, ok := a.addonID(w, r)
	if !ok {
		return
	}
	ad, err := a.Addons.Refresh(r.Context(), id.UUID())
	if err != nil {
		a.addonError(w, r, err)
		return
	}
	jfapi.WriteJSON(w, r, http.StatusOK, toAddonDto(ad))
}

func (a *api) setCatalog(w http.ResponseWriter, r *http.Request, _ auth.Session) {
	id, ok := a.addonID(w, r)
	if !ok {
		return
	}
	var body struct{ Enabled *bool }
	if err := jfapi.DecodeJSON(w, r, &body); err != nil || body.Enabled == nil {
		badBody(w, r, `{"Enabled": bool}`)
		return
	}
	ad, err := a.Addons.SetCatalogEnabled(r.Context(), id.UUID(), jfapi.URLParam(r, "type"), jfapi.URLParam(r, "catalogId"), *body.Enabled)
	if err != nil {
		a.addonError(w, r, err)
		return
	}
	jfapi.WriteJSON(w, r, http.StatusOK, toAddonDto(ad))
}
