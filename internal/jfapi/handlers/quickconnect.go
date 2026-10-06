package handlers

import (
	"net/http"

	"github.com/sysadmin/blockbustr/internal/jfapi"
)

// QuickConnect isn't implemented yet (P2.12). Reporting it disabled makes
// clients hide the option instead of offering a flow that would fail.
func registerQuickConnect(rt *jfapi.Router) {
	rt.Get("/QuickConnect/Enabled", func(w http.ResponseWriter, r *http.Request) {
		jfapi.WriteJSON(w, r, http.StatusOK, false)
	})
}
