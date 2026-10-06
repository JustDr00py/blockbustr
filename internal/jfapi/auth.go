package jfapi

import (
	"context"
	"net/http"
	"net/url"
	"strings"
)

// AuthInfo is what a request says about its client and credentials, parsed
// from every place Jellyfin accepts them (DESIGN §3.2). It is not yet
// verified: resolving Token to a user happens in the auth layer (P1.10).
type AuthInfo struct {
	Client   string // e.g. "Findroid", "Jellyfin for Android" (URL-decoded)
	Device   string // device name
	DeviceID string // opaque client-chosen ID; may be a dashed GUID, keep as-is
	Version  string // client version
	Token    string // access token or API key; "" if none
	// TokenSource says where Token came from: "authorization",
	// "x-emby-authorization", "x-emby-token", "x-mediabrowser-token",
	// "query:ApiKey" or "query:api_key". Empty when there's no token.
	TokenSource string
}

// HasClient reports whether the request identified its client and device.
func (a AuthInfo) HasClient() bool { return a.Client != "" && a.DeviceID != "" }

// ParseAuth extracts AuthInfo from r. With legacy=false it accepts exactly
// what stock Jellyfin 12.1.0 accepts (EnableLegacyAuthorization=false): the
// "Authorization: MediaBrowser …" header and the ApiKey query parameter.
// legacy=true (compat.legacy_auth) also accepts X-Emby-Authorization, the
// "Emby" scheme, X-Emby-Token, X-MediaBrowser-Token and api_key.
func ParseAuth(r *http.Request, legacy bool) AuthInfo {
	var a AuthInfo
	fields, source := authFields(r.Header.Get("Authorization"), legacy), "authorization"
	if fields == nil && legacy {
		fields, source = authFields(r.Header.Get("X-Emby-Authorization"), true), "x-emby-authorization"
	}
	a.Client, a.Device, a.DeviceID, a.Version = fields["client"], fields["device"], fields["deviceid"], fields["version"]
	a.setToken(fields["token"], source)

	if legacy {
		a.setToken(r.Header.Get("X-Emby-Token"), "x-emby-token")
		a.setToken(r.Header.Get("X-MediaBrowser-Token"), "x-mediabrowser-token")
	}
	q := QueryOf(r)
	a.setToken(q.Get("ApiKey"), "query:ApiKey")
	if legacy {
		a.setToken(q.Get("api_key"), "query:api_key")
	}
	return a
}

// setToken keeps the first real token. Clients send placeholders before
// signing in: Jellyfin for Android uses Token="null", Streamyfin Token="".
func (a *AuthInfo) setToken(tok, source string) {
	tok = strings.TrimSpace(tok)
	if a.Token != "" || tok == "" || strings.EqualFold(tok, "null") {
		return
	}
	a.Token, a.TokenSource = tok, source
}

// authFields parses `MediaBrowser Client="Findroid", Device="Pixel", …` into
// lower-cased keys with URL-decoded values. It returns nil if the header is
// missing or uses another scheme ("Emby" counts only in legacy mode).
func authFields(header string, legacy bool) map[string]string {
	header = strings.TrimSpace(header)
	scheme, rest, _ := strings.Cut(header, " ")
	switch {
	case strings.EqualFold(scheme, "MediaBrowser"):
	case legacy && strings.EqualFold(scheme, "Emby"):
	default:
		return nil
	}
	fields := map[string]string{}
	for _, kv := range splitParams(rest) {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		k = strings.ToLower(strings.TrimSpace(k))
		v = unquote(strings.TrimSpace(v))
		// Clients URL-encode values, as "%20" or as "+" (Jellyfin for Android
		// sends both); Jellyfin decodes them like WebUtility.UrlDecode.
		if dec, err := url.QueryUnescape(v); err == nil {
			v = dec
		}
		if _, dup := fields[k]; !dup && k != "" {
			fields[k] = v
		}
	}
	return fields
}

// splitParams splits on commas that are outside double quotes.
func splitParams(s string) []string {
	var out []string
	var b strings.Builder
	inQuote, escaped := false, false
	for _, c := range s {
		switch {
		case escaped:
			escaped = false
		case c == '\\' && inQuote:
			escaped = true
		case c == '"':
			inQuote = !inQuote
		case c == ',' && !inQuote:
			out = append(out, b.String())
			b.Reset()
			continue
		}
		b.WriteRune(c)
	}
	if strings.TrimSpace(b.String()) != "" {
		out = append(out, b.String())
	}
	return out
}

// unquote strips surrounding double quotes and resolves \" and \\ escapes.
func unquote(v string) string {
	if len(v) < 2 || v[0] != '"' || v[len(v)-1] != '"' {
		return v
	}
	v = v[1 : len(v)-1]
	if !strings.Contains(v, `\`) {
		return v
	}
	var b strings.Builder
	for i := 0; i < len(v); i++ {
		if v[i] == '\\' && i+1 < len(v) {
			i++
		}
		b.WriteByte(v[i])
	}
	return b.String()
}

type authKey struct{}

// AuthFrom returns the parsed AuthInfo stored by the router's middleware.
func AuthFrom(ctx context.Context) AuthInfo {
	a, _ := ctx.Value(authKey{}).(AuthInfo)
	return a
}

// withAuthInfo parses AuthInfo once per request and stores it in the context.
func withAuthInfo(legacy bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), authKey{}, ParseAuth(r, legacy))))
	})
}
