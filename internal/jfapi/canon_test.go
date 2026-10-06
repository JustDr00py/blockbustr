package jfapi

import "testing"

func TestCanonicalizer(t *testing.T) {
	c := newCanonicalizer()
	for _, p := range []string{
		"/System/Info/Public",
		"/Items/{itemId}",
		"/Items/Latest",
		"/Items/{itemId}/Images/{imageType}",
		"/Items/{itemId}/Images/{imageType}/{imageIndex}",
		"/Users/{userId}/Items/{itemId}",
		"/Videos/{itemId}/stream",
		"/Videos/{itemId}/stream.{container}",
		"/Videos/{itemId}/hls1/{playlistId}/{segmentId}.{segmentContainer}",
		"/web/*",
		"/",
	} {
		c.add(p)
	}
	cases := []struct{ in, want string }{
		{"/system/info/PUBLIC", "/System/Info/Public"},
		{"/items/latest", "/Items/Latest"}, // literal wins over {itemId}
		{"/ITEMS/84088E11EB63", "/Items/84088E11EB63"},
		{"/items/abc/images/Primary", "/Items/abc/Images/Primary"}, // param value case kept
		{"/items/abc/images/backdrop/0", "/Items/abc/Images/backdrop/0"},
		{"/users/U1/items/I1", "/Users/U1/Items/I1"},
		{"/videos/x/STREAM.MKV", "/Videos/x/stream.MKV"}, // literal part canonicalised, param kept
		{"/videos/x/stream", "/Videos/x/stream"},
		{"/videos/x/HLS1/main/12.ts", "/Videos/x/hls1/main/12.ts"},
		{"/WEB/Assets/Main.JS", "/web/Assets/Main.JS"},
		{"/Items/Latest/", "/Items/Latest"},
		{"/", "/"},
	}
	for _, tc := range cases {
		got, ok := c.canonical(tc.in)
		if !ok || got != tc.want {
			t.Errorf("canonical(%q) = %q, %v; want %q", tc.in, got, ok, tc.want)
		}
	}
	for _, miss := range []string{"/Nope", "/Items/a/b/c/d/e", "/System/Info/Public/extra"} {
		if got, ok := c.canonical(miss); ok || got != miss {
			t.Errorf("unregistered %q matched as %q", miss, got)
		}
	}
}

func TestParseSegment(t *testing.T) {
	got := parseSegment("{segmentId}.{segmentContainer}")
	if len(got) != 3 || !got[0].param || got[1].lit != "." || !got[2].param {
		t.Errorf("parseSegment = %+v", got)
	}
	if p := parseSegment("plain"); len(p) != 1 || p[0].param {
		t.Errorf("plain = %+v", p)
	}
}
