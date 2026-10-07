package handlers

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/sysadmin/blockbustr/internal/events"
)

// Home-screen and TV endpoints (TASKS P1.21): /Items/Latest,
// /UserItems/Resume, /Shows/NextUp, /Shows/{id}/Seasons,
// /Shows/{id}/Episodes and /Items/{id}/Similar, against the 12.1.0 captures.

// Expected differences from Jellyfin, not bugs (see itemIgnores): capture-time
// playback state (UserData.LastPlayedDate/PlayedPercentage) differs from the
// single state the seed holds.
var pageIgnores = []string{
	"$.Items[*].UserData.LastPlayedDate", "$.Items[*].UserData.PlayedPercentage",
}
var latestIgnores = []string{ // /Items/Latest answers with a bare array
	"$[*].UserData.LastPlayedDate", "$[*].UserData.PlayedPercentage",
}

// itemView is the slice of an item these tests compare.
type itemView struct {
	Type, Name  string
	IndexNumber *int
	ChildCount  *int
}

// sameItems replays every capture of endpoint and requires the same items in
// the same order (captures whose recorded list is empty are skipped: they
// predate the user starting anything, and the seed holds the final state).
func sameItems(t *testing.T, h http.Handler, endpoint string, array bool) {
	t.Helper()
	for _, c := range capturesFor(t, endpoint) {
		if c.Response.Status != 200 {
			continue
		}
		var want, got struct {
			Items []itemView
		}
		if array {
			var w, g []itemView
			_ = json.Unmarshal(c.Response.Body, &w)
			_ = json.Unmarshal(replay(t, h, c).Body.Bytes(), &g)
			want.Items, got.Items = w, g
		} else {
			_ = json.Unmarshal(c.Response.Body, &want)
			_ = json.Unmarshal(replay(t, h, c).Body.Bytes(), &got)
		}
		if len(want.Items) == 0 {
			continue
		}
		label := func(v []itemView) string {
			var s []string
			for _, it := range v {
				cc := ""
				if it.ChildCount != nil {
					cc = "+CC"
				}
				s = append(s, it.Type+":"+it.Name+cc)
			}
			return strings.Join(s, " | ")
		}
		if label(want.Items) != label(got.Items) {
			t.Errorf("%s ?%s\n  jellyfin:   %s\n  blockbustr: %s", c.Name,
				url.Values(c.Request.Query).Encode(), label(want.Items), label(got.Items))
		}
	}
}

// latestUnprobed reports whether a Latest capture predates Jellyfin probing
// the .strm movies: their first movie carries no Container.
func latestUnprobed(c capture) bool {
	var items []struct {
		Type      string
		Container *string
	}
	_ = json.Unmarshal(c.Response.Body, &items)
	for _, it := range items {
		if it.Type == "Movie" {
			return it.Container == nil
		}
	}
	return false
}

func TestLatestContract(t *testing.T) {
	h, d := newIntegrationServer(t)
	seedRecon(t, d)
	seedCaptureToken(t, d)
	var probed, unprobed []capture
	for _, c := range capturesFor(t, "GET /Items/Latest") {
		if latestUnprobed(c) {
			unprobed = append(unprobed, c)
		} else {
			probed = append(probed, c)
		}
	}
	checkContractOn(t, h, probed, latestIgnores...)
	// Findroid's early Latest captures predate Jellyfin probing the .strm
	// files: replay them against unprobed sources (as
	// TestItemDetailUnprobedContract does for details).
	if len(unprobed) == 0 {
		t.Fatal("no unprobed Latest captures")
	}
	execSQL(t, `DELETE FROM media_streams WHERE media_source_id IN (SELECT id FROM media_sources WHERE protocol = 'Http')`)
	execSQL(t, `UPDATE media_sources SET container = NULL, size = NULL, bitrate = NULL, runtime_ticks = NULL WHERE protocol = 'Http'`)
	execSQL(t, `UPDATE items SET runtime_ticks = NULL WHERE id IN (SELECT item_id FROM media_sources WHERE protocol = 'Http')`)
	checkContractOn(t, h, unprobed, latestIgnores...)
}

// Latest items are date-ordered and stable across captures, including the
// grouped duplicate Jellyfin sends for a TV library ([Season 1, MF GHOST,
// MF GHOST+ChildCount]).
func TestLatestSameItemsAsJellyfin(t *testing.T) {
	h, d := newIntegrationServer(t)
	seedRecon(t, d)
	seedCaptureToken(t, d)
	sameItems(t, h, "GET /Items/Latest", true)
}

func TestResumeContract(t *testing.T) {
	h, d := newIntegrationServer(t)
	seedRecon(t, d)
	seedCaptureToken(t, d)
	checkContractOn(t, h, capturesFor(t, "GET /UserItems/Resume"), pageIgnores...)
}

func TestResumeSameItemsAsJellyfin(t *testing.T) {
	h, d := newIntegrationServer(t)
	seedRecon(t, d)
	seedCaptureToken(t, d)
	// The seed's merged record for Little Fockers may hold an earlier
	// capture's position; pin the latest one, as TestItemsUserData does.
	execSQL(t, `UPDATE user_data SET playback_position_ticks = 26364670000, played = false, last_played_at = now() WHERE item_id = $1`, fockers)
	sameItems(t, h, "GET /UserItems/Resume", false)
}

func TestNextUpContract(t *testing.T) {
	h, d := newIntegrationServer(t)
	seedRecon(t, d)
	seedCaptureToken(t, d)
	checkContractOn(t, h, capturesFor(t, "GET /Shows/NextUp"), pageIgnores...)
}

func TestNextUpRules(t *testing.T) {
	h, d := newIntegrationServer(t)
	seedRecon(t, d)
	seedCaptureToken(t, d)
	episode := func(n int) string {
		var id string
		if err := testPool.QueryRow(t.Context(),
			`SELECT id::text FROM items WHERE type = 'Episode' AND index_number = $1 ORDER BY id LIMIT 1`, n).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	// Raw writes skip the handlers' events, so retire cached results the
	// way an event would (Bus.Publish).
	write := func(sql string, args ...any) {
		t.Helper()
		execSQL(t, sql, args...)
		(&events.Bus{Cache: d.Cache}).Publish(t.Context(), events.Event{Kind: events.UserDataChanged})
	}
	names := func(query string) []string {
		t.Helper()
		var res struct {
			Items []struct{ Name string }
		}
		getJSON(t, h, query, &res)
		var out []string
		for _, it := range res.Items {
			out = append(out, it.Name)
		}
		return out
	}

	// Seeded: episode 1 was begun (play count) but not finished.
	if got := names("/Shows/NextUp"); len(got) != 1 || got[0] != "The Challenger from England" {
		t.Errorf("begun series: %v", got)
	}
	// Fully played episode 1: the next one is up.
	write(`UPDATE user_data SET played = true WHERE item_id = $1`, episode(1))
	if got := names("/Shows/NextUp"); len(got) != 1 || got[0] != "The Shocking New MFG Generation" {
		t.Errorf("after episode 1: %v", got)
	}
	// Narrowed to one series and cut off before anything aired.
	if got := names("/Shows/NextUp?seriesId=" + mfGhost); len(got) != 1 {
		t.Errorf("seriesId: %v", got)
	}
	if got := names("/Shows/NextUp?nextUpDateCutoff=2020-01-01"); len(got) != 0 {
		t.Errorf("cutoff before air dates: %v", got)
	}
	// A half-watched episode counts once enableResumable is set; without it
	// the next fresh one is up.
	write(`DELETE FROM user_data WHERE item_id IN (SELECT id FROM items WHERE type = 'Episode')`)
	write(`INSERT INTO user_data (user_id, item_id, playback_position_ticks, last_played_at)
		VALUES ($1, $2, 500000000, now())`, captureUserID, episode(2))
	if got := names("/Shows/NextUp"); len(got) != 1 || got[0] != "The Challenger from England" {
		t.Errorf("resumable skipped by default: %v", got)
	}
	if got := names("/Shows/NextUp?enableResumable=true"); len(got) != 1 || got[0] != "The Shocking New MFG Generation" {
		t.Errorf("resumable included on request: %v", got)
	}
	// Nothing started: no next up (an unstarted series isn't "next").
	write(`DELETE FROM user_data WHERE item_id IN (SELECT id FROM items WHERE type = 'Episode')`)
	if got := names("/Shows/NextUp"); len(got) != 0 {
		t.Errorf("unstarted series: %v", got)
	}
	// Everything played: no next up either.
	write(`INSERT INTO user_data (user_id, item_id, played, play_count, last_played_at)
		SELECT $1, id, true, 1, now() FROM items WHERE type = 'Episode'`, captureUserID)
	if got := names("/Shows/NextUp"); len(got) != 0 {
		t.Errorf("finished series: %v", got)
	}
}

func TestSeasonsContract(t *testing.T) {
	h, d := newIntegrationServer(t)
	seedRecon(t, d)
	seedCaptureToken(t, d)
	checkContractOn(t, h, capturesFor(t, "GET /Shows/{id}/Seasons"), pageIgnores...)
	sameItems(t, h, "GET /Shows/{id}/Seasons", false)
}

func TestEpisodesContract(t *testing.T) {
	h, d := newIntegrationServer(t)
	seedRecon(t, d)
	seedCaptureToken(t, d)
	checkContractOn(t, h, capturesFor(t, "GET /Shows/{id}/Episodes"), pageIgnores...)
	sameItems(t, h, "GET /Shows/{id}/Episodes", false)
}

// adjacentTo answers the episode before, the named one and the one after.
func TestEpisodesAdjacentTo(t *testing.T) {
	h, d := newIntegrationServer(t)
	seedRecon(t, d)
	seedCaptureToken(t, d)
	episode := func(n int) string {
		t.Helper()
		var id string
		if err := testPool.QueryRow(t.Context(),
			`SELECT id::text FROM items WHERE type = 'Episode' AND index_number = $1 ORDER BY id LIMIT 1`, n).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	ordered := func(adjacent string) []string {
		var res struct {
			Items            []struct{ Name string }
			TotalRecordCount int
		}
		getJSON(t, h, "/Shows/"+mfGhost+"/Episodes?adjacentTo="+adjacent+"&limit=3", &res)
		var out []string
		for _, it := range res.Items {
			out = append(out, it.Name)
		}
		if res.TotalRecordCount != len(out) {
			t.Errorf("total %d, items %d", res.TotalRecordCount, len(out))
		}
		return out
	}
	for _, c := range []struct {
		adjacent int
		want     string
	}{
		{1, "The Challenger from England|The Shocking New MFG Generation"},
		{3, "The Shocking New MFG Generation|The Kamaboko Straight|Tire Management"},
		{5, "Tire Management|Teamwork"},
	} {
		if got := strings.Join(ordered(episode(c.adjacent)), "|"); got != c.want {
			t.Errorf("adjacentTo E%d: %s", c.adjacent, got)
		}
	}
}

func TestSimilarContract(t *testing.T) {
	h, d := newIntegrationServer(t)
	seedRecon(t, d)
	seedCaptureToken(t, d)
	checkContractOn(t, h, capturesFor(t, "GET /Items/{id}/Similar"), pageIgnores...)
	sameItems(t, h, "GET /Items/{id}/Similar", false)
}
