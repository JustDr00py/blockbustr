package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/sysadmin/blockbustr/internal/images"
	"github.com/sysadmin/blockbustr/internal/jfapi"
	"github.com/sysadmin/blockbustr/internal/jfapi/dto"
	"github.com/sysadmin/blockbustr/internal/store/pg/db"
	"github.com/sysadmin/blockbustr/internal/testutil"
)

func jpegBytes(w, h int) []byte {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{uint8(x), uint8(y), 90, 255})
		}
	}
	var buf bytes.Buffer
	_ = jpeg.Encode(&buf, img, nil)
	return buf.Bytes()
}

type imageEnv struct {
	h                     http.Handler
	item, person, noPhoto string
	posterTag, origin     string
}

func newImageEnv(t *testing.T) imageEnv {
	t.Helper()
	_, d := newIntegrationServer(t)
	poster := jpegBytes(600, 900)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/down.jpg" {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_, _ = w.Write(poster)
	}))
	t.Cleanup(origin.Close)

	ctx := t.Context()
	lib, err := d.Queries.CreateLibrary(ctx, db.CreateLibraryParams{Name: "Movies", Kind: "movies", Paths: []string{"/media/Movies"}})
	if err != nil {
		t.Fatal(err)
	}
	item, err := d.Queries.CreateItem(ctx, db.CreateItemParams{LibraryID: lib.ID, Type: "Movie", Name: "Luca", SortName: "luca", SourceKind: "virtual"})
	if err != nil {
		t.Fatal(err)
	}
	for _, im := range []struct{ typ, url string }{{"Primary", origin.URL + "/p.jpg"}, {"Backdrop", origin.URL + "/b.jpg"}, {"Logo", origin.URL + "/down.jpg"}} {
		if err := d.Queries.UpsertImage(ctx, db.UpsertImageParams{ItemID: item.ID, Type: im.typ, Idx: 0, SourceUrl: &im.url, Tag: images.Tag(im.url)}); err != nil {
			t.Fatal(err)
		}
	}
	person, noPhoto := uuid.New(), uuid.New()
	photo := origin.URL + "/jt.jpg"
	_ = d.Queries.UpsertPerson(ctx, db.UpsertPersonParams{ID: person, Name: "Jacob Tremblay", ProviderIds: []byte("{}"), ImageUrl: &photo})
	_ = d.Queries.UpsertPerson(ctx, db.UpsertPersonParams{ID: noPhoto, Name: "Saverio Raimondo", ProviderIds: []byte("{}")})

	d.Images = images.New(t.TempDir())
	rt := jfapi.NewRouter(testutil.Discard(), jfapi.Options{})
	Register(rt, d)
	return imageEnv{h: rt, item: dto.IDFromUUID(item.ID).String(), person: dto.IDFromUUID(person).String(),
		noPhoto: dto.IDFromUUID(noPhoto).String(), posterTag: images.Tag(origin.URL + "/p.jpg"), origin: origin.URL}
}

func get(h http.Handler, method, path string, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequestWithContext(context.Background(), method, path, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestItemImage(t *testing.T) {
	e := newImageEnv(t)
	rec := get(e.h, "GET", "/Items/"+e.item+"/Images/Primary?fillHeight=438&fillWidth=292&quality=96&tag="+e.posterTag, nil)
	h := rec.Header()
	if rec.Code != 200 || h.Get("Content-Type") != "image/jpeg" || h.Get("ETag") != `"`+e.posterTag+`"` ||
		h.Get("Cache-Control") != "public, max-age=31536000, immutable" || h.Get("Last-Modified") == "" {
		t.Fatalf("tagged poster: %d %v", rec.Code, h)
	}
	cfg, err := jpeg.DecodeConfig(bytes.NewReader(rec.Body.Bytes()))
	// fill* covers the box (the client crops), rounded up to 20 px: 300x450.
	if err != nil || cfg.Width < 292 || cfg.Height < 438 || cfg.Width > 320 {
		t.Errorf("rendition %dx%d should cover 292x438: %v", cfg.Width, cfg.Height, err)
	}
	// Without a tag, only "public"; lower-case path and type; HEAD has no body.
	if rec := get(e.h, "GET", "/items/"+e.item+"/images/primary", nil); rec.Code != 200 || rec.Header().Get("Cache-Control") != "public" {
		t.Errorf("untagged: %d %q", rec.Code, rec.Header().Get("Cache-Control"))
	}
	if rec := get(e.h, "HEAD", "/Items/"+e.item+"/Images/Primary?maxWidth=100", nil); rec.Code != 200 || rec.Body.Len() != 0 || rec.Header().Get("Content-Length") == "" {
		t.Errorf("HEAD: %d len=%d %v", rec.Code, rec.Body.Len(), rec.Header())
	}
	// Revalidation answers 304.
	if rec := get(e.h, "GET", "/Items/"+e.item+"/Images/Primary?maxWidth=100", map[string]string{"If-None-Match": `"` + e.posterTag + `"`}); rec.Code != 304 {
		t.Errorf("If-None-Match: %d", rec.Code)
	}
	if rec := get(e.h, "GET", "/Items/"+e.item+"/Images/Backdrop/0?width=300", nil); rec.Code != 200 {
		t.Errorf("indexed backdrop: %d", rec.Code)
	}
}

func TestItemImageErrors(t *testing.T) {
	e := newImageEnv(t)
	msg := func(rec *httptest.ResponseRecorder) string {
		var s string
		_ = json.Unmarshal(rec.Body.Bytes(), &s)
		return s
	}
	cases := []struct {
		path   string
		status int
		body   string
	}{
		{"/Items/" + e.item + "/Images/Thumb", 404, "Luca does not have an image of type Thumb"},
		{"/Items/" + e.item + "/Images/Backdrop/3", 404, "Luca does not have an image of type Backdrop"},
		{"/Items/" + e.item + "/Images/Logo", 404, "Luca does not have an image of type Logo"}, // origin returns 502
		{"/Items/" + e.noPhoto + "/Images/Primary", 404, "Saverio Raimondo does not have an image of type Primary"},
		{"/Items/" + uuid.NewString() + "/Images/Primary", 404, ""},
		{"/Items/not-an-id/Images/Primary", 404, ""},
		{"/Items/" + e.item + "/Images/Wallpaper", 400, ""},
		{"/Items/" + e.item + "/Images/Primary/-1", 400, ""},
	}
	for _, c := range cases {
		rec := get(e.h, "GET", c.path, nil)
		if rec.Code != c.status || (c.body != "" && msg(rec) != c.body) {
			t.Errorf("%s: %d %q", c.path, rec.Code, rec.Body.String())
		}
		if c.body != "" && rec.Header().Get("Content-Type") != "application/json; charset=utf-8" {
			t.Errorf("%s: 404 content type %q (Jellyfin sends a JSON string)", c.path, rec.Header().Get("Content-Type"))
		}
	}
	// People are addressed like items.
	if rec := get(e.h, "GET", "/Items/"+e.person+"/Images/Primary?maxWidth=120", nil); rec.Code != 200 || !strings.HasPrefix(rec.Header().Get("Content-Type"), "image/") {
		t.Errorf("person image: %d %v", rec.Code, rec.Header())
	}
}

func TestPrefetchFillsBlurhash(t *testing.T) {
	q, _ := testutil.Queries(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(jpegBytes(400, 600)) }))
	defer srv.Close()
	ctx := t.Context()
	lib, _ := q.CreateLibrary(ctx, db.CreateLibraryParams{Name: "M", Kind: "movies", Paths: []string{"/m"}})
	item, _ := q.CreateItem(ctx, db.CreateItemParams{LibraryID: lib.ID, Type: "Movie", Name: "X", SortName: "x", SourceKind: "virtual"})
	url := srv.URL + "/p.jpg"
	_ = q.UpsertImage(ctx, db.UpsertImageParams{ItemID: item.ID, Type: "Primary", SourceUrl: &url, Tag: images.Tag(url)})
	p := images.Prefetcher{Store: images.New(t.TempDir()), Q: q, Log: testutil.Discard()}
	if err := p.Run(ctx, lib); err != nil {
		t.Fatal(err)
	}
	img, err := q.GetImage(ctx, db.GetImageParams{ItemID: item.ID, Type: "Primary"})
	if err != nil || img.Blurhash == nil || *img.Width != 400 || *img.Height != 600 {
		t.Fatalf("after prefetch: %+v %v", img, err)
	}
	if rows, _ := q.ImagesMissingInfo(ctx, db.ImagesMissingInfoParams{LibraryID: lib.ID, RowLimit: 10}); len(rows) != 0 {
		t.Errorf("still missing info: %d", len(rows))
	}
}
