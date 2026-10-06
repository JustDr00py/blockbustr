package handlers

import (
	"encoding/json"
	"net/url"
	"sort"
	"strings"
	"testing"
)

// Suggestions are random, so each capture is compared as a set of items,
// and each item's shape against Jellyfin's item with the same id.
func TestSuggestionsSameAsJellyfin(t *testing.T) {
	var probed, unprobed []capture
	for _, c := range capturesFor(t, "GET /Items/Suggestions") {
		var b struct{ Items []map[string]any }
		_ = json.Unmarshal(c.Response.Body, &b)
		var early []string
		for _, it := range b.Items {
			if it["Type"] == "Movie" && it["Container"] == nil { // a .strm Jellyfin hadn't probed yet
				early = append(early, it["Id"].(string))
			}
		}
		if len(early) > 0 {
			// Findroid's captures predate Jellyfin probing some .strm files
			// (Luca was probed before Little Fockers): replay each against
			// the same unprobed sources, as TestLatestContract does.
			t.Run(c.Name, func(t *testing.T) { checkSuggestions(t, []capture{c}, early) })
			unprobed = append(unprobed, c)
		} else {
			probed = append(probed, c)
		}
	}
	if len(probed) == 0 || len(unprobed) == 0 {
		t.Fatalf("captures: %d probed, %d unprobed", len(probed), len(unprobed))
	}
	t.Run("probed", func(t *testing.T) { checkSuggestions(t, probed, nil) })
}

// checkSuggestions replays caps, with the items in unprobed reset to a
// never-probed .strm.
func checkSuggestions(t *testing.T, caps []capture, unprobed []string) {
	h, d := newIntegrationServer(t)
	seedRecon(t, d)
	seedCaptureToken(t, d)
	for _, id := range unprobed {
		execSQL(t, `DELETE FROM media_streams WHERE media_source_id IN (SELECT id FROM media_sources WHERE item_id = $1)`, id)
		execSQL(t, `UPDATE media_sources SET container = NULL, size = NULL, bitrate = NULL, runtime_ticks = NULL WHERE item_id = $1`, id)
		execSQL(t, `UPDATE items SET runtime_ticks = NULL WHERE id = $1`, id)
	}
	skip := map[string]bool{}
	for _, p := range detailIgnores {
		skip[p] = true
	}
	for _, c := range caps {
		var want, got struct {
			Items            []map[string]any
			TotalRecordCount int
		}
		_ = json.Unmarshal(c.Response.Body, &want)
		rec := replay(t, h, c)
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || rec.Code != 200 {
			t.Fatalf("%s: %d %s", c.Name, rec.Code, rec.Body)
		}
		names := func(items []map[string]any) string {
			var n []string
			for _, it := range items {
				n = append(n, it["Name"].(string))
			}
			sort.Strings(n)
			return strings.Join(n, " | ")
		}
		if names(got.Items) != names(want.Items) || got.TotalRecordCount != want.TotalRecordCount {
			t.Errorf("%s ?%s\n  jellyfin:   %s (%d)\n  blockbustr: %s (%d)", c.Name, url.Values(c.Request.Query).Encode(),
				names(want.Items), want.TotalRecordCount, names(got.Items), got.TotalRecordCount)
			continue
		}
		byID := map[string]map[string]any{}
		for _, it := range got.Items {
			byID[it["Id"].(string)] = it
		}
		for _, w := range want.Items {
			if err := sameShapeIgnoring("$", w, byID[w["Id"].(string)], skip); err != nil {
				t.Errorf("%s: %s: %v", c.Name, w["Name"], err)
			}
		}
	}
}

func TestSuggestionsRandomAndLegacy(t *testing.T) {
	h, d := newIntegrationServer(t)
	seedRecon(t, d)
	seedCaptureToken(t, d)
	var res struct {
		Items            []struct{ Name, Type string }
		TotalRecordCount int
	}
	orders := map[string]bool{}
	for range 30 {
		getJSON(t, h, "/Items/Suggestions?type=Movie&mediaType=Video", &res)
		var n []string
		for _, it := range res.Items {
			n = append(n, it.Name)
			if it.Type != "Movie" {
				t.Fatalf("type filter: %+v", res.Items)
			}
		}
		orders[strings.Join(n, "|")] = true
	}
	if len(orders) < 2 {
		t.Errorf("the order never changed over 30 calls: %v", orders)
	}
	getJSON(t, h, "/Users/"+strings.ReplaceAll(captureUserID, "-", "")+"/Suggestions?type=Series&limit=1&enableTotalRecordCount=false", &res)
	if len(res.Items) != 1 || res.Items[0].Type != "Series" || res.TotalRecordCount != 1 {
		t.Errorf("legacy route, limit: %+v", res)
	}
	getJSON(t, h, "/Items/Suggestions", &res) // no type: anything, never an error
	if len(res.Items) == 0 {
		t.Errorf("no type: %+v", res)
	}
}
