// Package handlers implements the Jellyfin API endpoints (DESIGN §3.5) on
// top of jfapi's router and encoders. Each file covers one API area.
package handlers

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/google/uuid"

	"github.com/sysadmin/blockbustr/internal/auth"
	"github.com/sysadmin/blockbustr/internal/cache"
	"github.com/sysadmin/blockbustr/internal/config"
	"github.com/sysadmin/blockbustr/internal/images"
	"github.com/sysadmin/blockbustr/internal/jfapi"
	"github.com/sysadmin/blockbustr/internal/jfapi/dto"
	"github.com/sysadmin/blockbustr/internal/resolve"
	"github.com/sysadmin/blockbustr/internal/store/pg/db"
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
}

// RemoteProber probes a remote (.strm) item's source and stores it.
type RemoteProber interface {
	ProbeRemote(ctx context.Context, item uuid.UUID) (bool, error)
}

type api struct{ Deps }

// Register mounts every implemented endpoint on rt.
func Register(rt *jfapi.Router, d Deps) {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	a := &api{d}
	a.registerSystem(rt)
	registerBranding(rt)
	a.registerUsers(rt)
	registerQuickConnect(rt)
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
