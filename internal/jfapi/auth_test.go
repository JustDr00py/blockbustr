package jfapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func authReq(t *testing.T, query string, hdr map[string]string) *http.Request {
	t.Helper()
	r := httptest.NewRequestWithContext(t.Context(), "GET", "/x"+query, nil)
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	return r
}

func TestParseAuth(t *testing.T) {
	const findroid = `MediaBrowser Client="Findroid", Version="1.1.0", DeviceId="dev-1", Device="Pixel 8", Token="tok-1"`
	cases := []struct {
		name   string
		legacy bool
		query  string
		hdr    map[string]string
		want   AuthInfo
	}{
		{"findroid", false, "", map[string]string{"Authorization": findroid},
			AuthInfo{Client: "Findroid", Device: "Pixel 8", DeviceID: "dev-1", Version: "1.1.0", Token: "tok-1", TokenSource: "authorization"}},
		{"percent-encoded client, token null before login", false, "", map[string]string{"Authorization": `MediaBrowser Client="Jellyfin%20for%20Android", Device="Moto", DeviceId="d", Version="2.7.3", Token="null"`},
			AuthInfo{Client: "Jellyfin for Android", Device: "Moto", DeviceID: "d", Version: "2.7.3"}},
		{"plus-encoded client", false, "", map[string]string{"Authorization": `MediaBrowser Client="Jellyfin+for+Android", DeviceId="d", Token="t"`},
			AuthInfo{Client: "Jellyfin for Android", DeviceID: "d", Token: "t", TokenSource: "authorization"}},
		{"streamyfin empty token, dashed device id", false, "", map[string]string{"authorization": `MediaBrowser Client="Streamyfin", Device="Moto", DeviceId="dddd0000-0000-0000-0000-000000000001", Version="0.55.0", Token=""`},
			AuthInfo{Client: "Streamyfin", Device: "Moto", DeviceID: "dddd0000-0000-0000-0000-000000000001", Version: "0.55.0"}},
		{"token-only header", false, "", map[string]string{"Authorization": `MediaBrowser Token="t2"`},
			AuthInfo{Token: "t2", TokenSource: "authorization"}},
		{"ApiKey query, any casing", false, "?apikey=q1", nil, AuthInfo{Token: "q1", TokenSource: "query:ApiKey"}},
		{"header token wins over query", false, "?ApiKey=q1", map[string]string{"Authorization": `MediaBrowser Token="h1"`},
			AuthInfo{Token: "h1", TokenSource: "authorization"}},
		{"placeholder header token falls back to query", false, "?ApiKey=q1", map[string]string{"Authorization": `MediaBrowser Client="C", DeviceId="d", Token="null"`},
			AuthInfo{Client: "C", DeviceID: "d", Token: "q1", TokenSource: "query:ApiKey"}},
		{"scheme and keys are case-insensitive, odd spacing", false, "", map[string]string{"Authorization": `mediabrowser   client = "C" ,DEVICEID="d",token="t"`},
			AuthInfo{Client: "C", DeviceID: "d", Token: "t", TokenSource: "authorization"}},
		{"quoted commas and escapes", false, "", map[string]string{"Authorization": `MediaBrowser Device="Living Room, TV \"4K\"", DeviceId="d", Client=Kodi`},
			AuthInfo{Client: "Kodi", Device: `Living Room, TV "4K"`, DeviceID: "d"}},
		{"first duplicate wins", false, "", map[string]string{"Authorization": `MediaBrowser Token="a", Token="b"`},
			AuthInfo{Token: "a", TokenSource: "authorization"}},
		{"other schemes ignored", false, "", map[string]string{"Authorization": "Bearer abc"}, AuthInfo{}},
		{"undecodable value kept as-is", false, "", map[string]string{"Authorization": `MediaBrowser Device="100%", DeviceId="d"`},
			AuthInfo{Device: "100%", DeviceID: "d"}},

		// Stock 12.1.0 (legacy off) rejects all of these…
		{"x-emby-token rejected", false, "", map[string]string{"X-Emby-Token": "t"}, AuthInfo{}},
		{"x-emby-authorization rejected", false, "", map[string]string{"X-Emby-Authorization": `MediaBrowser Client="Kodi", DeviceId="k", Token="t"`}, AuthInfo{}},
		{"api_key rejected", false, "?api_key=q", nil, AuthInfo{}},
		{"Emby scheme rejected", false, "", map[string]string{"Authorization": `Emby Client="Kodi", DeviceId="k", Token="t"`}, AuthInfo{}},
		// …and compat.legacy_auth accepts them.
		{"legacy x-emby-token", true, "", map[string]string{"X-Emby-Token": "t"}, AuthInfo{Token: "t", TokenSource: "x-emby-token"}},
		{"legacy x-mediabrowser-token", true, "", map[string]string{"X-MediaBrowser-Token": "t"}, AuthInfo{Token: "t", TokenSource: "x-mediabrowser-token"}},
		{"legacy x-emby-authorization", true, "", map[string]string{"X-Emby-Authorization": `MediaBrowser Client="Kodi", DeviceId="k", Token="t"`},
			AuthInfo{Client: "Kodi", DeviceID: "k", Token: "t", TokenSource: "x-emby-authorization"}},
		{"legacy Emby scheme", true, "", map[string]string{"Authorization": `Emby Client="Kodi", DeviceId="k"`}, AuthInfo{Client: "Kodi", DeviceID: "k"}},
		{"legacy api_key", true, "?api_key=q", nil, AuthInfo{Token: "q", TokenSource: "query:api_key"}},
		{"Authorization beats X-Emby-Authorization", true, "", map[string]string{
			"Authorization": `MediaBrowser Client="A", DeviceId="a"`, "X-Emby-Authorization": `MediaBrowser Client="B", DeviceId="b", Token="tb"`},
			AuthInfo{Client: "A", DeviceID: "a"}},
		{"header token beats legacy token header", true, "", map[string]string{"Authorization": `MediaBrowser Token="h"`, "X-Emby-Token": "x"},
			AuthInfo{Token: "h", TokenSource: "authorization"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ParseAuth(authReq(t, c.query, c.hdr), c.legacy)
			if got != c.want {
				t.Errorf("got  %+v\nwant %+v", got, c.want)
			}
		})
	}
}

// Every request in the scrubbed captures must parse to the same client,
// device and token that the capture tool recorded (with the client name
// URL-decoded, which Jellyfin does too).
func TestParseAuthMatchesCaptures(t *testing.T) {
	files, _ := filepath.Glob("../../testdata/jellyfin/*/*/[0-9]*.json")
	checked := 0
	for _, path := range files {
		if strings.Contains(path, "/_raw/") {
			continue
		}
		raw, _ := os.ReadFile(path)
		var fx struct {
			Request struct {
				Headers map[string]string   `json:"headers"`
				Query   map[string][]string `json:"query"`
				Auth    map[string]string   `json:"auth"`
			} `json:"request"`
		}
		if err := json.Unmarshal(raw, &fx); err != nil {
			t.Fatal(err)
		}
		want := fx.Request.Auth
		if want == nil {
			continue
		}
		r := authReq(t, "?"+url.Values(fx.Request.Query).Encode(), fx.Request.Headers)
		got := ParseAuth(r, false) // the captures came from stock 12.1.0
		name := filepath.Base(filepath.Dir(filepath.Dir(path))) + "/" + filepath.Base(path)

		if c, _ := url.QueryUnescape(want["Client"]); got.Client != c {
			t.Errorf("%s: Client %q, want %q", name, got.Client, c)
		}
		if got.DeviceID != want["DeviceId"] {
			t.Errorf("%s: DeviceID %q, want %q", name, got.DeviceID, want["DeviceId"])
		}
		if tok := want["Token"]; tok != "" && tok != "null" && got.Token != tok {
			t.Errorf("%s: Token %q, want %q", name, got.Token, tok)
		}
		checked++
	}
	if checked < 1000 {
		t.Errorf("only %d captured requests had auth to check", checked)
	}
	t.Logf("checked auth parsing on %d captured requests", checked)
}
