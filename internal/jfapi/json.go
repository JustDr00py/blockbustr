package jfapi

import (
	"bytes"
	"encoding"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"reflect"
	"strconv"
	"strings"
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
//
// Scalars are read as leniently as Jellyfin's converters read them: a
// number where a string belongs, and a numeric or "true"/"false" string
// where a number or bool belongs, are converted rather than refused.
// jellyfin-roku, for one, sends the AudioChannels profile condition's
// Value as a bare number.
func DecodeJSON(w http.ResponseWriter, r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(w, r.Body, MaxBodyBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return err
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return nil
	}
	err = json.Unmarshal(body, v)
	var typeErr *json.UnmarshalTypeError
	if !errors.As(err, &typeErr) {
		return err
	}
	// Only a mistyped scalar is worth a second, coercing pass.
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var raw any
	if dec.Decode(&raw) != nil {
		return err
	}
	fixed, merr := json.Marshal(coerce(raw, reflect.TypeOf(v)))
	if merr != nil {
		return err
	}
	return json.Unmarshal(fixed, v)
}

var (
	jsonUnmarshaler = reflect.TypeFor[json.Unmarshaler]()
	textUnmarshaler = reflect.TypeFor[encoding.TextUnmarshaler]()
)

// coerce reshapes a generically decoded JSON value (numbers as json.Number)
// toward type t, converting scalars Jellyfin would accept in another form.
// Anything it doesn't recognise is returned unchanged, so a genuinely bad
// body still fails the final Unmarshal.
func coerce(val any, t reflect.Type) any {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if reflect.PointerTo(t).Implements(jsonUnmarshaler) || reflect.PointerTo(t).Implements(textUnmarshaler) {
		return val
	}
	switch t.Kind() {
	case reflect.String:
		switch x := val.(type) {
		case json.Number:
			return string(x)
		case bool:
			return strconv.FormatBool(x)
		}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		if s, ok := val.(string); ok {
			if _, err := strconv.ParseFloat(strings.TrimSpace(s), 64); err == nil {
				return json.Number(strings.TrimSpace(s))
			}
		}
	case reflect.Bool:
		if s, ok := val.(string); ok {
			if b, err := strconv.ParseBool(strings.TrimSpace(s)); err == nil {
				return b
			}
		}
	case reflect.Slice, reflect.Array:
		if xs, ok := val.([]any); ok {
			for i := range xs {
				xs[i] = coerce(xs[i], t.Elem())
			}
		}
	case reflect.Map:
		if m, ok := val.(map[string]any); ok {
			for k := range m {
				m[k] = coerce(m[k], t.Elem())
			}
		}
	case reflect.Struct:
		if m, ok := val.(map[string]any); ok {
			fields := map[string]reflect.Type{}
			jsonFields(t, fields)
			for k := range m {
				if ft, ok := fields[strings.ToLower(k)]; ok {
					m[k] = coerce(m[k], ft)
				}
			}
		}
	}
	return val
}

// jsonFields maps struct t's lower-cased JSON field names to their types,
// descending into embedded structs as encoding/json does.
func jsonFields(t reflect.Type, out map[string]reflect.Type) {
	for i := range t.NumField() {
		f := t.Field(i)
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "-" || (!f.IsExported() && !f.Anonymous) {
			continue
		}
		ft := f.Type
		for ft.Kind() == reflect.Pointer {
			ft = ft.Elem()
		}
		if f.Anonymous && name == "" && ft.Kind() == reflect.Struct {
			jsonFields(ft, out)
			continue
		}
		if name == "" {
			name = f.Name
		}
		if _, dup := out[strings.ToLower(name)]; !dup {
			out[strings.ToLower(name)] = f.Type
		}
	}
}
