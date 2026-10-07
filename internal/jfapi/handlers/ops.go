package handlers

import (
	"net/http"
	"net/http/pprof"

	"github.com/sysadmin/blockbustr/internal/auth"
	"github.com/sysadmin/blockbustr/internal/jfapi"
	"github.com/sysadmin/blockbustr/internal/metrics"
)

// registerOps mounts the operator endpoints (P4.4): Prometheus metrics when
// enabled, the admin UI's summary of them (/blockbustr/stats, always on, for
// admins), and Go's pprof for admins.
func (a *api) registerOps(rt *jfapi.Router) {
	if a.Config.Metrics.Enabled {
		m := metrics.Handler(a.Config.Metrics.Token)
		rt.Get("/metrics", m.ServeHTTP)
	}
	rt.Get("/blockbustr/stats", a.requireAdmin(func(w http.ResponseWriter, r *http.Request, _ auth.Session) {
		s, err := metrics.Snapshot()
		if err != nil {
			a.internalError(w, r, err)
			return
		}
		jfapi.WriteJSON(w, r, http.StatusOK, s)
	}))
	admin := func(h http.HandlerFunc) http.HandlerFunc {
		return a.requireAdmin(func(w http.ResponseWriter, r *http.Request, _ auth.Session) { h(w, r) })
	}
	rt.Get("/debug/pprof/cmdline", admin(pprof.Cmdline))
	rt.Get("/debug/pprof/profile", admin(pprof.Profile))
	rt.Get("/debug/pprof/symbol", admin(pprof.Symbol))
	rt.Get("/debug/pprof/trace", admin(pprof.Trace))
	rt.Get("/debug/pprof/*", admin(pprof.Index)) // index and named profiles (heap, goroutine…)
}
