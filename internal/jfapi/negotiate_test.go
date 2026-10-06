package jfapi

import "testing"

// Every case was checked against Jellyfin 12.1.0 on the recon server, or
// taken from Accept headers real clients sent in the captures.
func TestNegotiate(t *testing.T) {
	cases := []struct {
		accept []string
		camel  bool
		ct     string
	}{
		{nil, false, contentTypeJSON},
		{[]string{"*/*"}, false, contentTypeJSON},
		{[]string{"application/json"}, false, contentTypeJSON},
		{[]string{"application/json, text/plain, */*"}, false, contentTypeJSON},                           // Streamyfin
		{[]string{"application/json, application/octet-stream;q=0.9, */*;q=0.8"}, false, contentTypeJSON}, // Findroid
		{[]string{"text/html, */*;q=0.8"}, false, contentTypeJSON},
		{[]string{"application/json; q=0.5, */*"}, false, contentTypeJSON},
		{[]string{"application/xml"}, false, contentTypeJSON}, // Jellyfin still answers JSON
		{[]string{`application/json;profile="CamelCase"`}, true, contentTypeCamelJSON},
		{[]string{"application/json; profile=CamelCase"}, true, contentTypeCamelJSON},
		{[]string{"application/json; profile=camelcase"}, true, contentTypeCamelJSON},
		{[]string{`application/json; profile="PascalCase"`}, false, contentTypePascalJSON},
		// Jellyfin Android's WebView: plain application/json comes first, so PascalCase.
		{[]string{"application/json,application/json; profile=CamelCase,application/json; profile=PascalCase,text/html"}, false, contentTypeJSON},
		// q ordering decides, not header order.
		{[]string{"application/json;q=0.5, application/json;profile=CamelCase"}, true, contentTypeCamelJSON},
		{[]string{"application/json;profile=CamelCase;q=0"}, false, contentTypeJSON},
		{[]string{"not a media type, */*"}, false, contentTypeJSON},
	}
	for _, c := range cases {
		f := negotiate(c.accept)
		if f.camel != c.camel || f.contentType != c.ct {
			t.Errorf("negotiate(%q) = %+v; want camel=%v ct=%q", c.accept, f, c.camel, c.ct)
		}
	}
}
