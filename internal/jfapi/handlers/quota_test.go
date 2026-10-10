package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sysadmin/blockbustr/internal/resolve"
	"github.com/sysadmin/blockbustr/internal/store/pg/db"
	"github.com/sysadmin/blockbustr/internal/stremio"
	"github.com/sysadmin/blockbustr/internal/testutil"
)

// A user's MonthlyDataGB caps what is proxied for them per calendar month:
// their streams count against it, last month's bytes don't, and once it's
// used up PlaybackInfo answers RateLimitExceeded and downloads 403, while
// users without a quota play on.
func TestMonthlyDataQuota(t *testing.T) {
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("bytes")) }))
	t.Cleanup(cdn.Close)
	fs := &fakeStreams{offers: map[string][]stremio.Offer{"movie/tt0133093": {
		{Addon: "A", Stream: stremio.Stream{Name: "1080p", URL: cdn.URL + "/f.mkv"}},
	}}}
	var q *db.Queries
	sf := newSyncFixture(t, func(d *Deps) {
		d.Streams = fs
		d.Resolver = &resolve.Resolver{Cache: d.Cache, Log: testutil.Discard()}
		q = d.Queries
	})
	sf.syncAll(t)
	matrix := sf.children(t, sf.views(t)["Popular Movies"].Id)[0]
	admin := `MediaBrowser Token="` + captureToken + `"`
	rec := call(t, sf.h, "POST", "/Users/New", admin, `{"Name":"guest","Password":"pw"}`)
	var g struct{ Id string }
	if json.Unmarshal(rec.Body.Bytes(), &g) != nil || g.Id == "" {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	if rec := call(t, sf.h, "POST", "/Users/"+g.Id+"/Policy", admin, `{"MonthlyDataGB":1,"EnableMediaPlayback":true}`); rec.Code != 204 {
		t.Fatalf("policy: %d %s", rec.Code, rec.Body)
	}
	guestID := uuid.MustParse(g.Id)
	guest := userClient(t, sf, "guest", "pw")
	ctx := context.Background()
	used := func() int64 {
		t.Helper()
		n, err := q.UserBytesSince(ctx, db.UserBytesSinceParams{UserID: guestID, Since: time.Now().AddDate(0, 0, -1)})
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	type playbackInfo struct {
		MediaSources  []struct{ Id string }
		PlaySessionId string
		ErrorCode     string
	}
	playback := func(auth string) playbackInfo {
		t.Helper()
		rec := call(t, sf.h, "POST", "/Items/"+matrix.Id+"/PlaybackInfo", auth, `{}`)
		var pb playbackInfo
		if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &pb) != nil {
			t.Fatalf("PlaybackInfo: %d %s", rec.Code, rec.Body)
		}
		return pb
	}

	pb := playback(guest)
	if pb.ErrorCode != "" || len(pb.MediaSources) == 0 {
		t.Fatalf("guest under quota: %+v", pb)
	}
	code := call(t, sf.h, "GET", "/Videos/"+matrix.Id+"/stream?static=true&MediaSourceId="+pb.MediaSources[0].Id+"&PlaySessionId="+pb.PlaySessionId, "", "").Code
	if code != 200 || used() != int64(len("bytes")) {
		t.Errorf("guest's stream: %d, counted %d bytes", code, used())
	}

	// Last month's bytes don't count.
	start := time.Date(time.Now().UTC().Year(), time.Now().UTC().Month(), 1, 0, 0, 0, 0, time.UTC)
	if err := q.AddUserBytes(ctx, db.AddUserBytesParams{UserID: guestID, Hour: start.Add(-time.Hour), Bytes: 5 << 30}); err != nil {
		t.Fatal(err)
	}
	if pb := playback(guest); pb.ErrorCode != "" {
		t.Errorf("last month's bytes refused the guest: %+v", pb)
	}

	if err := q.AddUserBytes(ctx, db.AddUserBytesParams{UserID: guestID, Hour: time.Now().UTC().Truncate(time.Hour), Bytes: 1 << 30}); err != nil {
		t.Fatal(err)
	}
	if pb := playback(guest); pb.ErrorCode != "RateLimitExceeded" || len(pb.MediaSources) != 0 {
		t.Errorf("guest over quota: %+v", pb)
	}
	if rec := call(t, sf.h, "GET", "/Items/"+matrix.Id+"/Download", guest, ""); rec.Code != 403 {
		t.Errorf("guest's download over quota: %d", rec.Code)
	}
	if pb := playback(admin); pb.ErrorCode != "" || len(pb.MediaSources) == 0 {
		t.Errorf("admin without a quota: %+v", pb)
	}

	var usage struct {
		Month string
		Users []struct {
			UserID uuid.UUID
			Bytes  int64
		}
	}
	rec = call(t, sf.h, "GET", "/blockbustr/usage", admin, "")
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &usage) != nil || usage.Month != start.Format("2006-01") {
		t.Fatalf("usage: %d %s", rec.Code, rec.Body)
	}
	found := false
	for _, u := range usage.Users {
		if u.UserID == guestID {
			found = u.Bytes == 1<<30+int64(len("bytes"))
		}
	}
	if !found {
		t.Errorf("guest's usage this month: %s", rec.Body)
	}
	if rec := call(t, sf.h, "GET", "/blockbustr/usage", guest, ""); rec.Code != 403 {
		t.Errorf("usage as a non-admin: %d", rec.Code)
	}
}
