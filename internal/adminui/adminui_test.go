package adminui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Without a build the placeholder answers every path; with one, index.html
// does (checked by the Makefile's UI build, not here).
func TestHandlerServesAPage(t *testing.T) {
	h := Handler()
	for _, p := range []string{Prefix, Prefix + "users", Prefix + "nope/deep"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, p, nil))
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), "blockbustr admin") {
			t.Errorf("%s: %d %.80q", p, rec.Code, rec.Body.String())
		}
		if !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/html") {
			t.Errorf("%s: content type %q", p, rec.Header().Get("Content-Type"))
		}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, Prefix+"../../etc/passwd", nil))
	if strings.Contains(rec.Body.String(), "root:") {
		t.Error("path traversal")
	}
}
