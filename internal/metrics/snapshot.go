package metrics

import (
	"cmp"
	"math"
	"slices"
	"strings"
	"time"

	dto "github.com/prometheus/client_model/go"
)

// started is when the process began, for Stats.UptimeSeconds.
var started = time.Now()

// Stats is the admin UI's view of the metrics (GET /blockbustr/stats): the
// same numbers /metrics serves, summarised, and available whether or not
// /metrics is enabled. Totals count since the server started.
type Stats struct {
	UptimeSeconds  float64
	ActiveStreams  float64 // remote streams being proxied now
	ProxiedBytes   float64
	Transcodes     float64
	DebridAccounts float64
	DBInUse        float64
	DBIdle         float64
	HeapBytes      float64
	Goroutines     float64
	Requests       []Timing // by route, busiest first
	Addons         []Timing // by resource (stream, meta, catalog…)
	Resolves       []Count  // by kind and result
	Search         Timing
}

// Timing summarises a histogram: how many, how many failed (5xx for
// requests, errors for addons), and the mean and 95th percentile in ms.
// P95Bound means the percentile fell in the first bucket, where nothing
// finer is known: P95Ms is then that bucket's bound ("at most").
type Timing struct {
	Name     string
	Count    float64
	Errors   float64
	AvgMs    float64
	P95Ms    float64
	P95Bound bool `json:",omitempty"`
}

// Count is one labelled counter.
type Count struct {
	Name  string
	Count float64
}

// Snapshot gathers the registry into Stats.
func Snapshot() (Stats, error) {
	fams, err := Registry.Gather()
	if err != nil {
		return Stats{}, err
	}
	s := Stats{UptimeSeconds: time.Since(started).Seconds()}
	byName := map[string]*dto.MetricFamily{}
	for _, f := range fams {
		byName[f.GetName()] = f
	}
	single := func(name string) float64 {
		f := byName[name]
		if f == nil || len(f.GetMetric()) == 0 {
			return 0
		}
		m := f.GetMetric()[0]
		switch {
		case m.GetGauge() != nil:
			return m.GetGauge().GetValue()
		case m.GetCounter() != nil:
			return m.GetCounter().GetValue()
		}
		return 0
	}
	s.ActiveStreams = single("blockbustr_stream_active_proxies")
	s.ProxiedBytes = single("blockbustr_stream_proxied_bytes_total")
	s.Transcodes = single("blockbustr_transcode_sessions")
	s.DebridAccounts = single("blockbustr_debrid_accounts_active")
	s.DBInUse = single("blockbustr_db_connections_in_use")
	s.DBIdle = single("blockbustr_db_connections_idle")
	s.HeapBytes = single("go_memstats_heap_alloc_bytes")
	s.Goroutines = single("go_goroutines")

	s.Requests = timings(byName["blockbustr_http_request_duration_seconds"], "route")
	addErrors(s.Requests, byName["blockbustr_http_requests_total"], "route", func(m *dto.Metric) bool {
		return strings.HasPrefix(label(m, "code"), "5")
	})
	s.Addons = timings(byName["blockbustr_addon_request_duration_seconds"], "resource")
	addErrors(s.Addons, byName["blockbustr_addon_requests_total"], "resource", func(m *dto.Metric) bool {
		return label(m, "result") != "ok"
	})
	if f := byName["blockbustr_resolve_total"]; f != nil {
		for _, m := range f.GetMetric() {
			s.Resolves = append(s.Resolves, Count{Name: label(m, "kind") + " " + label(m, "result"), Count: m.GetCounter().GetValue()})
		}
		slices.SortFunc(s.Resolves, func(a, b Count) int { return cmp.Compare(b.Count, a.Count) })
	}
	if t := timings(byName["blockbustr_search_duration_seconds"], ""); len(t) > 0 {
		s.Search = t[0]
	}
	return s, nil
}

// timings summarises each labelled histogram of f, busiest first.
func timings(f *dto.MetricFamily, by string) []Timing {
	if f == nil {
		return nil
	}
	var out []Timing
	for _, m := range f.GetMetric() {
		h := m.GetHistogram()
		n := float64(h.GetSampleCount())
		if n == 0 {
			continue
		}
		t := Timing{Name: label(m, by), Count: n, AvgMs: round1(h.GetSampleSum() / n * 1000)}
		if b := h.GetBucket(); len(b) > 0 && float64(b[0].GetCumulativeCount()) >= 0.95*n {
			t.P95Ms, t.P95Bound = round1(b[0].GetUpperBound()*1000), true
		} else {
			t.P95Ms = round1(quantile(0.95, h) * 1000)
		}
		out = append(out, t)
	}
	slices.SortFunc(out, func(a, b Timing) int { return cmp.Or(cmp.Compare(b.Count, a.Count), strings.Compare(a.Name, b.Name)) })
	return out
}

// addErrors adds the counters of f that failed (per failed) to the timing
// with the same by label.
func addErrors(ts []Timing, f *dto.MetricFamily, by string, failed func(*dto.Metric) bool) {
	if f == nil {
		return
	}
	errs := map[string]float64{}
	for _, m := range f.GetMetric() {
		if failed(m) {
			errs[label(m, by)] += m.GetCounter().GetValue()
		}
	}
	for i := range ts {
		ts[i].Errors = errs[ts[i].Name]
	}
}

// quantile estimates the q-quantile of h by linear interpolation inside
// the bucket that holds it, as Prometheus's histogram_quantile does. Past
// the last bucket it answers that bucket's bound.
func quantile(q float64, h *dto.Histogram) float64 {
	total := float64(h.GetSampleCount())
	if total == 0 {
		return 0
	}
	rank := q * total
	lower, prev := 0.0, 0.0
	for _, b := range h.GetBucket() {
		upper, count := b.GetUpperBound(), float64(b.GetCumulativeCount())
		if math.IsInf(upper, 1) {
			return lower
		}
		if count >= rank {
			if count == prev {
				return upper
			}
			return lower + (upper-lower)*(rank-prev)/(count-prev)
		}
		lower, prev = upper, count
	}
	return lower
}

func label(m *dto.Metric, name string) string {
	for _, l := range m.GetLabel() {
		if l.GetName() == name {
			return l.GetValue()
		}
	}
	return ""
}

func round1(v float64) float64 { return math.Round(v*10) / 10 }
