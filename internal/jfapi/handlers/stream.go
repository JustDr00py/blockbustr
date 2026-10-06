package handlers

import (
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/sysadmin/blockbustr/internal/jfapi"
	"github.com/sysadmin/blockbustr/internal/jfapi/dto"
	"github.com/sysadmin/blockbustr/internal/resolve"
	"github.com/sysadmin/blockbustr/internal/store/pg/db"
)

// Static video streams (TASKS P2.3, P2.4b, DESIGN §8.2): /Videos/{id}/stream
// serves a local file as it is, with Range and HEAD (http.ServeContent), and
// a remote source by redirecting to its resolved link or proxying it. Like
// Jellyfin 12.1.0 it needs no token: Findroid sends none. Remux/transcode go
// through HLS (P2.6).

func (a *api) registerStream(rt *jfapi.Router) {
	for _, p := range []string{"/Videos/{itemId}/stream", "/Videos/{itemId}/stream.{container}"} {
		rt.Get(p, a.videoStream)
		rt.Head(p, a.videoStream)
	}
}

// videoTypes are the Content-Types Jellyfin sends per container (observed
// for mkv, mp4 and mov); anything else falls back to the extension's type.
var videoTypes = map[string]string{
	"mkv": "video/x-matroska", "mp4": "video/mp4", "m4v": "video/x-m4v", "mov": "video/quicktime",
	"ts": "video/mp2t", "m2ts": "video/mp2t", "mpegts": "video/mp2t", "avi": "video/x-msvideo",
	"webm": "video/webm", "wmv": "video/x-ms-wmv", "flv": "video/x-flv", "mpg": "video/mpeg",
	"mpeg": "video/mpeg", "ogv": "video/ogg", "3gp": "video/3gpp",
}

// streamContentType is the type for the container the client named (the
// `container` parameter or the route's extension), else the source's
// probed container, else the file extension's.
func streamContentType(container, stored, path string) string {
	for _, c := range []string{container, stored, strings.TrimPrefix(filepath.Ext(path), ".")} {
		if t := videoTypes[strings.ToLower(c)]; t != "" {
			return t
		}
	}
	if t := mime.TypeByExtension(filepath.Ext(path)); t != "" {
		return t
	}
	return "application/octet-stream"
}

// streamSource picks the requested media source (mediaSourceId; the first
// by default). ok is false when the item has no such source.
func streamSource(it db.Item, sources []db.MediaSource, want string) (db.MediaSource, bool) {
	if len(sources) == 0 {
		if it.Path == nil || it.StrmUrl != nil { // nothing local to serve
			return db.MediaSource{}, false
		}
		return db.MediaSource{ItemID: it.ID, Protocol: "File", PathOrUrl: *it.Path}, want == "" || sameID(want, dto.IDFromUUID(it.ID).String())
	}
	if want == "" {
		return sources[0], true
	}
	for i, src := range sources {
		// The first source answers to the item id and to its own.
		if sameID(want, mediaSourceID(it, i, src)) || sameID(want, dto.IDFromUUID(src.ID).String()) {
			return src, true
		}
	}
	return db.MediaSource{}, false
}

func (a *api) videoStream(w http.ResponseWriter, r *http.Request) {
	id, err := dto.ParseID(jfapi.URLParam(r, "itemId"))
	if err != nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	// Visibility doesn't depend on the user (no token here).
	it, found, err := a.visibleItem(r, uuid.Nil, id.UUID())
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
	q := jfapi.QueryOf(r)
	src, ok := streamSource(it, b.sources[it.ID], q.Get("mediaSourceId"))
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	container := q.Get("container")
	if c := jfapi.URLParam(r, "container"); c != "" {
		container = c
	}
	contentType := streamContentType(container, deref(src.Container), src.PathOrUrl)
	if src.IsRemote || !strings.EqualFold(src.Protocol, "File") {
		a.remoteStream(w, r, src, contentType)
		return
	}
	f, err := os.Open(src.PathOrUrl)
	if err != nil {
		a.Log.WarnContext(r.Context(), "stream file unavailable", "item", it.ID, "err", err)
		w.WriteHeader(http.StatusNotFound)
		return
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil || fi.IsDir() {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", contentType)
	http.ServeContent(w, r, "", fi.ModTime(), f)
}

// remoteStream sends a remote source (DESIGN §8.2): a redirect to the
// resolved link when the client can follow it, else a proxy with Range
// passthrough. A link that stopped working is resolved again once.
func (a *api) remoteStream(w http.ResponseWriter, r *http.Request, src db.MediaSource, contentType string) {
	if a.Resolver == nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	ctx := r.Context()
	target := resolve.FromURL(src.PathOrUrl)
	link, err := a.Resolver.Resolve(ctx, target)
	if err != nil {
		a.Log.WarnContext(ctx, "remote source unavailable", "item", src.ItemID, "err", err)
		sourceUnavailable(w, err, http.StatusBadGateway)
		return
	}
	if a.mayRedirect(r, link.URL) {
		http.Redirect(w, r, link.URL, http.StatusFound)
		return
	}
	resp, err := a.Resolver.Open(ctx, r.Method, link.URL, r.Header)
	if err == nil && linkExpired(resp.StatusCode) {
		_ = resp.Body.Close()
		a.Resolver.Forget(ctx, target)
		if link, err = a.Resolver.Resolve(ctx, target); err == nil {
			resp, err = a.Resolver.Open(ctx, r.Method, link.URL, r.Header)
		}
	}
	if err != nil {
		a.Log.WarnContext(ctx, "remote stream failed", "item", src.ItemID, "err", err)
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	switch {
	case resp.StatusCode == http.StatusRequestedRangeNotSatisfiable:
	case resp.StatusCode >= 400:
		a.Log.WarnContext(ctx, "remote stream refused", "item", src.ItemID, "status", resp.Status)
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	h := w.Header()
	if contentType == "application/octet-stream" { // unprobed .strm: the CDN path usually names the file
		if u, err := url.Parse(link.URL); err == nil {
			contentType = streamContentType("", "", u.Path)
		}
	}
	h.Set("Content-Type", contentType)
	for _, k := range []string{"Content-Length", "Content-Range", "Accept-Ranges", "Last-Modified"} {
		if v := resp.Header.Get(k); v != "" {
			h.Set(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	if r.Method != http.MethodHead {
		_, _ = io.Copy(w, resp.Body) // ends when either side hangs up
	}
}

// sourceUnavailable answers a source that can't be opened: a torrent still
// downloading on debrid is 503 with Retry-After (the account goes on
// downloading it, DESIGN §7.3 step 5); anything else is status.
func sourceUnavailable(w http.ResponseWriter, err error, status int) {
	if errors.Is(err, resolve.ErrDownloading) {
		w.Header().Set("Retry-After", "60")
		errorText(w, http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(status)
}

// linkExpired: statuses a CDN answers for an expired or revoked link.
func linkExpired(status int) bool {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusGone:
		return true
	}
	return false
}

// mayRedirect says whether to send the client to link instead of proxying:
// per-client configuration first (compat.proxy_clients/redirect_clients,
// matched on the auth Client field), else only when the scheme stays the
// same. ExoPlayer refuses http → https redirects (DESIGN §8.2).
func (a *api) mayRedirect(r *http.Request, link string) bool {
	client := jfapi.AuthFrom(r.Context()).Client
	has := func(list []string) bool {
		return client != "" && slices.ContainsFunc(list, func(c string) bool { return strings.EqualFold(c, client) })
	}
	switch {
	case has(a.Config.Compat.ProxyClients):
		return false
	case has(a.Config.Compat.RedirectClients):
		return true
	}
	u, err := url.Parse(link)
	if err != nil {
		return false
	}
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	return strings.EqualFold(u.Scheme, scheme)
}
