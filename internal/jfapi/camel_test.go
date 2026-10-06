package jfapi

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestCamelName(t *testing.T) {
	cases := map[string]string{
		"ServerId": "serverId", "ID": "id", "Id": "id", "IPAddress": "ipAddress",
		"MyID": "myID", "A": "a", "already": "already", "HTTPS": "https",
		"URLPath": "urlPath", "IsHD": "isHD", "": "", "Ünicode": "ünicode",
	}
	for in, want := range cases {
		if got := camelName(in); got != want {
			t.Errorf("camelName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCamelJSONKeepsDictionaryKeys(t *testing.T) {
	in := `{"Name":"Luca","ImageTags":{"Primary":"t1","Logo":"t2"},` +
		`"ImageBlurHashes":{"Primary":{"Ab12":"LEHV6n"}},` +
		`"ProviderIds":{"Imdb":"tt12801262"},` +
		`"Trickplay":{"ms1":{"320":{"Width":320,"TileWidth":10}}},` +
		`"People":[{"Name":"X","ImageBlurHashes":{"Primary":{"h":"v"}}}],` +
		`"RunTimeTicks":57152000000,"CommunityRating":7.3,"Empty":[],"Null":null,"Html":"<b>&</b>"}`
	want := `{"name":"Luca","imageTags":{"Primary":"t1","Logo":"t2"},` +
		`"imageBlurHashes":{"Primary":{"Ab12":"LEHV6n"}},` +
		`"providerIds":{"Imdb":"tt12801262"},` +
		`"trickplay":{"ms1":{"320":{"width":320,"tileWidth":10}}},` +
		`"people":[{"name":"X","imageBlurHashes":{"Primary":{"h":"v"}}}],` +
		`"runTimeTicks":57152000000,"communityRating":7.3,"empty":[],"null":null,"html":"\u003cb\u003e\u0026\u003c/b\u003e"}` // escaped like json.Marshal on the PascalCase path
	got, err := camelJSON([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Errorf("camelJSON =\n%s\nwant\n%s", got, want)
	}
	if out, err := camelJSON([]byte(`[{"A":1},{"B":[true,false]}]`)); err != nil || string(out) != `[{"a":1},{"b":[true,false]}]` {
		t.Errorf("top-level array: %s %v", out, err)
	}
	if _, err := camelJSON([]byte(`{"A":`)); err == nil {
		t.Error("expected error for truncated JSON")
	}
}

// Golden pairs captured from Jellyfin 12.1.0: the same request with and
// without Accept: application/json; profile="CamelCase".
func TestCamelJSONMatchesJellyfin(t *testing.T) {
	pascals, _ := filepath.Glob("testdata/casing/*.pascal.json")
	if len(pascals) == 0 {
		t.Fatal("no golden casing pairs")
	}
	for _, p := range pascals {
		name := strings.TrimSuffix(filepath.Base(p), ".pascal.json")
		pascal, _ := os.ReadFile(p)
		camel, err := os.ReadFile(strings.Replace(p, ".pascal.", ".camel.", 1))
		if err != nil {
			t.Fatal(err)
		}
		got, err := camelJSON(pascal)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var g, w any
		if err := json.Unmarshal(got, &g); err != nil {
			t.Fatalf("%s: output not JSON: %v", name, err)
		}
		_ = json.Unmarshal(camel, &w)
		if !reflect.DeepEqual(g, w) {
			t.Errorf("%s: camelJSON differs from Jellyfin's camelCase response", name)
		}
	}
}
