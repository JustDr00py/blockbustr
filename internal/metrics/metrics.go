// Package metrics is blockbustr's Prometheus instrumentation (TASKS P4.4):
// the counters and histograms the request path updates, on a registry of
// its own (with the Go and process collectors), served at /metrics when
// enabled. Labels are bounded: routes are chi patterns, never raw paths.
package metrics

import (
	"crypto/subtle"
	"net/http"
	"strconv"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Registry holds every blockbustr metric.
var Registry = prometheus.NewRegistry()

var (
	// HTTPRequests counts requests by route pattern, method and status.
	HTTPRequests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "blockbustr_http_requests_total", Help: "HTTP requests by route pattern, method and status code.",
	}, []string{"route", "method", "code"})
	// HTTPDuration times requests by route pattern (streams and HLS
	// segments last as long as the client reads).
	HTTPDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name: "blockbustr_http_request_duration_seconds", Help: "HTTP request duration by route pattern.",
		Buckets: []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30},
	}, []string{"route"})

	// ProxiedBytes counts bytes proxied from remote sources to clients.
	ProxiedBytes = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "blockbustr_stream_proxied_bytes_total", Help: "Bytes proxied from remote sources (debrid, addons, .strm) to clients.",
	})
	// ActiveProxies is how many remote streams are being proxied now.
	ActiveProxies = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "blockbustr_stream_active_proxies", Help: "Remote streams being proxied right now.",
	})

	// Resolves counts source resolutions by kind (file, url, debrid,
	// torrent) and result (ok, cached, downloading, error).
	Resolves = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "blockbustr_resolve_total", Help: "Source resolutions by kind and result.",
	}, []string{"kind", "result"})

	// AddonRequests counts Stremio addon requests by resource (manifest,
	// catalog, meta, stream, subtitles) and result (ok, error).
	AddonRequests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "blockbustr_addon_requests_total", Help: "Stremio addon requests by resource and result (cache hits excluded).",
	}, []string{"resource", "result"})
	// AddonDuration times addon requests by resource.
	AddonDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name: "blockbustr_addon_request_duration_seconds", Help: "Stremio addon request duration by resource.",
		Buckets: []float64{.05, .1, .25, .5, 1, 2, 4, 8},
	}, []string{"resource"})

	// SearchDuration times remote searches (TMDB and addons, merged).
	SearchDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name: "blockbustr_search_duration_seconds", Help: "Remote search duration (all sources).",
		Buckets: []float64{.05, .1, .25, .5, 1, 2, 3, 5},
	})
)

func init() {
	Registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		HTTPRequests, HTTPDuration, ProxiedBytes, ActiveProxies, Resolves, AddonRequests, AddonDuration, SearchDuration,
	)
}

// Gauge registers a gauge read from f at scrape time (pool stats,
// transcode sessions).
func Gauge(name, help string, f func() float64) {
	Registry.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: name, Help: help}, f))
}

// ObserveRequest records one HTTP request.
func ObserveRequest(route, method string, status int, seconds float64) {
	HTTPRequests.WithLabelValues(route, method, strconv.Itoa(status)).Inc()
	HTTPDuration.WithLabelValues(route).Observe(seconds)
}

// Handler serves the metrics. With a token, a request needs
// "Authorization: Bearer <token>".
func Handler(token string) http.Handler {
	h := promhttp.HandlerFor(Registry, promhttp.HandlerOpts{})
	if token == "" {
		return h
	}
	want := []byte("Bearer " + token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), want) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="blockbustr metrics"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	})
}
