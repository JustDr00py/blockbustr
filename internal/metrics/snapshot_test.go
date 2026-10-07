package metrics

import (
	"math"
	"testing"

	dto "github.com/prometheus/client_model/go"
)

func TestSnapshot(t *testing.T) {
	// 19 fast requests and one slow 5xx on a route no other test uses.
	for range 19 {
		ObserveRequest("/snapshot-test", "GET", 200, 0.02)
	}
	ObserveRequest("/snapshot-test", "GET", 503, 3)
	ActiveProxies.Set(2)
	defer ActiveProxies.Set(0)

	s, err := Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if s.ActiveStreams != 2 || s.UptimeSeconds <= 0 || s.Goroutines <= 0 {
		t.Errorf("gauges: %+v", s)
	}
	var got *Timing
	for i := range s.Requests {
		if s.Requests[i].Name == "/snapshot-test" {
			got = &s.Requests[i]
		}
	}
	if got == nil {
		t.Fatalf("route missing from %+v", s.Requests)
	}
	// mean (19×20 ms + 3000 ms) / 20 = 169 ms; p95 is the 19th sample,
	// interpolated inside the 10–25 ms bucket.
	if got.Count != 20 || got.Errors != 1 || got.AvgMs != 169 || got.P95Ms < 10 || got.P95Ms > 25 || got.P95Bound {
		t.Errorf("timing = %+v", *got)
	}

	// All in the first bucket (≤ 5 ms): only the bound is known.
	for range 10 {
		ObserveRequest("/snapshot-fast", "GET", 200, 0.0001)
	}
	s, _ = Snapshot()
	for _, r := range s.Requests {
		if r.Name == "/snapshot-fast" && (!r.P95Bound || r.P95Ms != 5) {
			t.Errorf("fast route = %+v, want p95 bound 5 ms", r)
		}
	}
}

func TestQuantile(t *testing.T) {
	bucket := func(le float64, n uint64) *dto.Bucket { return &dto.Bucket{UpperBound: &le, CumulativeCount: &n} }
	hist := func(n uint64, b ...*dto.Bucket) *dto.Histogram { return &dto.Histogram{SampleCount: &n, Bucket: b} }
	cases := []struct {
		name string
		h    *dto.Histogram
		want float64
	}{
		{"empty", hist(0), 0},
		{"inside the first bucket", hist(10, bucket(1, 10), bucket(2, 10)), 0.95},
		{"interpolated in the second", hist(10, bucket(1, 5), bucket(3, 10)), 1 + 2*(9.5-5)/5},
		{"beyond the last bound", hist(10, bucket(1, 1), bucket(math.Inf(1), 10)), 1},
	}
	for _, c := range cases {
		if got := quantile(0.95, c.h); math.Abs(got-c.want) > 1e-9 {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}
