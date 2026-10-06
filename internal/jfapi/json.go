package jfapi

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
)

// MaxBodyBytes caps request bodies. The largest real ones are PlaybackInfo
// DeviceProfiles, a few tens of KB.
const MaxBodyBytes = 10 << 20

// WriteJSON encodes v (normally a dto type) in the casing the request
// negotiated (DESIGN §3.1) and writes it with status.
func WriteJSON(w http.ResponseWriter, r *http.Request, status int, v any) {
	f := negotiate(r.Header.Values("Accept"))
	body, err := json.Marshal(v)
	if err == nil && f.camel {
		body, err = camelJSON(body)
	}
	if err != nil {
		slog.ErrorContext(r.Context(), "encode response", "path", r.URL.Path, "err", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", f.contentType)
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// DecodeJSON reads a JSON request body into v. Field names match
// case-insensitively (Go's default), so camelCase bodies from clients such
// as Streamyfin decode into the PascalCase DTOs. An empty body is not an
// error and leaves v unchanged, since many Jellyfin bodies are optional.
func DecodeJSON(w http.ResponseWriter, r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(w, r.Body, MaxBodyBytes)
	err := json.NewDecoder(r.Body).Decode(v)
	if errors.Is(err, io.EOF) {
		return nil
	}
	return err
}
