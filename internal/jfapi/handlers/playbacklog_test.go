package handlers

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/sysadmin/blockbustr/internal/cache"
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
	// What PlaybackInfo leaves for the stream requests of its play sessions.
	for _, ps := range []string{"ps-proxied", "ps-redirected"} {
		if err := d.Cache.SetJSON(t.Context(), cache.PlaySessionKey(ps), PlaySession{UserID: u.ID, DeviceID: "capture-device", ItemID: uuid.MustParse(fockers)}, cache.PlaySessionTTL); err != nil {
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
