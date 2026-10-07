// Package adminui serves the admin web UI (TASKS P4.3): a React app in
// web/admin, built into dist/ and embedded in the binary. Without a build
// (a plain "go build"), a page explains how to make one.
package adminui

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// Prefix is where the UI is served.
const Prefix = "/blockbustr/ui/"

//go:embed all:dist placeholder.html
var files embed.FS

// Handler serves the UI under Prefix: built files as they are (hashed
// assets cached for a year), and index.html for any other path, so the
// app's own routes load.
func Handler() http.Handler {
	dist, _ := fs.Sub(files, "dist")
	index, err := fs.ReadFile(dist, "index.html")
	if err != nil {
		index, _ = files.ReadFile("placeholder.html")
	}
	fileServer := http.FileServerFS(dist)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(path.Clean("/"+strings.TrimPrefix(r.URL.Path, strings.TrimSuffix(Prefix, "/"))), "/")
		if name != "" && name != "index.html" && name != ".gitkeep" {
			if f, err := dist.Open(name); err == nil {
				_ = f.Close()
				if strings.HasPrefix(name, "assets/") {
					w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
				}
				r2 := r.Clone(r.Context())
				r2.URL.Path = "/" + name
				fileServer.ServeHTTP(w, r2)
				return
			}
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write(index)
	})
}
