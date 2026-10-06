// Package jfapi is blockbustr's Jellyfin-compatible HTTP layer (DESIGN §3):
// routing, content negotiation, request helpers and, later, the handlers.
// Domain packages must not import it.
package jfapi

import (
	"log/slog"
	"net/http"
	"net/url"

	"github.com/go-chi/chi/v5"
)

// Router serves the Jellyfin API. Routes match case-insensitively; parameter
// values keep their original case (see canonicalizer).
type Router struct {
	mux     *chi.Mux
	canon   *canonicalizer
	handler http.Handler
}

// Options configures the router.
type Options struct {
	// LegacyAuth also accepts the pre-12.x auth forms (X-Emby-*, ?api_key=,
	// "Emby" scheme); see ParseAuth and compat.legacy_auth.
	LegacyAuth bool
}

// NewRouter builds an empty router with the standard middleware chain:
// panic recovery → request log → CORS → auth parsing → case-insensitive routing.
func NewRouter(log *slog.Logger, opts Options) *Router {
	rt := &Router{mux: chi.NewRouter(), canon: newCanonicalizer()}
	// Like Jellyfin (Kestrel): unknown routes get a bare 404 with no body.
	// Clients probe plugin routes this way (/Streamyfin/config).
	rt.mux.NotFound(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) })
	rt.handler = recoverer(log, logRequests(log, cors(withAuthInfo(opts.LegacyAuth, http.HandlerFunc(rt.route)))))
	return rt
}

func (rt *Router) ServeHTTP(w http.ResponseWriter, r *http.Request) { rt.handler.ServeHTTP(w, r) }

func (rt *Router) route(w http.ResponseWriter, r *http.Request) {
	if p, ok := rt.canon.canonical(r.URL.EscapedPath()); ok {
		if unescaped, err := url.PathUnescape(p); err == nil {
			r.URL.Path, r.URL.RawPath = unescaped, p
		}
	}
	rt.mux.ServeHTTP(w, r)
}

// Method registers h for method and a chi pattern (which may contain
// parameters such as "{itemId}" or "stream.{container}").
func (rt *Router) Method(method, pattern string, h http.HandlerFunc) {
	rt.canon.add(pattern)
	rt.mux.Method(method, pattern, h)
}

// Get registers a GET route.
func (rt *Router) Get(pattern string, h http.HandlerFunc) { rt.Method(http.MethodGet, pattern, h) }

// Head registers a HEAD route (Jellyfin clients HEAD streams and images).
func (rt *Router) Head(pattern string, h http.HandlerFunc) { rt.Method(http.MethodHead, pattern, h) }

// Post registers a POST route.
func (rt *Router) Post(pattern string, h http.HandlerFunc) { rt.Method(http.MethodPost, pattern, h) }

// Delete registers a DELETE route.
func (rt *Router) Delete(pattern string, h http.HandlerFunc) {
	rt.Method(http.MethodDelete, pattern, h)
}

// URLParam returns a route parameter, e.g. URLParam(r, "itemId").
func URLParam(r *http.Request, name string) string { return chi.URLParam(r, name) }
