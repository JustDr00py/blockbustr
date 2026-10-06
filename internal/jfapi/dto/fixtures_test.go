package dto

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// Each captured endpoint's 200 response body must decode into its generated
// type with no unknown fields and re-encode to the same JSON value: same keys,
// same omitted/explicit nulls, same ID and timestamp spellings. This is what
// "use the DTOs and you're wire-compatible" rests on (AGENTS.md).
var responseTypes = map[string]func() any{
	"GET /System/Info/Public":                  func() any { return new(PublicSystemInfo) },
	"POST /Users/AuthenticateByName":           func() any { return new(AuthenticationResult) },
	"GET /Users/Public":                        func() any { return new([]UserDto) },
	"GET /UserViews":                           func() any { return new(BaseItemDtoQueryResult) },
	"GET /Items":                               func() any { return new(BaseItemDtoQueryResult) },
	"GET /Items/{id}":                          func() any { return new(BaseItemDto) },
	"GET /Items/Latest":                        func() any { return new([]BaseItemDto) },
	"GET /Items/Suggestions":                   func() any { return new(BaseItemDtoQueryResult) },
	"GET /Items/{id}/Similar":                  func() any { return new(BaseItemDtoQueryResult) },
	"GET /UserItems/Resume":                    func() any { return new(BaseItemDtoQueryResult) },
	"GET /Shows/NextUp":                        func() any { return new(BaseItemDtoQueryResult) },
	"GET /Shows/{id}/Seasons":                  func() any { return new(BaseItemDtoQueryResult) },
	"GET /Shows/{id}/Episodes":                 func() any { return new(BaseItemDtoQueryResult) },
	"GET /Studios":                             func() any { return new(BaseItemDtoQueryResult) },
	"GET /Persons":                             func() any { return new(BaseItemDtoQueryResult) },
	"GET /Artists":                             func() any { return new(BaseItemDtoQueryResult) },
	"GET /Items/Filters":                       func() any { return new(QueryFiltersLegacy) },
	"GET /Items/Filters2":                      func() any { return new(QueryFilters) },
	"GET /Items/{id}/Collections":              func() any { return new(BaseItemDtoQueryResult) },
	"GET /Items/{id}/ThemeMedia":               func() any { return new(AllThemeMediaResult) },
	"GET /LiveTv/Programs/Recommended":         func() any { return new(BaseItemDtoQueryResult) },
	"GET /Users/{id}/Items/{id}/Intros":        func() any { return new(BaseItemDtoQueryResult) },
	"GET /SyncPlay/List":                       func() any { return new([]GroupInfoDto) },
	"POST /QuickConnect/Initiate":              func() any { return new(QuickConnectResult) },
	"GET /QuickConnect/Connect":                func() any { return new(QuickConnectResult) },
	"POST /Users/AuthenticateWithQuickConnect": func() any { return new(AuthenticationResult) },
	"POST /Items/{id}/PlaybackInfo":            func() any { return new(PlaybackInfoResponse) },
	"GET /Sessions":                            func() any { return new([]SessionInfoDto) },
	"GET /MediaSegments/{id}":                  func() any { return new(MediaSegmentDtoQueryResult) },
	"GET /Branding/Configuration":              func() any { return new(BrandingOptionsDto) },
	"GET /DisplayPreferences/usersettings":     func() any { return new(DisplayPreferencesDto) },
	"POST /Users/{id}/FavoriteItems/{id}":      func() any { return new(UserItemDataDto) },
	"DELETE /Users/{id}/FavoriteItems/{id}":    func() any { return new(UserItemDataDto) },
}

type fixture struct {
	Endpoint string `json:"endpoint"`
	Response *struct {
		Status int             `json:"status"`
		Body   json.RawMessage `json:"body"`
	} `json:"response"`
}

func TestFixturesRoundTrip(t *testing.T) {
	files, _ := filepath.Glob("../../../testdata/jellyfin/*/*/[0-9]*.json")
	if len(files) == 0 {
		t.Skip("no scrubbed fixtures in testdata/jellyfin")
	}
	lookup := map[string]func() any{}
	for k, v := range responseTypes {
		lookup[strings.ToLower(k)] = v
	}
	checked := map[string]int{}
	for _, path := range files {
		if strings.Contains(path, string(filepath.Separator)+"_raw"+string(filepath.Separator)) {
			continue
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var fx fixture
		if err := json.Unmarshal(raw, &fx); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		newT := lookup[strings.ToLower(fx.Endpoint)]
		if newT == nil || fx.Response == nil || fx.Response.Status != 200 || len(fx.Response.Body) == 0 || string(fx.Response.Body) == "null" {
			continue
		}
		name := filepath.Join(filepath.Base(filepath.Dir(filepath.Dir(path))), filepath.Base(filepath.Dir(path)), filepath.Base(path))
		if err := roundTrip(fx.Response.Body, newT()); err != nil {
			t.Errorf("%s (%s): %v", name, fx.Endpoint, err)
			continue
		}
		checked[fx.Endpoint]++
	}
	if len(checked) < 15 {
		t.Errorf("only %d endpoints had fixtures to check: %v", len(checked), checked)
	}
	total := 0
	keys := make([]string, 0, len(checked))
	for k, n := range checked {
		keys = append(keys, fmt.Sprintf("%s×%d", k, n))
		total += n
	}
	sort.Strings(keys)
	t.Logf("round-tripped %d responses across %d endpoints: %s", total, len(checked), strings.Join(keys, ", "))
}

func roundTrip(body json.RawMessage, dst any) error {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	out, err := json.Marshal(dst)
	if err != nil {
		return fmt.Errorf("encode: %w", err)
	}
	var want, got any
	_ = json.Unmarshal(body, &want)
	_ = json.Unmarshal(out, &got)
	if diff := firstDiff("$", want, got); diff != "" {
		return fmt.Errorf("re-encoded JSON differs: %s", diff)
	}
	return nil
}

// firstDiff describes the first difference between two decoded JSON values.
func firstDiff(path string, want, got any) string {
	switch w := want.(type) {
	case map[string]any:
		g, ok := got.(map[string]any)
		if !ok {
			return fmt.Sprintf("%s: want object, got %T", path, got)
		}
		keys := make([]string, 0, len(w)+len(g))
		for k := range w {
			keys = append(keys, k)
		}
		for k := range g {
			if _, ok := w[k]; !ok {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		for _, k := range keys {
			wv, wok := w[k]
			gv, gok := g[k]
			switch {
			case !gok:
				return fmt.Sprintf("%s.%s: missing (want %v)", path, k, wv)
			case !wok:
				return fmt.Sprintf("%s.%s: unexpected (got %v)", path, k, gv)
			}
			if d := firstDiff(path+"."+k, wv, gv); d != "" {
				return d
			}
		}
		return ""
	case []any:
		g, ok := got.([]any)
		if !ok || len(g) != len(w) {
			return fmt.Sprintf("%s: want array of %d, got %v", path, len(w), got)
		}
		for i := range w {
			if d := firstDiff(fmt.Sprintf("%s[%d]", path, i), w[i], g[i]); d != "" {
				return d
			}
		}
		return ""
	default:
		if !reflect.DeepEqual(want, got) {
			return fmt.Sprintf("%s: want %v, got %v", path, want, got)
		}
		return ""
	}
}
