package handlers

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sysadmin/blockbustr/internal/images"
	"github.com/sysadmin/blockbustr/internal/jfapi"
	"github.com/sysadmin/blockbustr/internal/jfapi/dto"
	"github.com/sysadmin/blockbustr/internal/store/pg/db"
)

// Jellyfin's ImageType names, by lower-case spelling (clients send both).
var imageTypes = map[string]string{}

func init() {
	for _, t := range []string{"Primary", "Art", "Backdrop", "Banner", "Logo", "Thumb", "Disc", "Box",
		"Screenshot", "Menu", "Chapter", "BoxRear", "Profile"} {
		imageTypes[strings.ToLower(t)] = t
	}
}

// Images are public in Jellyfin (no token), so clients can load them in
// plain <img>/Coil/Glide requests.
func (a *api) registerImages(rt *jfapi.Router) {
	for _, p := range []string{"/Items/{itemId}/Images/{imageType}", "/Items/{itemId}/Images/{imageType}/{imageIndex}"} {
		rt.Get(p, a.itemImage)
		rt.Head(p, a.itemImage)
	}
}

func (a *api) itemImage(w http.ResponseWriter, r *http.Request) {
	id, err := dto.ParseID(jfapi.URLParam(r, "itemId"))
	if err != nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	typ, ok := imageTypes[strings.ToLower(jfapi.URLParam(r, "imageType"))]
	if !ok {
		errorText(w, http.StatusBadRequest)
		return
	}
	idx := 0
	if s := jfapi.URLParam(r, "imageIndex"); s != "" {
		if idx, err = strconv.Atoi(s); err != nil || idx < 0 {
			errorText(w, http.StatusBadRequest)
			return
		}
	}

	src, owner, err := a.imageSource(r, id, typ, idx)
	switch {
	case err != nil:
		a.internalError(w, r, err)
		return
	case owner == "":
		w.WriteHeader(http.StatusNotFound) // no such item or person
		return
	case src.Tag == "" || a.Images == nil:
		imageNotFound(w, r, owner, typ)
		return
	}

	q := jfapi.QueryOf(r)
	num := func(name string) int { n, _ := q.Int(name); return n }
	rend, err := a.Images.Get(r.Context(), src, images.Request{
		Width: num("width"), Height: num("height"), MaxWidth: num("maxWidth"), MaxHeight: num("maxHeight"),
		FillWidth: num("fillWidth"), FillHeight: num("fillHeight"), Quality: num("quality"), Format: q.Get("format"),
	})
	if err != nil {
		if r.Context().Err() == nil {
			a.Log.WarnContext(r.Context(), "image unavailable", "item", id.String(), "type", typ, "err", err)
		}
		imageNotFound(w, r, owner, typ)
		return
	}
	f, err := os.Open(rend.Path)
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	defer func() { _ = f.Close() }()

	// Jellyfin 12.1.0's headers: a tagged URL never changes, so it is
	// cacheable forever; untagged URLs are only "public".
	h := w.Header()
	h.Set("Content-Type", rend.ContentType)
	h.Set("ETag", `"`+src.Tag+`"`)
	if q.Get("tag") != "" {
		h.Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		h.Set("Cache-Control", "public")
	}
	http.ServeContent(w, r, "", rend.ModTime, f) // HEAD, Range, If-None-Match/If-Modified-Since
}

// imageSource finds the image for an item, or for a person (Jellyfin
// addresses people as items: /Items/{personId}/Images/Primary). owner is
// the item/person name, "" if the id is unknown.
func (a *api) imageSource(r *http.Request, id dto.ID, typ string, idx int) (images.Source, string, error) {
	ctx := r.Context()
	img, err := a.Queries.GetImage(ctx, db.GetImageParams{ItemID: id.UUID(), Type: typ, Idx: int32(idx)})
	if err == nil {
		item, err := a.Queries.GetItem(ctx, id.UUID())
		if err != nil {
			return images.Source{}, "", err
		}
		return images.Source{URL: deref(img.SourceUrl), LocalPath: deref(img.LocalPath), Tag: img.Tag}, item.Name, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return images.Source{}, "", err
	}
	if item, err := a.Queries.GetItem(ctx, id.UUID()); err == nil {
		return images.Source{}, item.Name, nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return images.Source{}, "", err
	}
	p, err := a.Queries.GetPerson(ctx, id.UUID())
	if errors.Is(err, pgx.ErrNoRows) {
		return images.Source{}, "", nil
	}
	if err != nil {
		return images.Source{}, "", err
	}
	if typ != "Primary" || idx != 0 || p.ImageUrl == nil {
		return images.Source{}, p.Name, nil
	}
	return images.Source{URL: *p.ImageUrl, Tag: images.Tag(*p.ImageUrl)}, p.Name, nil
}

// imageNotFound is Jellyfin's 404: a JSON string naming the item.
func imageNotFound(w http.ResponseWriter, r *http.Request, owner, typ string) {
	jfapi.WriteJSON(w, r, http.StatusNotFound, fmt.Sprintf("%s does not have an image of type %s", owner, typ))
}
