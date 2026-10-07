// Package handlers implements the Jellyfin API endpoints (DESIGN §3.5) on
// top of jfapi's router and encoders. Each file covers one API area.
package handlers

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"sync"

	"github.com/google/uuid"

	"github.com/sysadmin/blockbustr/internal/adminui"
	"github.com/sysadmin/blockbustr/internal/auth"
	"github.com/sysadmin/blockbustr/internal/cache"
	"github.com/sysadmin/blockbustr/internal/config"
	"github.com/sysadmin/blockbustr/internal/events"
	"github.com/sysadmin/blockbustr/internal/images"
	"github.com/sysadmin/blockbustr/internal/jfapi"
	"github.com/sysadmin/blockbustr/internal/jfapi/dto"
	"github.com/sysadmin/blockbustr/internal/resolve"
	"github.com/sysadmin/blockbustr/internal/store/pg/db"
	"github.com/sysadmin/blockbustr/internal/stremio"
	"github.com/sysadmin/blockbustr/internal/subtitles"
	"github.com/sysadmin/blockbustr/internal/urlsign"
)

// Deps is what the handlers need from the rest of the server.
type Deps struct {
	Config   config.Config
	ServerID dto.ID
	Auth     *auth.Service
	Queries  *db.Queries
	// DB is the pool behind Queries, for dynamic queries (pg.QueryItems).
	DB  db.DBTX
	Log *slog.Logger
	// Library starts a scan of every library (library.Scanner); nil disables
	// manual refreshes.
	Library interface{ Trigger() }
	// Images serves artwork; nil answers every image request with 404.
	Images *images.Store
	// RemoteSearch adds titles outside the library to search results; nil
	// searches the library only.
	RemoteSearch RemoteSearch
	// Cache holds play sessions (PlaybackInfo decisions).
	Cache *cache.Cache
	// Resolver turns remote sources into fetchable links (nil: remote
	// streams answer 404).
	Resolver *resolve.Resolver
	// Probe probes a .strm item's source at first play (library.Scanner);
	// nil leaves unprobed sources undecided.
	Probe RemoteProber
	// Transcoding runs HLS sessions; nil answers HLS requests with 404.
	Transcoding *Transcoding
	// Subtitles extracts and converts text subtitles; nil answers subtitle
	// requests with 404 and can't burn in text subtitles.
	Subtitles *subtitles.Store
	// Events announces user data changes (and feeds the WebSocket hub); nil
	// drops them.
	Events *events.Bus
	// Hub serves /socket; nil leaves the route out.
	Hub *Hub
	// Addons is the Stremio addon registry behind /blockbustr/addons; nil
	// leaves those routes out.
	Addons *stremio.Registry
	// CatalogSync re-syncs catalog libraries after an addon or catalog
	// changes (stremio.Syncer); nil waits for the next scheduled sync.
	CatalogSync interface{ Trigger() }
	// Streams collects addon streams for catalog titles at PlaybackInfo
	// (stremio.Collector); nil leaves them with a placeholder source.
	Streams StreamCollector
	// StreamSigner signs remote sources' stream URLs; nil leaves them
	// unsigned.
	StreamSigner *urlsign.Signer
	// Debrid manages the debrid accounts behind /blockbustr/debrid; nil
	// leaves those routes out.
	Debrid DebridManager
}

// RemoteProber probes a remote (.strm) item's source and stores it.
type RemoteProber interface {
	ProbeRemote(ctx context.Context, item uuid.UUID) (bool, error)
}

type api struct {
	Deps
	lastTouch sync.Map // device id → last session write (touchSession)
}

// Register mounts every implemented endpoint on rt.
func Register(rt *jfapi.Router, d Deps) {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	a := &api{Deps: d}
	a.registerSystem(rt)
	registerBranding(rt)
	a.registerUsers(rt)
	a.registerUserAdmin(rt)
	a.registerDebrid(rt)
	a.registerOps(rt)
	// The admin web UI (P4.3); it signs in like any client.
	ui := adminui.Handler()
	rt.Get("/blockbustr/ui", func(w http.ResponseWriter, r *http.Request) {
		// Routing trims a trailing slash, so look at what was asked for:
		// "/blockbustr/ui/" is the app, "/blockbustr/ui" redirects to it.
		if p, _, _ := strings.Cut(r.RequestURI, "?"); strings.HasSuffix(p, "/") {
			ui.ServeHTTP(w, r)
			return
		}
		http.Redirect(w, r, adminui.Prefix, http.StatusFound)
	})
	rt.Get("/blockbustr/ui/*", ui.ServeHTTP)
	a.registerQuickConnect(rt)
	a.registerLibrary(rt)
	a.registerImages(rt)
	a.registerViews(rt)
	a.registerItems(rt)
	a.registerItemDetail(rt)
	a.registerShows(rt)
	a.registerNames(rt)
	a.registerMisc(rt)
	a.registerUserData(rt)
	a.registerPlayback(rt)
	a.registerStream(rt)
	a.registerHLS(rt)
	a.registerSubtitles(rt)
	a.registerSessions(rt)
	a.registerSocket(rt)
	a.registerAddons(rt)
}

func ptr[T any](v T) *T { return &v }

// errorText is Jellyfin's body for failed requests such as a bad login:
// "Error processing request." as text/plain.
func errorText(w http.ResponseWriter, status int) {
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(status)
	_, _ = w.Write([]byte("Error processing request."))
}

// internalError logs err and answers 500.
func (a *api) internalError(w http.ResponseWriter, r *http.Request, err error) {
	a.Log.ErrorContext(r.Context(), "request failed", "path", r.URL.Path, "err", err)
	errorText(w, http.StatusInternalServerError)
}
