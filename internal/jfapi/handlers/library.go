package handlers

import (
	"net/http"

	"github.com/sysadmin/blockbustr/internal/auth"
	"github.com/sysadmin/blockbustr/internal/jfapi"
)

// POST /Library/Refresh ("Scan All Libraries" in Jellyfin's dashboard) is
// admin-only and returns 204 at once; the scan runs in the background.
func (a *api) registerLibrary(rt *jfapi.Router) {
	rt.Post("/Library/Refresh", a.requireAdmin(func(w http.ResponseWriter, r *http.Request, s auth.Session) {
		if a.Library != nil {
			a.Library.Trigger()
			a.Log.InfoContext(r.Context(), "library refresh requested", "user", s.UserName)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
}
