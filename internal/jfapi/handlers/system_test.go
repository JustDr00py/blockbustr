package handlers

import (
	"context"
	"net/http/httptest"
	"regexp"
	"testing"
)

func TestSystemInfoPublicContract(t *testing.T) {
	for _, b := range checkContract(t, "GET /System/Info/Public") {
		m := b.(map[string]any)
		for k, want := range map[string]any{
			"Version": "12.1.0", "ProductName": "Jellyfin Server", "StartupWizardCompleted": true,
			"OperatingSystem": "", "ServerName": "blockbustr", "Id": "cccc0000000000000000000000000001",
		} {
			if m[k] != want {
				t.Errorf("%s = %v, want %v", k, m[k], want)
			}
		}
		if !regexp.MustCompile(`^https?://[^/]+$`).MatchString(m["LocalAddress"].(string)) {
			t.Errorf("LocalAddress = %v", m["LocalAddress"])
		}
	}
}

// No captured client calls /System/Ping; expected values were probed on the
// recon server: both methods return the product name as a JSON string.
func TestPing(t *testing.T) {
	h, _ := newTestServer(t)
	for _, method := range []string{"GET", "POST"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), method, "/system/ping", nil))
		if rec.Code != 200 || rec.Body.String() != `"Jellyfin Server"` || rec.Header().Get("Content-Type") != "application/json; charset=utf-8" {
			t.Errorf("%s ping = %d %s %q", method, rec.Code, rec.Body, rec.Header().Get("Content-Type"))
		}
	}
}

func TestBrandingContract(t *testing.T) {
	for _, b := range checkContract(t, "GET /Branding/Configuration") {
		if m := b.(map[string]any); m["SplashscreenEnabled"] != false {
			t.Errorf("branding = %v", m)
		}
	}
	h, _ := newTestServer(t)
	for _, p := range []string{"/Branding/Css", "/branding/css.css"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), "GET", p, nil))
		if rec.Code != 200 || rec.Header().Get("Content-Type") != "text/css; charset=utf-8" || rec.Body.Len() != 0 {
			t.Errorf("%s = %d %q %q", p, rec.Code, rec.Header().Get("Content-Type"), rec.Body)
		}
	}
}

func TestLocalAddress(t *testing.T) {
	r := httptest.NewRequestWithContext(context.Background(), "GET", "http://media.lan:8096/System/Info/Public", nil)
	if got := localAddress("", r); got != "http://media.lan:8096" {
		t.Errorf("from request: %q", got)
	}
	r.Header.Set("X-Forwarded-Proto", "https")
	if got := localAddress("", r); got != "https://media.lan:8096" {
		t.Errorf("behind TLS proxy: %q", got)
	}
	if got := localAddress("https://blockbustr.example.ts.net/", r); got != "https://blockbustr.example.ts.net" {
		t.Errorf("configured: %q", got)
	}
}
