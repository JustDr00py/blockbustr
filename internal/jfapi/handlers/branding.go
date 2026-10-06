package handlers

import (
	"net/http"

	"github.com/sysadmin/blockbustr/internal/jfapi"
	"github.com/sysadmin/blockbustr/internal/jfapi/dto"
)

// Branding endpoints are public. Values are Jellyfin's defaults until an
// admin UI exists (P4.3): no login disclaimer, no custom CSS, no splash
// screen. Null fields are omitted, as in 12.1.0 ({"SplashscreenEnabled":false}).
func registerBranding(rt *jfapi.Router) {
	rt.Get("/Branding/Configuration", func(w http.ResponseWriter, r *http.Request) {
		jfapi.WriteJSON(w, r, http.StatusOK, dto.BrandingOptionsDto{SplashscreenEnabled: ptr(false)})
	})
	css := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
		w.WriteHeader(http.StatusOK)
	}
	rt.Get("/Branding/Css", css)
	rt.Get("/Branding/Css.css", css)
}
