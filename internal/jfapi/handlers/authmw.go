package handlers

import (
	"context"
	"errors"
	"net/http"

	"github.com/sysadmin/blockbustr/internal/auth"
	"github.com/sysadmin/blockbustr/internal/jfapi"
	"github.com/sysadmin/blockbustr/internal/store/pg"
)

// sessionHandler is a handler that runs for a signed-in user.
type sessionHandler func(w http.ResponseWriter, r *http.Request, s auth.Session)

// requireUser resolves the request's token (any accepted form, see
// jfapi.ParseAuth). Like Jellyfin 12.1.0, a missing or invalid token gets a
// bare 401 with no body.
func (a *api) requireUser(h sessionHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s, err := a.Auth.Resolve(r.Context(), jfapi.AuthFrom(r.Context()).Token)
		if errors.Is(err, auth.ErrNoSession) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if err != nil {
			a.internalError(w, r, err)
			return
		}
		// Media sources point remote items at this server's own stream URL.
		r = r.WithContext(context.WithValue(r.Context(), baseURLKey{}, localAddress(a.Config.Server.ExternalURL, r)))
		// Every item query of the request sees only what the user's policy
		// allows (P4.1).
		r = r.WithContext(pg.WithAccess(r.Context(), s.Access))
		r = r.WithContext(context.WithValue(r.Context(), mayDownloadKey{}, !s.NoPlayback && !s.NoDownload))
		a.touchSession(r, s) // GET /Sessions lists recently active devices
		h(w, r, s)
	}
}

type baseURLKey struct{}

// baseURL is the URL clients reach this server at, for the request in ctx
// (set by requireUser; "" elsewhere).
func baseURL(ctx context.Context) string {
	s, _ := ctx.Value(baseURLKey{}).(string)
	return s
}

type mayDownloadKey struct{}

// mayDownload says whether the user of the request in ctx may download
// media (set by requireUser; false elsewhere).
func mayDownload(ctx context.Context) bool {
	ok, _ := ctx.Value(mayDownloadKey{}).(bool)
	return ok
}

// requireAdmin is requireUser plus an administrator check (403 otherwise).
func (a *api) requireAdmin(h sessionHandler) http.HandlerFunc {
	return a.requireUser(func(w http.ResponseWriter, r *http.Request, s auth.Session) {
		if !s.IsAdmin {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		h(w, r, s)
	})
}
