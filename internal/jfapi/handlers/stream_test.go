package handlers

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// headerWriter records the status and headers of a response and refuses
// the body, so replaying a multi-GB stream stops after the headers.
type headerWriter struct {
	h    http.Header
	code int
}

var errBodyRefused = errors.New("body not wanted")

func (w *headerWriter) Header() http.Header { return w.h }
func (w *headerWriter) WriteHeader(code int) {
	if w.code == 0 {
		w.code = code
	}
}
func (w *headerWriter) Write([]byte) (int, error) {
	w.WriteHeader(http.StatusOK)
	return 0, errBodyRefused
}

// sparseFile creates a file of size bytes (no disk used) with mtime modified.
func sparseFile(t *testing.T, size int64, modified time.Time) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "video")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(size); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	if err := os.Chtimes(p, modified, modified); err != nil {
		t.Fatal(err)
	}
	return p
}

// Every captured stream request gets Jellyfin's status and headers: the
// files are sparse stand-ins of the recorded size and modification time.
func TestStreamContract(t *testing.T) {
	h, d := newIntegrationServer(t)
	seedRecon(t, d)
	files := map[string]bool{}
	for _, c := range capturesFor(t, "GET /Videos/{id}/stream") {
		size := c.Response.Headers["Content-Length"]
		if cr := c.Response.Headers["Content-Range"]; cr != "" {
			size = cr[strings.LastIndex(cr, "/")+1:]
		}
		total, _ := strconv.ParseInt(size, 10, 64)
		modified, err := http.ParseTime(c.Response.Headers["Last-Modified"])
		if err != nil || total == 0 {
			t.Fatalf("%s: headers %v", c.Name, c.Response.Headers)
		}
		id := strings.ReplaceAll(strings.Split(c.Request.Path, "/")[2], "-", "")
		if !files[id] {
			execSQL(t, `UPDATE media_sources SET path_or_url = $1, is_remote = false, protocol = 'File' WHERE item_id = $2`, sparseFile(t, total, modified), id)
			files[id] = true
		}

		target := c.Request.Path + "?" + url.Values(c.Request.Query).Encode()
		req := httptest.NewRequestWithContext(context.Background(), "GET", target, nil)
		for k, v := range c.Request.Headers {
			req.Header.Set(k, v)
		}
		w := &headerWriter{h: http.Header{}}
		h.ServeHTTP(w, req)
		if w.code != c.Response.Status {
			t.Errorf("%s: status %d, Jellyfin %d", c.Name, w.code, c.Response.Status)
		}
		for _, k := range []string{"Content-Length", "Content-Type", "Accept-Ranges", "Content-Range", "Last-Modified"} {
			if got, want := w.h.Get(k), c.Response.Headers[k]; got != want {
				t.Errorf("%s: %s %q, Jellyfin %q", c.Name, k, got, want)
			}
		}
		if w.h.Get("ETag") != "" {
			t.Errorf("%s: Jellyfin sends no ETag", c.Name)
		}
	}
}

func TestStreamBytesAndErrors(t *testing.T) {
	h, d := newIntegrationServer(t)
	seedRecon(t, d)
	const mkii = "1f77a87a9603bff7d1589d1e076f060c"
	p := filepath.Join(t.TempDir(), "movie.mp4")
	if err := os.WriteFile(p, []byte("0123456789"), 0o644); err != nil {
		t.Fatal(err)
	}
	execSQL(t, `UPDATE media_sources SET path_or_url = $1 WHERE item_id = $2`, p, mkii)

	get := func(method, path string, hdr map[string]string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequestWithContext(context.Background(), method, path, nil)
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	rec := get("GET", "/Videos/"+mkii+"/stream?static=true", map[string]string{"Range": "bytes=2-5"})
	if body, _ := io.ReadAll(rec.Body); rec.Code != 206 || string(body) != "2345" || rec.Header().Get("Content-Range") != "bytes 2-5/10" {
		t.Errorf("range: %d %q %v", rec.Code, body, rec.Header())
	}
	rec = get("HEAD", "/videos/"+mkii+"/stream.mkv", nil) // lower-case route, extension names the container
	if rec.Code != 200 || rec.Body.Len() != 0 || rec.Header().Get("Content-Length") != "10" || rec.Header().Get("Content-Type") != "video/x-matroska" {
		t.Errorf("HEAD: %d %v", rec.Code, rec.Header())
	}
	lm := rec.Header().Get("Last-Modified")
	if rec := get("GET", "/Videos/"+mkii+"/stream", map[string]string{"If-Modified-Since": lm}); rec.Code != 304 {
		t.Errorf("revalidation = %d", rec.Code)
	}
	if rec := get("GET", "/Videos/"+mkii+"/stream?mediaSourceId="+mkii, nil); rec.Code != 200 || rec.Body.String() != "0123456789" || rec.Header().Get("Content-Type") != "video/mp4" {
		t.Errorf("whole file: %d %q", rec.Code, rec.Body)
	}
	for path, why := range map[string]string{
		"/Videos/" + mkii + "/stream?mediaSourceId=00000000000000000000000000000042": "unknown media source",
		"/Videos/" + mfGhost + "/stream":                                             "a series",
		"/Videos/00000000000000000000000000000042/stream":                            "unknown item",
		"/Videos/" + fockers + "/stream":                                             "remote source (P2.4b)",
	} {
		if rec := get("GET", path, nil); rec.Code != 404 {
			t.Errorf("%s: %d", why, rec.Code)
		}
	}
	execSQL(t, `UPDATE media_sources SET path_or_url = '/nonexistent/x.mp4' WHERE item_id = $1`, mkii)
	if rec := get("GET", "/Videos/"+mkii+"/stream", nil); rec.Code != 404 {
		t.Errorf("missing file = %d", rec.Code)
	}
}
