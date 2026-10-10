package handlers

import (
	"context"
	"net/http"
	"net/http/pprof"
	"time"

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
		s.Host = metrics.ReadHost(map[string]string{
			"Cache": a.Config.Paths.Cache, "Transcode": a.Config.Paths.TranscodeDir(), "Images": a.Config.Paths.ImagesDir(),
		})
		s.ProxiedMonths = a.proxiedMonths(r.Context())
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

// proxiedMonths is the stored monthly proxied-bytes totals, newest first,
// with the bytes not stored yet added to this month's.
func (a *api) proxiedMonths(ctx context.Context) []metrics.MonthBytes {
	rows, err := a.Queries.ListProxiedBytes(ctx, 12)
	if err != nil {
		a.Log.WarnContext(ctx, "reading the monthly proxied bytes failed", "err", err)
	}
	var out []metrics.MonthBytes
	for _, r := range rows {
		out = append(out, metrics.MonthBytes{Month: r.Month.Format("2006-01"), Bytes: float64(r.Bytes)})
	}
	var pending int64
	if a.MonthlyBytes != nil {
		pending = a.MonthlyBytes.Unflushed()
	}
	this := metrics.MonthStart(time.Now()).Format("2006-01")
	if len(out) == 0 || out[0].Month != this {
		out = append([]metrics.MonthBytes{{Month: this}}, out...)
	}
	out[0].Bytes += float64(pending)
	return out
}
