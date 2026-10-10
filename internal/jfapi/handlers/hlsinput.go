package handlers

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"net"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/sysadmin/blockbustr/internal/metrics"
)

// HLS input through blockbustr. ffmpeg, which remuxes or transcodes a remote
// source into HLS, used to open the debrid link itself, so the download
// never passed through the proxy: it wasn't counted, and the Playback page
// had nothing to show for the play. Now ffmpeg reads a loopback URL of this
// server instead, which fetches the real link and passes it on through the
// same counting writer as a proxied stream. It also re-resolves a link that
// expired mid-play, which ffmpeg's own reconnects could not.
//
// The loopback listener is separate from the public one and bound to
// 127.0.0.1; the URL carries a random token, valid for the process's life.

const hlsInputKeep = 24 * time.Hour

type hlsInput struct {
	once sync.Once
	base string // http://127.0.0.1:<port>
	err  error

	mu        sync.Mutex
	byToken   map[string]*hlsInputEntry
	bySession map[string]*hlsInputEntry
}

type hlsInputEntry struct {
	playSession string
	refresh     func(context.Context) (string, error) // a fresh link for the source
	created     time.Time

	mu  sync.Mutex
	url string // the link now in use
}

func (e *hlsInputEntry) link() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.url
}

func (e *hlsInputEntry) setLink(u string) {
	e.mu.Lock()
	e.url = u
	e.mu.Unlock()
}

// hlsInputURL is the loopback URL ffmpeg reads upstream (the resolved link
// of the play session's source) through. refresh gets a new link when the
// old one stops working.
func (a *api) hlsInputURL(playSession, upstream string, refresh func(context.Context) (string, error)) (string, error) {
	h := &a.hlsIn
	h.once.Do(func() { h.err = a.hlsInputListen() })
	if h.err != nil {
		return "", h.err
	}
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	tok := hex.EncodeToString(b[:])
	e := &hlsInputEntry{playSession: playSession, refresh: refresh, created: time.Now(), url: upstream}
	h.mu.Lock()
	if h.byToken == nil {
		h.byToken, h.bySession = map[string]*hlsInputEntry{}, map[string]*hlsInputEntry{}
	}
	for t, old := range h.byToken {
		if time.Since(old.created) > hlsInputKeep {
			delete(h.byToken, t)
			if h.bySession[old.playSession] == old {
				delete(h.bySession, old.playSession)
			}
		}
	}
	h.byToken[tok], h.bySession[playSession] = e, e
	h.mu.Unlock()
	// The upstream's extension helps ffmpeg tell the container.
	ext := ""
	if u, err := url.Parse(upstream); err == nil {
		if x := path.Ext(u.Path); len(x) <= 5 {
			ext = x
		}
	}
	return h.base + "/in/" + tok + "/media" + ext, nil
}

// hlsUpstream is the link a play session's ffmpeg reads through the
// loopback, or "" when it reads a file or the link directly.
func (a *api) hlsUpstream(playSession string) string {
	h := &a.hlsIn
	h.mu.Lock()
	defer h.mu.Unlock()
	if e := h.bySession[playSession]; e != nil {
		return e.link()
	}
	return ""
}

func (a *api) hlsInputListen() error {
	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	a.hlsIn.base = "http://" + ln.Addr().String()
	srv := &http.Server{Handler: http.HandlerFunc(a.serveHLSInput), ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	return nil
}

func (a *api) serveHLSInput(w http.ResponseWriter, r *http.Request) {
	rest, ok := strings.CutPrefix(r.URL.Path, "/in/")
	tok, _, _ := strings.Cut(rest, "/")
	a.hlsIn.mu.Lock()
	e := a.hlsIn.byToken[tok]
	a.hlsIn.mu.Unlock()
	if !ok || e == nil || (r.Method != http.MethodGet && r.Method != http.MethodHead) {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	ctx := r.Context()
	resp, err := a.Resolver.Open(ctx, r.Method, e.link(), r.Header)
	if err == nil && linkExpired(resp.StatusCode) && e.refresh != nil {
		if fresh, rerr := e.refresh(ctx); rerr == nil {
			_ = resp.Body.Close()
			e.setLink(fresh)
			resp, err = a.Resolver.Open(ctx, r.Method, fresh, r.Header)
		}
	}
	if err != nil {
		a.Log.WarnContext(ctx, "transcode input failed", "playSession", e.playSession, "err", err)
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 && resp.StatusCode != http.StatusRequestedRangeNotSatisfiable {
		a.Log.WarnContext(ctx, "transcode input refused", "playSession", e.playSession, "status", resp.Status)
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	h := w.Header()
	for _, k := range []string{"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges", "Last-Modified"} {
		if v := resp.Header.Get(k); v != "" {
			h.Set(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	if r.Method == http.MethodHead {
		return
	}
	ps, _ := a.loadPlaySession(ctx, e.playSession)
	cw := &countingWriter{w: w, a: a, ctx: ctx, sl: streamLog{playSession: e.playSession, owner: ps.UserID}, last: time.Now()}
	metrics.ActiveProxies.Inc()
	_, _ = io.Copy(cw, resp.Body) // ends when ffmpeg hangs up
	metrics.ActiveProxies.Dec()
	cw.flush()
}
