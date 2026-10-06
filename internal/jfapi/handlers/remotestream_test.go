package handlers

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sysadmin/blockbustr/internal/jfapi"
	"github.com/sysadmin/blockbustr/internal/resolve"
	"github.com/sysadmin/blockbustr/internal/testutil"
)

// remoteFixture is a .strm target like jellybird's: an http origin that
// redirects to an https CDN serving the bytes.
type remoteFixture struct {
	origin, cdn  *httptest.Server
	originHits   atomic.Int32
	cdnGone      atomic.Int32 // answer 410 this many more times
	originBroken atomic.Bool
	h            http.Handler
	resolver     *resolve.Resolver
	content      []byte
	modified     time.Time
}

func newRemoteFixture(t *testing.T, d Deps, configure func(*Deps)) *remoteFixture {
	t.Helper()
	f := &remoteFixture{content: []byte("0123456789"), modified: time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)}
	f.cdn = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if f.cdnGone.Load() > 0 {
			f.cdnGone.Add(-1)
			w.WriteHeader(http.StatusGone)
			return
		}
		http.ServeContent(w, r, "", f.modified, bytes.NewReader(f.content))
	}))
	f.origin = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.originHits.Add(1)
		if f.originBroken.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		http.Redirect(w, r, f.cdn.URL+"/file/Luca.mkv?token=cdn", http.StatusFound)
	}))
	t.Cleanup(f.origin.Close)
	t.Cleanup(f.cdn.Close)
	client := f.cdn.Client() // trusts the test CDN's certificate, follows redirects
	f.resolver = &resolve.Resolver{Cache: d.Cache, HTTP: client, Stream: client}
	d.Resolver = f.resolver
	if configure != nil {
		configure(&d)
	}
	rt := jfapi.NewRouter(testutil.Discard(), jfapi.Options{LegacyAuth: true})
	Register(rt, d)
	f.h = rt
	execSQL(t, `UPDATE media_sources SET path_or_url = $1, is_remote = true, protocol = 'Http' WHERE item_id = $2`, f.origin.URL+"/stream/realdebrid/X/1?sig=s", fockers)
	return f
}

func (f *remoteFixture) get(t *testing.T, method string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(context.Background(), method, "/Videos/"+fockers+"/stream?static=true", nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	return rec
}

func TestRemoteStreamProxyAndRedirect(t *testing.T) {
	_, d := newIntegrationServer(t)
	seedRecon(t, d)
	f := newRemoteFixture(t, d, nil)
	execSQL(t, `UPDATE media_sources SET container = NULL WHERE item_id = $1`, fockers) // unprobed: type from the CDN URL

	// Reached over http, the https CDN is proxied (ExoPlayer won't follow
	// http → https), with Range passed through.
	rec := f.get(t, "GET", map[string]string{"Range": "bytes=2-5"})
	if body, _ := io.ReadAll(rec.Body); rec.Code != 206 || string(body) != "2345" || rec.Header().Get("Content-Range") != "bytes 2-5/10" ||
		rec.Header().Get("Content-Type") != "video/x-matroska" || rec.Header().Get("Accept-Ranges") != "bytes" {
		t.Errorf("proxied range: %d %q %v", rec.Code, body, rec.Header())
	}
	if rec := f.get(t, "HEAD", nil); rec.Code != 200 || rec.Body.Len() != 0 || rec.Header().Get("Content-Length") != "10" {
		t.Errorf("proxied HEAD: %d %v", rec.Code, rec.Header())
	}
	if rec := f.get(t, "GET", nil); rec.Code != 200 || rec.Body.String() != "0123456789" {
		t.Errorf("proxied whole file: %d %q", rec.Code, rec.Body)
	}
	if n := f.originHits.Load(); n != 1 {
		t.Errorf("the link is resolved once and cached: %d origin hits", n)
	}

	// Reached over https (behind a proxy), the client is sent to the CDN.
	rec = f.get(t, "GET", map[string]string{"X-Forwarded-Proto": "https"})
	if rec.Code != 302 || rec.Header().Get("Location") != f.cdn.URL+"/file/Luca.mkv?token=cdn" {
		t.Errorf("redirect: %d %v", rec.Code, rec.Header())
	}

	// An expired link is resolved again, once.
	f.cdnGone.Store(1)
	if rec := f.get(t, "GET", map[string]string{"Range": "bytes=0-0"}); rec.Code != 206 || rec.Body.String() != "0" || f.originHits.Load() != 2 {
		t.Errorf("expired link: %d %q, %d origin hits", rec.Code, rec.Body, f.originHits.Load())
	}
	f.cdnGone.Store(5)
	if rec := f.get(t, "GET", nil); rec.Code != 502 {
		t.Errorf("CDN keeps refusing = %d", rec.Code)
	}
	f.cdnGone.Store(0)

	// The origin itself failing.
	f.resolver.Forget(t.Context(), resolve.FromURL(f.origin.URL+"/stream/realdebrid/X/1?sig=s"))
	f.originBroken.Store(true)
	if rec := f.get(t, "GET", nil); rec.Code != 502 {
		t.Errorf("unresolvable = %d", rec.Code)
	}
}

func TestRemoteStreamClientOverrides(t *testing.T) {
	_, d := newIntegrationServer(t)
	seedRecon(t, d)
	f := newRemoteFixture(t, d, func(d *Deps) {
		d.Config.Compat.RedirectClients = []string{"Infuse-Direct"}
		d.Config.Compat.ProxyClients = []string{"Findroid"}
	})
	if rec := f.get(t, "GET", map[string]string{"Authorization": `MediaBrowser Client="Infuse-Direct", Device="tv", DeviceId="1", Version="8"`}); rec.Code != 302 {
		t.Errorf("redirect_clients: %d", rec.Code)
	}
	if rec := f.get(t, "GET", map[string]string{"X-Forwarded-Proto": "https", "Authorization": `MediaBrowser Client="Findroid", Device="p", DeviceId="2", Version="1"`}); rec.Code != 200 {
		t.Errorf("proxy_clients: %d", rec.Code)
	}
}

// fakeProber records first-play probes and stores a probed state.
type fakeProber struct {
	t     *testing.T
	calls atomic.Int32
}

func (p *fakeProber) ProbeRemote(_ context.Context, item uuid.UUID) (bool, error) {
	p.calls.Add(1)
	execSQL(p.t, `UPDATE media_sources SET probed_at = now() WHERE item_id = $1`, item)
	return true, nil
}

func TestPlaybackInfoProbesRemoteOnce(t *testing.T) {
	_, d := newIntegrationServer(t)
	seedRecon(t, d)
	seedCaptureToken(t, d)
	p := &fakeProber{t: t}
	d.Probe = p
	rt := jfapi.NewRouter(testutil.Discard(), jfapi.Options{LegacyAuth: true})
	Register(rt, d)
	execSQL(t, `DELETE FROM media_streams WHERE media_source_id IN (SELECT id FROM media_sources WHERE item_id = $1)`, fockers)
	execSQL(t, `UPDATE media_sources SET probed_at = NULL, path_or_url = 'http://jellybird:8097/stream/realdebrid/SECRET/1?sig=TOKEN' WHERE item_id = $1`, fockers)

	for range 2 {
		if rec := call(t, rt, "POST", "/Items/"+fockers+"/PlaybackInfo", `MediaBrowser Token="`+captureToken+`"`, `{"DeviceProfile":{}}`); rec.Code != 200 {
			t.Fatalf("PlaybackInfo = %d %s", rec.Code, rec.Body)
		}
	}
	if p.calls.Load() != 1 {
		t.Errorf("probed %d times, want once", p.calls.Load())
	}

	// No raw .strm URL reaches clients: Path is this server's stream URL.
	var item struct {
		MediaSources []struct{ Path, Protocol string }
	}
	getJSON(t, rt, "/Items/"+fockers, &item)
	if len(item.MediaSources) != 1 || item.MediaSources[0].Path != "http://example.com/Videos/"+fockers+"/stream?static=true&MediaSourceId="+fockers ||
		item.MediaSources[0].Protocol != "Http" || strings.Contains(item.MediaSources[0].Path, "SECRET") {
		t.Errorf("details: %+v", item.MediaSources)
	}
}
