package handlers

import (
	"context"
	"errors"
	"net/http"

	"github.com/sysadmin/blockbustr/internal/auth"
	"github.com/sysadmin/blockbustr/internal/jfapi"
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
