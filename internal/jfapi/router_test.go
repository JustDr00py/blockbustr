package jfapi

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sysadmin/blockbustr/internal/jfapi/dto"
)

func newTestRouter(t *testing.T, logs *bytes.Buffer) *Router {
	t.Helper()
	rt := NewRouter(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})), Options{})
	rt.Get("/System/Info/Public", func(w http.ResponseWriter, r *http.Request) {
		name, version := "blockbustr", "12.1.0"
		WriteJSON(w, r, http.StatusOK, dto.PublicSystemInfo{ServerName: &name, Version: &version})
	})
	rt.Get("/Items/{itemId}/Images/{imageType}", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, URLParam(r, "itemId")+"|"+URLParam(r, "imageType"))
	})
	rt.Post("/Items/{itemId}/PlaybackInfo", func(w http.ResponseWriter, r *http.Request) {
		var body dto.PlaybackInfoDto
		if err := DecodeJSON(w, r, &body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		br, aidx := int32(0), int32(-1)
		if body.MaxStreamingBitrate != nil {
			br = *body.MaxStreamingBitrate
		}
		if body.AudioStreamIndex != nil {
			aidx = *body.AudioStreamIndex
		}
		WriteJSON(w, r, http.StatusOK, map[string]int32{"Bitrate": br, "Audio": aidx})
	})
	rt.Get("/boom", func(http.ResponseWriter, *http.Request) { panic("kaboom") })
	rt.Get("/whoami", func(w http.ResponseWriter, r *http.Request) {
		a := AuthFrom(r.Context())
		_, _ = io.WriteString(w, a.Client+"|"+a.DeviceID+"|"+a.Token+"|"+a.TokenSource)
	})
	return rt
}

func do(rt http.Handler, method, path string, body string, hdr map[string]string) *httptest.ResponseRecorder {
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req := httptest.NewRequestWithContext(context.Background(), method, path, rd)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	rt.ServeHTTP(rec, req)
	return rec
}

func TestRouterCaseInsensitivePaths(t *testing.T) {
	var logs bytes.Buffer
	rt := newTestRouter(t, &logs)
	rec := do(rt, "GET", "/items/84088E11/images/Primary?ApiKey=secret-token", "", nil)
	if rec.Code != 200 || rec.Body.String() != "84088E11|Primary" {
		t.Fatalf("got %d %q", rec.Code, rec.Body.String())
	}
	if strings.Contains(logs.String(), "secret-token") || !strings.Contains(logs.String(), "path=/Items/84088E11/Images/Primary") {
		t.Errorf("request log must show the canonical path and never the query:\n%s", logs.String())
	}
	if rec := do(rt, "GET", "/does/not/exist", "", nil); rec.Code != 404 {
		t.Errorf("unknown route = %d", rec.Code)
	}
}

func TestRouterJSONCasing(t *testing.T) {
	rt := newTestRouter(t, &bytes.Buffer{})
	rec := do(rt, "GET", "/system/info/public", "", nil)
	if rec.Body.String() != `{"ServerName":"blockbustr","Version":"12.1.0"}` || rec.Header().Get("Content-Type") != contentTypeJSON {
		t.Errorf("pascal: %s %q", rec.Body, rec.Header().Get("Content-Type"))
	}
	rec = do(rt, "GET", "/System/Info/Public", "", map[string]string{"Accept": `application/json; profile="CamelCase"`})
	if rec.Body.String() != `{"serverName":"blockbustr","version":"12.1.0"}` || rec.Header().Get("Content-Type") != contentTypeCamelJSON {
		t.Errorf("camel: %s %q", rec.Body, rec.Header().Get("Content-Type"))
	}
}

func TestRouterDecodesCamelCaseBodies(t *testing.T) {
	rt := newTestRouter(t, &bytes.Buffer{})
	// Streamyfin's PlaybackInfo body shape (camelCase keys).
	rec := do(rt, "POST", "/Items/x/PlaybackInfo", `{"userId":"bbbb0000000000000000000000000001","maxStreamingBitrate":4000000,"audioStreamIndex":2,"deviceProfile":{}}`, nil)
	if rec.Code != 200 || rec.Body.String() != `{"Audio":2,"Bitrate":4000000}` {
		t.Errorf("camel body: %d %s", rec.Code, rec.Body)
	}
	if rec := do(rt, "POST", "/Items/x/PlaybackInfo", "", nil); rec.Code != 200 || rec.Body.String() != `{"Audio":-1,"Bitrate":0}` {
		t.Errorf("empty body: %d %s", rec.Code, rec.Body)
	}
	if rec := do(rt, "POST", "/Items/x/PlaybackInfo", `{"maxStreamingBitrate":`, nil); rec.Code != 400 {
		t.Errorf("bad body: %d", rec.Code)
	}
	big := `{"x":"` + strings.Repeat("a", MaxBodyBytes) + `"}`
	if rec := do(rt, "POST", "/Items/x/PlaybackInfo", big, nil); rec.Code != 400 {
		t.Errorf("oversized body: %d", rec.Code)
	}
}

func TestRouterCORS(t *testing.T) {
	rt := newTestRouter(t, &bytes.Buffer{})
	rec := do(rt, "OPTIONS", "/Items/Latest", "", map[string]string{
		"Origin": "http://x", "Access-Control-Request-Method": "GET", "Access-Control-Request-Headers": "authorization,content-type",
	})
	h := rec.Header()
	if rec.Code != 204 || h.Get("Access-Control-Allow-Origin") != "*" || h.Get("Access-Control-Allow-Methods") != "GET" ||
		h.Get("Access-Control-Allow-Headers") != "authorization,content-type" {
		t.Errorf("preflight: %d %v", rec.Code, h)
	}
	if rec := do(rt, "GET", "/System/Info/Public", "", map[string]string{"Origin": "http://x"}); rec.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Error("simple request missing CORS header")
	}
}

func TestRouterRecoversPanics(t *testing.T) {
	var logs bytes.Buffer
	rt := newTestRouter(t, &logs)
	rec := do(rt, "GET", "/boom", "", nil)
	if rec.Code != 500 || !strings.Contains(logs.String(), "kaboom") || !strings.Contains(logs.String(), "level=ERROR") {
		t.Errorf("panic: %d\n%s", rec.Code, logs.String())
	}
}

func TestRouterStoresAuthInfo(t *testing.T) {
	rt := newTestRouter(t, &bytes.Buffer{})
	rec := do(rt, "GET", "/WhoAmI?ApiKey=q-token", "", map[string]string{
		"Authorization": `MediaBrowser Client="Jellyfin%20for%20Android", DeviceId="d1", Token="null"`,
	})
	if rec.Body.String() != "Jellyfin for Android|d1|q-token|query:ApiKey" {
		t.Errorf("auth info = %q", rec.Body.String())
	}
	// The router built with default Options rejects legacy forms.
	if rec := do(rt, "GET", "/whoami", "", map[string]string{"X-Emby-Token": "legacy"}); rec.Body.String() != "|||" {
		t.Errorf("legacy header accepted without LegacyAuth: %q", rec.Body.String())
	}
}
