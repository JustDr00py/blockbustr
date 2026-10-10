package handlers

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/sysadmin/blockbustr/internal/cache"
	"github.com/sysadmin/blockbustr/internal/jfapi"
	"github.com/sysadmin/blockbustr/internal/jfapi/dto"
	"github.com/sysadmin/blockbustr/internal/metrics"
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
	q := jfapi.QueryOf(r)
	if !a.signatureOK(r, q, it) {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	// No token here: the user's height cap comes with their play session.
	ps, _ := a.loadPlaySession(r.Context(), q.Get("PlaySessionId"))
	r = r.WithContext(withMaxHeight(r.Context(), ps.MaxHeight))
	b := &itemBatch{sources: map[uuid.UUID][]db.MediaSource{}, streams: map[uuid.UUID][]db.MediaStream{}}
	if err := a.loadPlaySources(r.Context(), b, it); err != nil {
		a.internalError(w, r, err)
		return
	}
	b.sources[it.ID] = a.inPlayOrder(r.Context(), it, q.Get("PlaySessionId"), b.sources[it.ID])
	src, ok := streamSource(it, b.sources[it.ID], q.Get("mediaSourceId"))
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	container := q.Get("container")
	if c := jfapi.URLParam(r, "container"); c != "" {
		container = c
	}
	a.serveSource(w, r, it, src, b.sources[it.ID], q.Get("mediaSourceId"), container)
}

// serveSource sends src as it is: a local file with Range and HEAD, a
// remote one through remoteStream. container names the client's expected
// container ("" for the source's own).
func (a *api) serveSource(w http.ResponseWriter, r *http.Request, it db.Item, src db.MediaSource, sources []db.MediaSource, want, container string) {
	contentType := streamContentType(container, deref(src.Container), src.PathOrUrl)
	if src.IsRemote || !strings.EqualFold(src.Protocol, "File") {
		a.remoteStream(w, r, it, src, sources, want, contentType)
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
	if r.Method != http.MethodHead {
		a.logStream(r, it, src, deliveryLocal, "")
	}
	w.Header().Set("Content-Type", contentType)
	http.ServeContent(w, r, "", fi.ModTime(), f)
}

// remoteStream sends a remote source (DESIGN §8.2): a redirect to the
// resolved link when the client can follow it, else a proxy with Range
// passthrough. A link that stopped working is resolved again once. A
// catalog title's addon links are proxied: they may carry the addon's
// credentials (a debrid key in a resolve URL), and a version that fails
// falls back to the next (playLink). The exception is a link on one of
// stremio.redirect_hosts, a stream proxy the admin runs for apps to fetch
// from themselves.
func (a *api) remoteStream(w http.ResponseWriter, r *http.Request, it db.Item, src db.MediaSource, sources []db.MediaSource, want, contentType string) {
	if a.Resolver == nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	ctx := r.Context()
	src, target, link, err := a.playLink(ctx, it, src, sources, want)
	if err != nil {
		a.Log.WarnContext(ctx, "remote source unavailable", "item", src.ItemID, "err", err)
		sourceUnavailable(w, err, http.StatusBadGateway)
		return
	}
	_, _, catalog := stremioRef(it)
	if !resolve.IsPrivate(link) && (!catalog || a.redirectHost(link.URL)) && a.mayRedirect(r, link.URL) {
		if r.Method != http.MethodHead {
			a.logStream(r, it, src, deliveryRedirected, link.URL)
		}
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
		cw := &countingWriter{w: w, a: a, ctx: ctx, sl: a.logStream(r, it, src, deliveryProxied, link.URL), last: time.Now()}
		metrics.ActiveProxies.Inc()
		_, _ = io.Copy(cw, resp.Body) // ends when either side hangs up
		metrics.ActiveProxies.Dec()
		cw.flush()
	}
}

// playLink resolves src. For a catalog title asked for by its default
// source (no id, or the item id: the player didn't pick a version), a
// version that fails falls back to the next ranked one, skipping those
// that failed recently; a version that fails for good (blocked by the
// debrid service, refused by every account) is remembered as bad and no
// longer offered (TASKS P3.11). Still downloading isn't failing for good,
// but the default moves on to a version that plays now.
func (a *api) playLink(ctx context.Context, it db.Item, src db.MediaSource, sources []db.MediaSource, want string) (db.MediaSource, resolve.Source, resolve.Link, error) {
	try := []db.MediaSource{src}
	_, _, catalog := stremioRef(it)
	if catalog && (want == "" || sameID(want, dto.IDFromUUID(it.ID).String())) {
		bad := a.badChoices(ctx, it.ID)
		try = try[:0]
		for _, s := range sources {
			if !bad[s.ID] {
				try = append(try, s)
			}
		}
		if len(try) == 0 {
			try = []db.MediaSource{src}
		}
	}
	var firstErr error
	downloading := false
	for _, s := range try {
		target := resolve.FromURL(s.PathOrUrl)
		link, err := a.Resolver.Resolve(ctx, target)
		if err == nil {
			if catalog && s.IsRemote {
				// It plays now (a torrent too, once added): probe its tracks
				// for the next PlaybackInfo, in the background.
				c := StreamChoice{ID: s.ID, Target: s.PathOrUrl, Ready: true}
				if _, probed := a.loadChoiceProbes(ctx, []StreamChoice{c})[c.ID]; !probed && a.ChoiceProber != nil {
					go a.probeChoice(context.WithoutCancel(ctx), c)
				}
			}
			return s, target, link, nil
		}
		if ctx.Err() != nil {
			return s, target, link, err
		}
		if errors.Is(err, resolve.ErrDownloading) {
			downloading = true
		} else if catalog {
			a.markBadChoice(ctx, it.ID, s.ID)
		}
		if firstErr == nil {
			firstErr = err
		}
		if len(try) > 1 {
			a.Log.WarnContext(ctx, "stream version unavailable; trying the next", "item", it.ID, "source", s.ID, "err", err)
		}
	}
	if downloading {
		firstErr = fmt.Errorf("%w (and no other version plays now)", resolve.ErrDownloading)
	}
	return src, resolve.FromURL(src.PathOrUrl), resolve.Link{}, firstErr
}

func (a *api) badChoices(ctx context.Context, item uuid.UUID) map[uuid.UUID]bool {
	out := map[uuid.UUID]bool{}
	if a.Cache == nil {
		return out
	}
	ids, err := a.Cache.SetMembers(ctx, cache.StreamBadKey(item.String()))
	if err != nil {
		return out
	}
	for _, s := range ids {
		if id, err := uuid.Parse(s); err == nil {
			out[id] = true
		}
	}
	return out
}

func (a *api) markBadChoice(ctx context.Context, item, choice uuid.UUID) {
	if a.Cache != nil {
		_ = a.Cache.AddToSet(ctx, cache.StreamBadKey(item.String()), cache.StreamBadTTL, choice.String())
	}
}

// signatureOK checks a signed stream URL (a remote source's Path, P3.10).
// A request without a signature is let through, as Jellyfin lets
// anonymous stream requests through (Findroid builds its own URL with no
// credentials); one with a bad or expired signature is refused.
func (a *api) signatureOK(r *http.Request, q jfapi.Query, it db.Item) bool {
	sig := q.Get("Signature")
	if sig == "" || a.StreamSigner == nil {
		return true
	}
	ms := q.Get("mediaSourceId")
	if id, err := dto.ParseID(ms); err == nil {
		ms = id.String()
	}
	if err := a.StreamSigner.Verify(dto.IDFromUUID(it.ID).String(), ms, q.Get("Expires"), sig, time.Now()); err != nil {
		a.Log.InfoContext(r.Context(), "stream signature refused", "item", it.ID, "err", err)
		return false
	}
	return true
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

// redirectHost says whether link is on one of stremio.redirect_hosts (or a
// subdomain of one).
func (a *api) redirectHost(link string) bool {
	u, err := url.Parse(link)
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	return host != "" && slices.ContainsFunc(a.live().Stremio.RedirectHosts, func(h string) bool {
		h = strings.ToLower(h)
		return host == h || strings.HasSuffix(host, "."+h)
	})
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

// loadPlaySession is the play session PlaybackInfo stored under id; ok is
// false when there's none (no id, expired).
func (a *api) loadPlaySession(ctx context.Context, id string) (ps PlaySession, ok bool) {
	if id == "" || a.Cache == nil {
		return ps, false
	}
	ok, err := a.Cache.GetJSON(ctx, cache.PlaySessionKey(id), &ps)
	return ps, err == nil && ok
}

// inPlayOrder puts an addon title's sources in the order PlaybackInfo
// offered them to playSession (playable first), so the item id, which
// clients send for the default, names the same version here as there, and
// the fallback tries them in that order. Without the session (or for other
// items) they stay in rank order.
func (a *api) inPlayOrder(ctx context.Context, it db.Item, playSession string, sources []db.MediaSource) []db.MediaSource {
	if _, _, ok := stremioRef(it); !ok {
		return sources
	}
	ps, ok := a.loadPlaySession(ctx, playSession)
	if !ok || ps.ItemID != it.ID || len(ps.Order) == 0 {
		return sources
	}
	pos := make(map[uuid.UUID]int, len(ps.Order))
	for i, id := range ps.Order {
		pos[id] = i
	}
	out := slices.Clone(sources)
	slices.SortStableFunc(out, func(a, b db.MediaSource) int {
		pa, oka := pos[a.ID]
		pb, okb := pos[b.ID]
		switch {
		case oka && okb:
			return cmp.Compare(pa, pb)
		case oka:
			return -1
		case okb:
			return 1
		}
		return 0
	})
	return out
}
