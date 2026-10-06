package images

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func encode(t *testing.T, format string, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{uint8(x * 255 / w), uint8(y * 255 / h), 120, 255})
		}
	}
	var buf bytes.Buffer
	if format == "png" {
		_ = png.Encode(&buf, img)
	} else {
		_ = jpeg.Encode(&buf, img, &jpeg.Options{Quality: 95})
	}
	return buf.Bytes()
}

// origin serves files and counts requests per path.
func origin(t *testing.T, files map[string][]byte) (*httptest.Server, *sync.Map) {
	t.Helper()
	hits := &sync.Map{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, _ := hits.LoadOrStore(r.URL.Path, new(atomic.Int32))
		n.(*atomic.Int32).Add(1)
		data, ok := files[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(data)
	}))
	t.Cleanup(srv.Close)
	return srv, hits
}

func hitCount(m *sync.Map, path string) int32 {
	v, ok := m.Load(path)
	if !ok {
		return 0
	}
	return v.(*atomic.Int32).Load()
}

func dims(t *testing.T, path string) (int, int, string) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	cfg, format, err := image.DecodeConfig(f)
	if err != nil {
		t.Fatal(err)
	}
	return cfg.Width, cfg.Height, format
}

func TestTargetSize(t *testing.T) {
	cases := []struct {
		w, h   int
		r      Request
		ww, wh int
	}{
		{1000, 1500, Request{}, 1000, 1500},                               // nothing asked: original
		{1000, 1500, Request{MaxWidth: 300}, 300, 450},                    // fit width
		{1000, 1500, Request{MaxHeight: 300}, 200, 300},                   // fit height
		{1000, 1500, Request{MaxWidth: 300, MaxHeight: 300}, 200, 300},    // fit box
		{1000, 1500, Request{FillWidth: 778, FillHeight: 438}, 780, 1170}, // cover box (Jellyfin Android)
		{1920, 1080, Request{FillHeight: 389}, 700, 394},                  // Streamyfin thumb, rounded to 20 px
		{1920, 1080, Request{Width: 1000}, 1000, 563},                     // Streamyfin backdrop
		{500, 500, Request{MaxWidth: 4000}, 500, 500},                     // never upscale
		{500, 500, Request{FillWidth: 2000}, 500, 500},
		{1000, 1500, Request{MaxWidth: 389}, 400, 600}, // 389 → 400
	}
	for _, c := range cases {
		if w, h := targetSize(c.w, c.h, c.r); w != c.ww || h != c.wh {
			t.Errorf("targetSize(%dx%d, %+v) = %dx%d, want %dx%d", c.w, c.h, c.r, w, h, c.ww, c.wh)
		}
	}
}

func TestGetResizesCachesAndKeepsFormat(t *testing.T) {
	srv, hits := origin(t, map[string][]byte{"/poster.jpg": encode(t, "jpeg", 1000, 1500), "/logo.png": encode(t, "png", 800, 300)})
	s := New(t.TempDir())
	ctx := t.Context()
	poster := Source{URL: srv.URL + "/poster.jpg", Tag: Tag(srv.URL + "/poster.jpg")}

	r, err := s.Get(ctx, poster, Request{MaxWidth: 300, Quality: 80})
	if err != nil {
		t.Fatal(err)
	}
	if w, h, f := dims(t, r.Path); w != 300 || h != 450 || f != "jpeg" || r.ContentType != "image/jpeg" {
		t.Errorf("poster rendition %dx%d %s %s", w, h, f, r.ContentType)
	}
	if !strings.Contains(r.Path, "300x450-q80.jpg") {
		t.Errorf("rendition path %s", r.Path)
	}
	// Same request again, and a different size: the origin is fetched once.
	if _, err := s.Get(ctx, poster, Request{MaxWidth: 300, Quality: 80}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx, poster, Request{FillHeight: 200}); err != nil {
		t.Fatal(err)
	}
	if n := hitCount(hits, "/poster.jpg"); n != 1 {
		t.Errorf("origin fetched %d times", n)
	}
	// No size and no quality: the original file itself.
	if r, _ := s.Get(ctx, poster, Request{}); !strings.Contains(r.Path, filepath.Join("orig", poster.Tag[:2], poster.Tag)) {
		t.Errorf("original not served as-is: %s", r.Path)
	}

	logo := Source{URL: srv.URL + "/logo.png", Tag: Tag(srv.URL + "/logo.png")}
	r, err = s.Get(ctx, logo, Request{MaxWidth: 400, Quality: 96})
	if err != nil {
		t.Fatal(err)
	}
	if w, _, f := dims(t, r.Path); w != 400 || f != "png" || r.ContentType != "image/png" {
		t.Errorf("logo stays PNG (alpha): %d %s %s", w, f, r.ContentType)
	}
	if r, _ := s.Get(ctx, logo, Request{MaxWidth: 400, Format: "jpg"}); r.ContentType != "image/jpeg" {
		t.Errorf("format=jpg: %s", r.ContentType)
	}
}

func TestConcurrentRequestsBuildOnce(t *testing.T) {
	srv, hits := origin(t, map[string][]byte{"/b.jpg": encode(t, "jpeg", 1920, 1080)})
	s := New(t.TempDir())
	src := Source{URL: srv.URL + "/b.jpg", Tag: Tag(srv.URL + "/b.jpg")}
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.Get(context.Background(), src, Request{Width: 1000})
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if n := hitCount(hits, "/b.jpg"); n != 1 {
		t.Errorf("origin fetched %d times for concurrent requests", n)
	}
	matches, _ := filepath.Glob(filepath.Join(s.dir, "r", "*", src.Tag, "*"))
	if len(matches) != 1 {
		t.Errorf("renditions on disk: %v (no temp files left behind)", matches)
	}
}

func TestInspect(t *testing.T) {
	srv, _ := origin(t, map[string][]byte{"/p.jpg": encode(t, "jpeg", 600, 900)})
	s := New(t.TempDir())
	info, err := s.Inspect(t.Context(), Source{URL: srv.URL + "/p.jpg", Tag: "t1"})
	if err != nil || info.Width != 600 || info.Height != 900 || len(info.Blurhash) < 6 {
		t.Errorf("inspect: %+v %v", info, err)
	}
	local := filepath.Join(t.TempDir(), "folder.png")
	if err := os.WriteFile(local, encode(t, "png", 300, 200), 0o600); err != nil {
		t.Fatal(err)
	}
	if info, err := s.Inspect(t.Context(), Source{LocalPath: local}); err != nil || info.Width != 300 {
		t.Errorf("local inspect: %+v %v", info, err)
	}
}

func TestRejectsBadSources(t *testing.T) {
	srv, _ := origin(t, map[string][]byte{"/notes.txt": []byte("not an image")})
	s := New(t.TempDir())
	ctx := t.Context()
	for name, src := range map[string]Source{
		"not an image":  {URL: srv.URL + "/notes.txt", Tag: "a"},
		"404":           {URL: srv.URL + "/missing.jpg", Tag: "b"},
		"bad scheme":    {URL: "file:///etc/passwd", Tag: "c"},
		"no tag":        {URL: srv.URL + "/x.jpg"},
		"missing local": {LocalPath: "/nonexistent/x.jpg"},
		"empty":         {},
	} {
		if _, err := s.Get(ctx, src, Request{}); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := os.Stat(filepath.Join(s.dir, "orig", "a", "a")); err == nil {
		t.Error("non-image was cached")
	}
	if Tag("x") == Tag("y") || len(Tag("x")) != 16 {
		t.Error("Tag")
	}
}
