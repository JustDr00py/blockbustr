package handlers

import (
	"fmt"
	"mime"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/google/uuid"

	"github.com/sysadmin/blockbustr/internal/auth"
	"github.com/sysadmin/blockbustr/internal/jfapi"
	"github.com/sysadmin/blockbustr/internal/jfapi/dto"
	"github.com/sysadmin/blockbustr/internal/store/pg/db"
)

// Downloads: /Items/{id}/Download sends a movie or episode as it is, for
// clients' offline copies. Unlike /Videos/{id}/stream it needs a token,
// and the user's policy must allow playback and EnableContentDownloading
// (on by default, as in Jellyfin). The bytes come the stream's way: a
// local file served from disk, a remote one redirected to or proxied
// (a debrid link is always proxied, DESIGN §8.2).

func (a *api) registerDownload(rt *jfapi.Router) {
	rt.Get("/Items/{itemId}/Download", a.requireUser(a.download))
	rt.Head("/Items/{itemId}/Download", a.requireUser(a.download))
}

func (a *api) download(w http.ResponseWriter, r *http.Request, s auth.Session) {
	if !mayDownload(r.Context()) {
		errorText(w, http.StatusForbidden)
		return
	}
	if over, err := a.overQuota(r.Context(), s); err != nil {
		a.internalError(w, r, err)
		return
	} else if over {
		errorText(w, http.StatusForbidden)
		return
	}
	id, err := dto.ParseID(jfapi.URLParam(r, "itemId"))
	if err != nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	it, found, err := a.visibleItem(r, s.UserID, id.UUID())
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	if !found || (it.Type != "Movie" && it.Type != "Episode") {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	b := &itemBatch{sources: map[uuid.UUID][]db.MediaSource{}, streams: map[uuid.UUID][]db.MediaStream{}}
	if err := a.loadPlaySources(r.Context(), b, it); err != nil {
		a.internalError(w, r, err)
		return
	}
	want := jfapi.QueryOf(r).Get("mediaSourceId")
	src, ok := streamSource(it, b.sources[it.ID], want)
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	a.Log.InfoContext(r.Context(), "download", "user", s.UserName, "item", it.ID, "source", src.ID)
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": downloadName(it, src)}))
	a.serveSource(w, r, it, src, b.sources[it.ID], want, "")
}

// downloadName is the file name offered for src: a local file's own name,
// else the item's ("S01E02 - Name" for an episode) with the source's
// container as extension.
func downloadName(it db.Item, src db.MediaSource) string {
	if !src.IsRemote && strings.EqualFold(src.Protocol, "File") {
		return filepath.Base(src.PathOrUrl)
	}
	name := it.Name
	switch {
	case it.Type == "Episode" && it.ParentIndexNumber != nil && it.IndexNumber != nil:
		name = fmt.Sprintf("S%02dE%02d - %s", *it.ParentIndexNumber, *it.IndexNumber, name)
	case it.Type == "Movie" && it.ProductionYear != nil:
		name = fmt.Sprintf("%s (%d)", name, *it.ProductionYear)
	}
	name = strings.Map(func(r rune) rune {
		if strings.ContainsRune(`/\:*?"<>|`, r) || r < 0x20 {
			return '_'
		}
		return r
	}, name)
	ext := strings.ToLower(deref(src.Container))
	if ext == "" || strings.Contains(ext, ",") { // probed as "mov,mp4,m4a,…" or not at all
		ext = "mkv"
	}
	return name + "." + ext
}
