package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sysadmin/blockbustr/internal/cache"
	"github.com/sysadmin/blockbustr/internal/store/pg/db"
	"github.com/sysadmin/blockbustr/internal/testutil"
)

func TestPlaybackLog(t *testing.T) {
	_, d := newIntegrationServer(t)
	seedRecon(t, d)
	seedCaptureToken(t, d)
	f := newRemoteFixture(t, d, nil)
	authz := `MediaBrowser Token="` + captureToken + `"`
	u, err := d.Queries.GetUserByName(t.Context(), "user1")
	if err != nil {
		t.Fatal(err)
	}
	// What PlaybackInfo leaves for the stream requests of its play sessions
	// (on two devices: one device's would join the play just stopped).
	for ps, dev := range map[string]string{"ps-proxied": "capture-device", "ps-redirected": "other-device"} {
		if err := d.Cache.SetJSON(t.Context(), cache.PlaySessionKey(ps), PlaySession{UserID: u.ID, DeviceID: dev, ItemID: uuid.MustParse(fockers)}, cache.PlaySessionTTL); err != nil {
			t.Fatal(err)
		}
	}
	stream := func(query string, hdr map[string]string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequestWithContext(context.Background(), "GET", "/Videos/"+fockers+"/stream?static=true"+query, nil)
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		f.h.ServeHTTP(rec, req)
		return rec
	}
	report := func(path, body string) {
		t.Helper()
		if rec := call(t, f.h, "POST", path, authz, body); rec.Code != 204 {
			t.Fatalf("%s = %d", path, rec.Code)
		}
	}

	// A proxied play: the report starts the row, two stream requests add
	// their bytes, the stop ends it.
	report("/Sessions/Playing", `{"ItemId":"`+fockers+`","PlaySessionId":"ps-proxied","PositionTicks":0,"PlayMethod":"DirectPlay"}`)
	if rec := stream("&PlaySessionId=ps-proxied", nil); rec.Code != 200 || rec.Body.Len() != 10 {
		t.Fatalf("proxied: %d %q", rec.Code, rec.Body)
	}
	if rec := stream("&PlaySessionId=ps-proxied", map[string]string{"Range": "bytes=2-5"}); rec.Code != 206 {
		t.Fatalf("proxied range: %d", rec.Code)
	}
	report("/Sessions/Playing/Stopped", `{"ItemId":"`+fockers+`","PlaySessionId":"ps-proxied","PositionTicks":1234}`)

	// A redirected play whose report never came, and a stream request
	// without a play session, which isn't logged.
	if rec := stream("&PlaySessionId=ps-redirected", map[string]string{"X-Forwarded-Proto": "https"}); rec.Code != 302 {
		t.Fatalf("redirect: %d", rec.Code)
	}
	if rec := stream("", nil); rec.Code != 200 {
		t.Fatalf("no play session: %d", rec.Code)
	}

	rec := call(t, f.h, "GET", "/blockbustr/playback", authz, "")
	if rec.Code != 200 {
		t.Fatalf("playback log = %d %s", rec.Code, rec.Body)
	}
	var got playbackResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Entries) != 2 {
		t.Fatalf("entries: %+v", got.Entries)
	}
	redirected, proxied := got.Entries[0], got.Entries[1] // newest first
	if proxied.UserName != "user1" || proxied.DeviceName != "captures" || proxied.PlayMethod != "DirectPlay" ||
		proxied.Delivery != deliveryProxied || proxied.Bytes != 14 || proxied.LinkHost != "127.0.0.1" ||
		proxied.PositionTicks != 1234 || proxied.StoppedAt == nil || proxied.ItemName == "" {
		t.Errorf("proxied play: %+v", proxied)
	}
	if redirected.UserName != "user1" || redirected.Delivery != deliveryRedirected || redirected.Bytes != 0 || redirected.StoppedAt != nil {
		t.Errorf("redirected play: %+v", redirected)
	}
	if len(got.Totals) != 1 || got.Totals[0].Plays != 2 || got.Totals[0].Bytes != 14 || got.Days != playbackLogDays {
		t.Errorf("totals: %+v", got.Totals)
	}

	// Filtered by user, and paged.
	rec = call(t, f.h, "GET", "/blockbustr/playback?limit=1&userId="+u.ID.String(), authz, "")
	got = playbackResponse{}
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if len(got.Entries) != 1 || !got.More || got.Entries[0].ID != redirected.ID {
		t.Errorf("first page: %+v", got)
	}
	rec = call(t, f.h, "GET", "/blockbustr/playback?userId="+uuid.NewString(), authz, "")
	got = playbackResponse{}
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if len(got.Entries) != 0 {
		t.Errorf("another user's log: %+v", got.Entries)
	}
}

// A stream request without a PlaySessionId (Streamyfin's direct play) joins
// the play its token's user reported, by item.
func TestPlaybackLogStreamWithoutPlaySession(t *testing.T) {
	_, d := newIntegrationServer(t)
	seedRecon(t, d)
	seedCaptureToken(t, d)
	f := newRemoteFixture(t, d, nil)
	authz := `MediaBrowser Token="` + captureToken + `"`
	stream := func(hdr map[string]string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequestWithContext(context.Background(), "GET", "/Videos/"+fockers+"/stream?static=true", nil)
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		f.h.ServeHTTP(rec, req)
		return rec
	}
	plays := func() []playbackEntry {
		t.Helper()
		rec := call(t, f.h, "GET", "/blockbustr/playback", authz, "")
		var got playbackResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		return got.Entries
	}

	// No row yet: the bytes aren't lost track of, but nothing is logged.
	if rec := stream(map[string]string{"Authorization": authz}); rec.Code != 200 {
		t.Fatalf("stream before the report: %d", rec.Code)
	}
	if got := plays(); len(got) != 0 {
		t.Fatalf("a play appeared from a stream alone: %+v", got)
	}

	if rec := call(t, f.h, "POST", "/Sessions/Playing", authz,
		`{"ItemId":"`+fockers+`","PlaySessionId":"ps-streamyfin","PositionTicks":0,"PlayMethod":"DirectPlay"}`); rec.Code != 204 {
		t.Fatalf("report = %d", rec.Code)
	}
	if rec := stream(map[string]string{"Authorization": authz}); rec.Code != 200 || rec.Body.Len() != 10 {
		t.Fatalf("stream: %d %q", rec.Code, rec.Body)
	}
	// A stream with no token can't be told apart by user: not attributed.
	if rec := stream(nil); rec.Code != 200 {
		t.Fatalf("anonymous stream: %d", rec.Code)
	}
	got := plays()
	if len(got) != 1 || got[0].Delivery != deliveryProxied || got[0].Bytes != 10 || got[0].LinkHost != "127.0.0.1" {
		t.Errorf("play: %+v", got)
	}
}

// A client that asks for a new play session mid-play (Void, on every track
// change) keeps one row: the new session joins the device's running play,
// and both sessions' bytes count on it.
func TestPlaybackLogNewPlaySessionMerges(t *testing.T) {
	_, d := newIntegrationServer(t)
	seedRecon(t, d)
	seedCaptureToken(t, d)
	f := newRemoteFixture(t, d, nil)
	authz := `MediaBrowser Token="` + captureToken + `"`
	u, err := d.Queries.GetUserByName(t.Context(), "user1")
	if err != nil {
		t.Fatal(err)
	}
	for _, ps := range []string{"ps-first", "ps-second"} {
		if err := d.Cache.SetJSON(t.Context(), cache.PlaySessionKey(ps), PlaySession{UserID: u.ID, DeviceID: "capture-device", ItemID: uuid.MustParse(fockers)}, cache.PlaySessionTTL); err != nil {
			t.Fatal(err)
		}
	}
	for _, ps := range []string{"ps-first", "ps-second"} {
		if rec := call(t, f.h, "POST", "/Sessions/Playing", authz,
			`{"ItemId":"`+fockers+`","PlaySessionId":"`+ps+`","PositionTicks":0,"PlayMethod":"DirectPlay"}`); rec.Code != 204 {
			t.Fatalf("report %s = %d", ps, rec.Code)
		}
		req := httptest.NewRequestWithContext(context.Background(), "GET", "/Videos/"+fockers+"/stream?static=true&PlaySessionId="+ps, nil)
		rec := httptest.NewRecorder()
		f.h.ServeHTTP(rec, req)
		if rec.Code != 200 || rec.Body.Len() != 10 {
			t.Fatalf("stream %s: %d %q", ps, rec.Code, rec.Body)
		}
	}
	rows, err := d.Queries.ListPlaybackLog(t.Context(), db.ListPlaybackLogParams{Lim: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Bytes != 20 {
		t.Fatalf("rows: %+v", rows)
	}
	var ps string
	if err := testPool.QueryRow(t.Context(), `SELECT play_session_id FROM playback_log WHERE id = $1`, rows[0].ID).Scan(&ps); err != nil || ps != "ps-first" {
		t.Errorf("play session = %q, %v; want the first", ps, err)
	}
}

// Plays merge only on the same user, item and device, while the play is
// still going.
func TestPlaybackLogMergeBounds(t *testing.T) {
	_, d := newIntegrationServer(t)
	seedRecon(t, d)
	a := &api{Deps: Deps{Queries: d.Queries, Log: testutil.Discard()}}
	u, err := d.Queries.GetUserByName(t.Context(), "user1")
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	play := func(item, device string) int64 {
		t.Helper()
		n++
		id, err := a.logPlayback(t.Context(), db.UpsertPlaybackParams{
			UserID: u.ID, ItemID: uuid.MustParse(item), DeviceID: device, PlaySessionID: ptr(fmt.Sprint("ps-", n)), Reopen: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}

	first := play(fockers, "tv")
	if play(fockers, "phone") == first {
		t.Error("two devices share a row")
	}
	if play(mfGhost, "tv") == first {
		t.Error("two items share a row")
	}
	if x, y := play(fockers, ""), play(fockers, ""); x == first || y == first || x == y {
		t.Error("a play without a device id merged")
	}
	if play(fockers, "tv") != first {
		t.Error("the device's new play session didn't join its play")
	}
	execSQL(t, `UPDATE playback_log SET stopped_at = now() - interval '1 minute' WHERE id = $1`, first)
	if play(fockers, "tv") != first {
		t.Error("a play stopped a minute ago didn't take the new session")
	}
	execSQL(t, `UPDATE playback_log SET stopped_at = now() - interval '3 minutes' WHERE id = $1`, first)
	if play(fockers, "tv") == first {
		t.Error("a play stopped 3 minutes ago took the new session")
	}
}

// A stream request with neither a play session nor a token (Infuse opens
// the details' signed Path) takes the item's play session when only one
// user asked PlaybackInfo for it: the row gets its delivery and bytes, and
// the bytes count against the user's quota. With two users it isn't
// guessed.
func TestPlaybackLogAnonymousStreamTakesItemPlay(t *testing.T) {
	_, d := newIntegrationServer(t)
	seedRecon(t, d)
	seedCaptureToken(t, d)
	f := newRemoteFixture(t, d, nil)
	authz := `MediaBrowser Token="` + captureToken + `"`
	u, err := d.Queries.GetUserByName(t.Context(), "user1")
	if err != nil {
		t.Fatal(err)
	}
	anonymous := func() {
		t.Helper()
		req := httptest.NewRequestWithContext(context.Background(), "GET", "/Videos/"+fockers+"/stream?static=true", nil)
		rec := httptest.NewRecorder()
		f.h.ServeHTTP(rec, req)
		if rec.Code != 200 || rec.Body.Len() != 10 {
			t.Fatalf("stream: %d %q", rec.Code, rec.Body)
		}
	}
	used := func() int64 {
		t.Helper()
		n, err := d.Queries.UserBytesSince(t.Context(), db.UserBytesSinceParams{UserID: u.ID, Since: time.Now().Add(-time.Hour)})
		if err != nil {
			t.Fatal(err)
		}
		return n
	}

	rec := call(t, f.h, "POST", "/Items/"+fockers+"/PlaybackInfo", authz, `{"DeviceProfile":{}}`)
	var info struct{ PlaySessionId string }
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &info) != nil || info.PlaySessionId == "" {
		t.Fatalf("PlaybackInfo = %d %s", rec.Code, rec.Body)
	}
	anonymous()
	rows, err := d.Queries.ListPlaybackLog(t.Context(), db.ListPlaybackLogParams{Lim: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Delivery != deliveryProxied || rows[0].Bytes != 10 || rows[0].UserID == nil || *rows[0].UserID != u.ID {
		t.Fatalf("rows: %+v", rows)
	}
	if n := used(); n != 10 {
		t.Errorf("user's bytes = %d, want 10", n)
	}

	// Another user's play of the item: which one streams is a guess now.
	a := &api{Deps: Deps{Cache: d.Cache, Log: testutil.Discard()}}
	a.rememberItemPlay(t.Context(), uuid.MustParse(fockers), uuid.New(), "ps-someone-else")
	anonymous()
	rows, err = d.Queries.ListPlaybackLog(t.Context(), db.ListPlaybackLogParams{Lim: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Bytes != 10 {
		t.Errorf("rows after an ambiguous stream: %+v", rows)
	}
	if n := used(); n != 10 {
		t.Errorf("user's bytes = %d after an ambiguous stream, want 10", n)
	}
}
