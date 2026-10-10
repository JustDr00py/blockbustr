package jfapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"runtime/debug"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/sysadmin/blockbustr/internal/metrics"
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

// StatusClientClosed (nginx's 499) answers a request whose client hung up
// before the response was ready; no client sees it.
const StatusClientClosed = 499

// slowRequest is how long a request runs before its log line carries the
// query string.
const slowRequest = 5 * time.Second

// redactedQuery renders q for a log line without credentials: Jellyfin
// clients send tokens as ApiKey/api_key, and signed URLs carry signatures.
func redactedQuery(q url.Values) string {
	for k := range q {
		lk := strings.ToLower(k)
		for _, secret := range []string{"key", "token", "sig", "auth", "password"} {
			if strings.Contains(lk, secret) {
				q[k] = []string{"REDACTED"}
				break
			}
		}
	}
	return q.Encode()
}

// logRequests logs one line per request. The raw query string is never
// logged: Jellyfin clients put access tokens there (?ApiKey=…).
// Successful requests are debug-level because clients poll constantly
// (Streamyfin calls GET /Sessions every ~2s).
func logRequests(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor) // keeps Flusher/Hijacker for streams and websockets
		// chi reuses a route context it finds, so the matched pattern can be
		// read back for the metrics (bounded labels, unlike raw paths).
		rctx := chi.NewRouteContext()
		r = r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rctx))
		next.ServeHTTP(ww, r)
		path := r.URL.Path // read after routing, so it's the canonical casing
		status := ww.Status()
		if status == 0 {
			status = http.StatusOK
		}
		route := rctx.RoutePattern()
		if route == "" {
			route = "unmatched"
		}
		took := time.Since(start)
		metrics.ObserveRequest(route, r.Method, status, took.Seconds())
		level := slog.LevelDebug
		switch {
		case status == StatusClientClosed && took >= slowRequest:
			level = slog.LevelWarn // the client gave up waiting on us
		case status == StatusClientClosed:
		case status >= 500:
			level = slog.LevelError
		case status >= 400:
			level = slog.LevelInfo
		}
		attrs := []any{"method", r.Method, "path", path, "status", status,
			"bytes", ww.BytesWritten(), "took", took, "remote", r.RemoteAddr}
		// A failed or slow request is hard to reproduce without its
		// parameters, so those carry the query string, credentials removed.
		if (status >= 500 || took >= slowRequest) && r.URL.RawQuery != "" {
			attrs = append(attrs, "query", redactedQuery(r.URL.Query()))
		}
		log.Log(r.Context(), level, "request", attrs...)
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
