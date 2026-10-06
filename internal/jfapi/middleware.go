package jfapi

import (
	"errors"
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/go-chi/chi/v5/middleware"
)

// recoverer turns a handler panic into a logged 500 instead of a dropped
// connection.
func recoverer(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			rec := recover()
			if rec == nil {
				return
			}
			if err, ok := rec.(error); ok && errors.Is(err, http.ErrAbortHandler) {
				panic(rec) // let net/http abort the response as intended
			}
			log.ErrorContext(r.Context(), "handler panic", "method", r.Method, "path", r.URL.Path,
				"panic", rec, "stack", string(debug.Stack()))
			http.Error(w, "internal server error", http.StatusInternalServerError)
		}()
		next.ServeHTTP(w, r)
	})
}

// logRequests logs one line per request. Only the path is logged, never the
// query string: Jellyfin clients put access tokens there (?ApiKey=…).
// Successful requests are debug-level because clients poll constantly
// (Streamyfin calls GET /Sessions every ~2s).
func logRequests(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor) // keeps Flusher/Hijacker for streams and websockets
		next.ServeHTTP(ww, r)
		path := r.URL.Path // read after routing, so it's the canonical casing
		status := ww.Status()
		if status == 0 {
			status = http.StatusOK
		}
		level := slog.LevelDebug
		switch {
		case status >= 500:
			level = slog.LevelError
		case status >= 400:
			level = slog.LevelInfo
		}
		log.Log(r.Context(), level, "request", "method", r.Method, "path", path, "status", status,
			"bytes", ww.BytesWritten(), "took", time.Since(start), "remote", r.RemoteAddr)
	})
}

// cors mirrors Jellyfin 12.1.0's defaults (observed on the recon server):
// "Access-Control-Allow-Origin: *" on every response, and preflight requests
// answered 204 with the requested method and headers echoed back.
func cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Access-Control-Allow-Origin", "*")
		if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
			h.Set("Access-Control-Allow-Methods", r.Header.Get("Access-Control-Request-Method"))
			if hdrs := r.Header.Get("Access-Control-Request-Headers"); hdrs != "" {
				h.Set("Access-Control-Allow-Headers", hdrs)
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}
