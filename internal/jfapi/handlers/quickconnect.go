package handlers

import (
	"errors"
	"net/http"

	"github.com/sysadmin/blockbustr/internal/auth"
	"github.com/sysadmin/blockbustr/internal/jfapi"
	"github.com/sysadmin/blockbustr/internal/jfapi/dto"
)

// QuickConnect (TASKS P2.12): a device shows a 6-digit code and polls
// /QuickConnect/Connect with its secret (Jellyfin Android every ~5s); a
// signed-in user approves the code with /QuickConnect/Authorize; the device
// then logs in with /Users/AuthenticateWithQuickConnect.
func (a *api) registerQuickConnect(rt *jfapi.Router) {
	rt.Get("/QuickConnect/Enabled", func(w http.ResponseWriter, r *http.Request) {
		jfapi.WriteJSON(w, r, http.StatusOK, true)
	})
	rt.Post("/QuickConnect/Initiate", a.quickConnectInitiate)
	rt.Get("/QuickConnect/Connect", a.quickConnectConnect)
	rt.Post("/QuickConnect/Authorize", a.requireUser(a.quickConnectAuthorize))
	rt.Post("/Users/AuthenticateWithQuickConnect", a.authenticateWithQuickConnect)
}

func quickConnectResult(req auth.QuickConnectRequest) dto.QuickConnectResult {
	return dto.QuickConnectResult{
		Authenticated: ptr(req.Authenticated), Secret: ptr(req.Secret), Code: ptr(req.Code),
		DeviceId: ptr(req.Device.ID), DeviceName: ptr(req.Device.Name), AppName: ptr(req.Device.AppName),
		AppVersion: ptr(req.Device.AppVersion), DateAdded: ptr(dto.NewTime(req.DateAdded)),
	}
}

// quickConnectInitiate starts a request for the calling device, which has
// no token yet but must say who it is (like a password login).
func (a *api) quickConnectInitiate(w http.ResponseWriter, r *http.Request) {
	info := jfapi.AuthFrom(r.Context())
	if !info.HasClient() {
		errorText(w, http.StatusBadRequest)
		return
	}
	req, err := a.Auth.QuickConnectInitiate(r.Context(), auth.Device{ID: info.DeviceID, Name: info.Device, AppName: info.Client, AppVersion: info.Version})
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	jfapi.WriteJSON(w, r, http.StatusOK, quickConnectResult(req))
}

func (a *api) quickConnectConnect(w http.ResponseWriter, r *http.Request) {
	req, err := a.Auth.QuickConnectState(r.Context(), jfapi.QueryOf(r).Get("secret"))
	switch {
	case errors.Is(err, auth.ErrQuickConnectUnknown):
		errorText(w, http.StatusNotFound)
	case err != nil:
		a.internalError(w, r, err)
	default:
		jfapi.WriteJSON(w, r, http.StatusOK, quickConnectResult(req))
	}
}

// quickConnectAuthorize approves a code for the caller, or for userId when
// the caller is an administrator.
func (a *api) quickConnectAuthorize(w http.ResponseWriter, r *http.Request, s auth.Session) {
	user, ok := queryUser(r, s)
	if !ok {
		errorText(w, http.StatusForbidden)
		return
	}
	err := a.Auth.QuickConnectAuthorize(r.Context(), jfapi.QueryOf(r).Get("code"), user)
	switch {
	case errors.Is(err, auth.ErrQuickConnectUnknown):
		errorText(w, http.StatusNotFound)
	case err != nil:
		a.internalError(w, r, err)
	default:
		jfapi.WriteJSON(w, r, http.StatusOK, true)
	}
}

func (a *api) authenticateWithQuickConnect(w http.ResponseWriter, r *http.Request) {
	var body dto.QuickConnectDto
	if err := jfapi.DecodeJSON(w, r, &body); err != nil || body.Secret == "" {
		errorText(w, http.StatusBadRequest)
		return
	}
	u, dev, err := a.Auth.QuickConnectRedeem(r.Context(), body.Secret)
	switch {
	case errors.Is(err, auth.ErrQuickConnectUnknown), errors.Is(err, auth.ErrQuickConnectPending), errors.Is(err, auth.ErrInvalidCredentials):
		errorText(w, http.StatusUnauthorized)
		return
	case err != nil:
		a.internalError(w, r, err)
		return
	}
	// The token belongs to the device that asked; a newer header (e.g. an
	// app update) refreshes its details.
	if info := jfapi.AuthFrom(r.Context()); info.HasClient() && info.DeviceID == dev.ID {
		dev = auth.Device{ID: info.DeviceID, Name: info.Device, AppName: info.Client, AppVersion: info.Version}
	}
	a.writeLogin(w, r, u, dev, true)
}
