package handlers

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/sysadmin/blockbustr/internal/store/pg/db"
)

func TestResumePoint(t *testing.T) {
	const hour = 36_000_000_000 // ticks
	for _, c := range []struct {
		position, runtime int64
		pos               int64
		finished, touch   bool
	}{
		{hour / 2, hour, hour / 2, false, true},        // half way: resume there
		{hour * 91 / 100, hour, 0, true, true},         // past 90%: played
		{hour * 3 / 100, hour, 0, false, false},        // under 5%: nothing kept
		{2_000_000_000, 2_400_000_000, 0, false, true}, // 200s of a 4-minute clip: no resume point
		{2_300_000_000, 2_400_000_000, 0, true, true},  // ...but finishing it counts
		{5_000_000_000, 0, 5_000_000_000, false, true}, // unknown runtime: keep the position
		{0, 0, 0, false, false},
	} {
		pos, finished, touch := resumePoint(c.position, c.runtime)
		if pos != c.pos || finished != c.finished || touch != c.touch {
			t.Errorf("%d of %d: %d %v %v, want %d %v %v", c.position, c.runtime, pos, finished, touch, c.pos, c.finished, c.touch)
		}
	}
}

func TestSessionReportsContract(t *testing.T) {
	h, d := newIntegrationServer(t)
	seedRecon(t, d)
	seedCaptureToken(t, d)
	for _, ep := range []string{"POST /Sessions/Capabilities/Full", "POST /Sessions/Playing", "POST /Sessions/Playing/Progress", "POST /Sessions/Playing/Stopped"} {
		checkContractOn(t, h, capturesFor(t, ep))
	}
}

// GET /Sessions has Jellyfin's shape for an idle and a playing session.
func TestSessionsListContract(t *testing.T) {
	h, d := newIntegrationServer(t)
	seedRecon(t, d)
	seedCaptureToken(t, d)
	authz := `MediaBrowser Token="` + captureToken + `"`
	// Streamyfin's own capabilities (with its app store and icon URLs).
	var caps capture
	for _, c := range capturesFor(t, "POST /Sessions/Capabilities/Full") {
		if strings.Contains(string(c.Request.Body), "AppStoreUrl") {
			caps = c
		}
	}
	if rec := call(t, h, "POST", "/Sessions/Capabilities/Full", authz, string(caps.Request.Body)); rec.Code != 204 {
		t.Fatal(rec.Code)
	}

	var idle, playing []capture
	for _, c := range capturesFor(t, "GET /Sessions") {
		var b []map[string]any
		_ = json.Unmarshal(c.Response.Body, &b)
		switch {
		case len(b) > 0 && b[0]["NowPlayingItem"] == nil:
			idle = append(idle, c)
		case len(b) > 0:
			playing = append(playing, c)
		}
	}
	if len(idle) == 0 || len(playing) == 0 {
		t.Fatalf("captures: %d idle, %d playing first", len(idle), len(playing))
	}
	// LastPausedDate: only once the session has paused (checked in
	// TestPlaybackReporting); the captures differ in their history.
	checkContractOn(t, h, idle, "$[*].LastPausedDate")

	// Now playing: the session carries Jellyfin's PlayState and item shape.
	call(t, h, "POST", "/Sessions/Playing", authz, `{"ItemId":"`+mkii+`","MediaSourceId":"`+mkii+`","AudioStreamIndex":1,"PositionTicks":0,"PlayMethod":"DirectPlay","CanSeek":true,"VolumeLevel":60}`)
	call(t, h, "POST", "/Sessions/Playing/Progress", authz, `{"ItemId":"`+mkii+`","MediaSourceId":"`+mkii+`","AudioStreamIndex":1,"PositionTicks":2817810000,"IsPaused":false,"PlayMethod":"DirectPlay","CanSeek":true,"VolumeLevel":60}`)
	// The capture's playing session is Jellyfin Android's, which sent no
	// capabilities: replay as that client.
	checkContractOn(t, h, playing[:1], "$[*].NowPlayingItem.MediaStreams[*].Score", "$[*].LastPausedDate",
		"$[*].Capabilities.AppStoreUrl", "$[*].Capabilities.IconUrl")
}

type sessionView struct {
	Id, DeviceId, Client, UserName string
	PlayState                      struct {
		PositionTicks    int64
		IsPaused         bool
		AudioStreamIndex *int
		MediaSourceId    string
	}
	NowPlayingItem *struct{ Id, Name string }
}

func TestPlaybackReporting(t *testing.T) {
	h, d := newIntegrationServer(t)
	seedRecon(t, d)
	seedCaptureToken(t, d)
	authz := `MediaBrowser Token="` + captureToken + `"`
	runtime := int64(63585920000) // Mortal Kombat II
	itoa := func(n int64) string { return strconv.FormatInt(n, 10) }
	state := func() db.UserDatum {
		t.Helper()
		uds, err := d.Queries.ListUserData(t.Context(), db.ListUserDataParams{UserID: uuid.MustParse(captureUserID), ItemIds: []uuid.UUID{uuid.MustParse(mkii)}})
		if err != nil {
			t.Fatal(err)
		}
		if len(uds) == 0 {
			return db.UserDatum{}
		}
		return uds[0]
	}
	report := func(path, body string) {
		t.Helper()
		if rec := call(t, h, "POST", path, authz, body); rec.Code != 204 {
			t.Fatalf("%s = %d %s", path, rec.Code, rec.Body)
		}
	}
	execSQL(t, `DELETE FROM user_data WHERE item_id = $1`, mkii)

	report("/Sessions/Playing", `{"itemId":"`+mkii+`","positionTicks":0,"audioStreamIndex":1}`) // camelCase like Streamyfin
	third := runtime / 3
	report("/Sessions/Playing/Progress", `{"ItemId":"`+mkii+`","PositionTicks":`+itoa(third)+`}`)
	if ud := state(); ud.PlaybackPositionTicks != 0 {
		t.Errorf("progress within 30s isn't saved: %+v", ud)
	}
	var sessions []sessionView
	getJSON(t, h, "/Sessions?activeWithinSeconds=360", &sessions)
	if len(sessions) != 1 || sessions[0].NowPlayingItem == nil || sessions[0].NowPlayingItem.Name != "Mortal Kombat II" ||
		sessions[0].PlayState.PositionTicks != third || sessions[0].PlayState.AudioStreamIndex == nil {
		t.Fatalf("sessions while playing: %+v", sessions)
	}
	// Pausing saves at once, with the chosen track, and is remembered.
	report("/Sessions/Playing/Progress", `{"ItemId":"`+mkii+`","PositionTicks":`+itoa(third)+`,"IsPaused":true,"AudioStreamIndex":1}`)
	var paused []struct{ LastPausedDate *string }
	getJSON(t, h, "/Sessions", &paused)
	if len(paused) != 1 || paused[0].LastPausedDate == nil {
		t.Errorf("LastPausedDate after a pause: %+v", paused)
	}
	if ud := state(); ud.PlaybackPositionTicks != third || ud.Played || ud.LastPlayedAt == nil || ud.AudioStreamIdx == nil || *ud.AudioStreamIdx != 1 {
		t.Errorf("paused: %+v", ud)
	}
	var resume struct{ Items []struct{ Name string } }
	getJSON(t, h, "/UserItems/Resume", &resume)
	if len(resume.Items) == 0 || resume.Items[0].Name != "Mortal Kombat II" {
		t.Errorf("Continue Watching: %+v", resume.Items)
	}

	// A failed stop changes nothing; a stop past 90% marks it played.
	report("/Sessions/Playing/Stopped", `{"ItemId":"`+mkii+`","PositionTicks":`+itoa(runtime-1)+`,"Failed":true}`)
	if ud := state(); ud.Played {
		t.Errorf("failed stop marked played: %+v", ud)
	}
	report("/Sessions/Playing", `{"ItemId":"`+mkii+`"}`)
	report("/Sessions/Playing/Stopped", `{"ItemId":"`+mkii+`","PositionTicks":`+itoa(runtime*95/100)+`}`)
	if ud := state(); !ud.Played || ud.PlayCount != 1 || ud.PlaybackPositionTicks != 0 {
		t.Errorf("finished: %+v", ud)
	}
	var afterStop []sessionView // fresh: Unmarshal keeps fields absent from the new JSON
	getJSON(t, h, "/Sessions?activeWithinSeconds=360", &afterStop)
	if len(afterStop) != 1 || afterStop[0].NowPlayingItem != nil {
		t.Errorf("after stop: %+v", afterStop)
	}

	// Legacy query-string reporting; under 5% keeps no resume point.
	report("/Users/"+strings.ReplaceAll(captureUserID, "-", "")+"/PlayingItems/"+mkii+"/Progress?positionTicks="+itoa(runtime/2)+"&isPaused=true", "")
	if ud := state(); ud.PlaybackPositionTicks != runtime/2 || !ud.Played || ud.PlayCount != 1 {
		t.Errorf("legacy progress (rewatching keeps played): %+v", ud)
	}
	if rec := call(t, h, "DELETE", "/PlayingItems/"+mkii+"?positionTicks="+itoa(runtime/50), authz, ""); rec.Code != 204 {
		t.Fatalf("legacy stop = %d", rec.Code)
	}
	if ud := state(); ud.PlaybackPositionTicks != 0 || ud.PlayCount != 1 {
		t.Errorf("stopped at 2%%: %+v", ud)
	}

	// Unknown items and bad bodies still answer 204.
	report("/Sessions/Playing/Progress", `{"ItemId":"00000000000000000000000000000042","PositionTicks":5}`)
	report("/Sessions/Playing/Stopped", `{not json`)
	report("/Sessions/Playing/Ping?playSessionId=x", "")
}

func TestSessionsVisibility(t *testing.T) {
	h, d := newIntegrationServer(t)
	seedCaptureToken(t, d)
	if _, err := d.Queries.CreateUser(t.Context(), db.CreateUserParams{Name: "kid"}); err != nil {
		t.Fatal(err)
	}
	rec := call(t, h, "POST", "/Users/AuthenticateByName", `MediaBrowser Client="KidApp", DeviceId="kid-1", Device="Tablet", Version="1"`, `{"Username":"kid","Pw":""}`)
	var login struct{ AccessToken string }
	_ = json.Unmarshal(rec.Body.Bytes(), &login)
	kid := `MediaBrowser Token="` + login.AccessToken + `"`
	call(t, h, "GET", "/Users/Me", kid, "") // any request counts as activity
	var sessions []sessionView
	getJSON(t, h, "/Sessions", &sessions) // the admin's own request is activity too
	if len(sessions) != 2 {
		t.Errorf("admin sees every session: %+v", sessions)
	}
	rec = call(t, h, "GET", "/Sessions", kid, "")
	_ = json.Unmarshal(rec.Body.Bytes(), &sessions)
	if len(sessions) != 1 || sessions[0].DeviceId != "kid-1" || sessions[0].Client != "KidApp" || sessions[0].UserName != "kid" {
		t.Errorf("a user sees only their own: %+v", sessions)
	}
}
