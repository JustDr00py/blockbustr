package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/sysadmin/blockbustr/internal/jfapi"
	"github.com/sysadmin/blockbustr/internal/testutil"
)

func TestMetricsAndPprof(t *testing.T) {
	_, d := newIntegrationServer(t)
	seedCaptureToken(t, d)
	d.Config.Metrics.Enabled, d.Config.Metrics.Token = true, "scrape-token"
	rt := jfapi.NewRouter(testutil.Discard(), jfapi.Options{LegacyAuth: true})
	Register(rt, d)
	admin := `MediaBrowser Token="` + captureToken + `"`

	// Some traffic: a fixed route and one with an id in it.
	call(t, rt, "GET", "/System/Info", admin, "")
	call(t, rt, "GET", "/Items/0123456789abcdef0123456789abcdef", admin, "")

	scrape := func(authz string) *httptest.ResponseRecorder {
		req := httptest.NewRequestWithContext(t.Context(), "GET", "/metrics", nil)
		if authz != "" {
			req.Header.Set("Authorization", authz)
		}
		rec := httptest.NewRecorder()
		rt.ServeHTTP(rec, req)
		return rec
	}
	if rec := scrape(""); rec.Code != http.StatusUnauthorized {
		t.Errorf("no token: %d", rec.Code)
	}
	if rec := scrape("Bearer wrong"); rec.Code != http.StatusUnauthorized {
		t.Errorf("wrong token: %d", rec.Code)
	}
	rec := scrape("Bearer scrape-token")
	body := rec.Body.String()
	if rec.Code != 200 {
		t.Fatalf("scrape: %d", rec.Code)
	}
	for _, want := range []string{
		`blockbustr_http_requests_total{code="200",method="GET",route="/System/Info"}`,
		`route="/Items/{itemId}"`, // the pattern, not the id
		"go_goroutines", "blockbustr_stream_proxied_bytes_total",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics lack %s", want)
		}
	}
	if strings.Contains(body, "0123456789abcdef0123456789abcdef") {
		t.Error("a raw id became a label")
	}

	// pprof: admins only.
	if rec := call(t, rt, "GET", "/debug/pprof/", "", ""); rec.Code != 401 {
		t.Errorf("pprof anonymous: %d", rec.Code)
	}
	if rec := call(t, rt, "GET", "/debug/pprof/", admin, ""); rec.Code != 200 || !strings.Contains(rec.Body.String(), "goroutine") {
		t.Errorf("pprof index: %d", rec.Code)
	}
	if rec := call(t, rt, "GET", "/debug/pprof/goroutine?debug=1", admin, ""); rec.Code != 200 || !strings.Contains(rec.Body.String(), "goroutine profile") {
		t.Errorf("goroutine profile: %d", rec.Code)
	}

	// Disabled: no /metrics at all, but the admin UI's stats still work.
	d.Config.Metrics.Enabled = false
	off := jfapi.NewRouter(testutil.Discard(), jfapi.Options{LegacyAuth: true})
	Register(off, d)
	if rec := call(t, off, "GET", "/metrics", "", ""); rec.Code != 404 {
		t.Errorf("disabled /metrics: %d", rec.Code)
	}
	if rec := call(t, off, "GET", "/blockbustr/stats", "", ""); rec.Code != 401 {
		t.Errorf("stats anonymous: %d", rec.Code)
	}
	rec = call(t, off, "GET", "/blockbustr/stats", admin, "")
	var stats struct {
		UptimeSeconds float64
		Requests      []struct {
			Name  string
			Count float64
		}
	}
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &stats) != nil || stats.UptimeSeconds <= 0 {
		t.Fatalf("stats: %d %s", rec.Code, rec.Body)
	}
	if !slices.ContainsFunc(stats.Requests, func(r struct {
		Name  string
		Count float64
	}) bool {
		return r.Name == "/System/Info" && r.Count >= 1
	}) {
		t.Errorf("stats lack /System/Info: %+v", stats.Requests)
	}
}
