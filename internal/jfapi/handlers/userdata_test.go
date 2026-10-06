package handlers

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/sysadmin/blockbustr/internal/store/pg/db"
)

type userDataResp struct {
	Played                bool
	PlayCount             int
	PlaybackPositionTicks int64
	IsFavorite            bool
	LastPlayedDate        *string
	UnplayedItemCount     *int
	Rating                *float64
	ItemId                string
}

// sendUserData calls a user data endpoint as the capture user and decodes
// the UserItemDataDto it answers with.
func sendUserData(t *testing.T, h http.Handler, method, path, body string) userDataResp {
	t.Helper()
	rec := call(t, h, method, path, `MediaBrowser Token="`+captureToken+`"`, body)
	if rec.Code != 200 {
		t.Fatalf("%s %s = %d %s", method, path, rec.Code, rec.Body)
	}
	var ud userDataResp
	if err := json.Unmarshal(rec.Body.Bytes(), &ud); err != nil {
		t.Fatal(err)
	}
	return ud
}

func TestUserDataFavoriteContract(t *testing.T) {
	h, d := newIntegrationServer(t)
	seedRecon(t, d)
	seedCaptureToken(t, d)
	for _, ep := range []string{"POST /Users/{id}/FavoriteItems/{id}", "DELETE /Users/{id}/FavoriteItems/{id}"} {
		caps := capturesFor(t, ep)
		for _, c := range caps {
			var want, got userDataResp
			_ = json.Unmarshal(c.Response.Body, &want)
			_ = json.Unmarshal(replay(t, h, c).Body.Bytes(), &got)
			if got.IsFavorite != want.IsFavorite || got.ItemId != want.ItemId {
				t.Errorf("%s: %+v, Jellyfin %+v", c.Name, got, want)
			}
		}
		// LastPlayedDate: the movie was played during that capture session,
		// after the state the recon seed holds (see itemIgnores).
		checkContractOn(t, h, caps, "$.LastPlayedDate")
	}
}

// episodes returns MF Ghost's episode ids in aired order.
func episodes(t *testing.T) []string {
	t.Helper()
	rows, err := testPool.Query(t.Context(), `SELECT replace(id::text, '-', '') FROM items WHERE type = 'Episode' ORDER BY parent_index_number, index_number`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		_ = rows.Scan(&id)
		ids = append(ids, id)
	}
	return ids
}

func TestMarkPlayed(t *testing.T) {
	h, d := newIntegrationServer(t)
	seedRecon(t, d)
	seedCaptureToken(t, d)
	execSQL(t, `DELETE FROM user_data`)
	eps := episodes(t)
	if len(eps) < 2 {
		t.Fatal("need episodes")
	}

	execSQL(t, `INSERT INTO user_data (user_id, item_id, playback_position_ticks) VALUES ($1, $2, 5000)`, captureUserID, eps[0])
	ud := sendUserData(t, h, "POST", "/UserPlayedItems/"+eps[0], "")
	if !ud.Played || ud.PlayCount != 1 || ud.PlaybackPositionTicks != 0 || ud.LastPlayedDate == nil {
		t.Errorf("played: %+v", ud)
	}
	if ud = sendUserData(t, h, "POST", "/UserPlayedItems/"+eps[0], ""); ud.PlayCount != 1 {
		t.Errorf("ticking played again is not a new play: %+v", ud)
	}
	ud = sendUserData(t, h, "POST", "/UserPlayedItems/"+eps[0]+"?datePlayed=2026-01-02T03:04:05.000Z", "")
	if ud.PlayCount != 2 || ud.LastPlayedDate == nil || !strings.HasPrefix(*ud.LastPlayedDate, "2026-01-02T03:04:05") {
		t.Errorf("dated play: %+v", ud)
	}

	// A series: every episode, and the series reports none left.
	ud = sendUserData(t, h, "POST", "/Users/"+strings.ReplaceAll(captureUserID, "-", "")+"/PlayedItems/"+mfGhost, "")
	if !ud.Played || ud.UnplayedItemCount == nil || *ud.UnplayedItemCount != 0 {
		t.Errorf("series played: %+v", ud)
	}
	if ud = sendUserData(t, h, "GET", "/UserItems/"+eps[len(eps)-1]+"/UserData", ""); !ud.Played || ud.PlayCount != 1 {
		t.Errorf("last episode: %+v", ud)
	}
	ud = sendUserData(t, h, "DELETE", "/UserPlayedItems/"+mfGhost, "")
	if ud.Played || ud.UnplayedItemCount == nil || *ud.UnplayedItemCount != len(eps) {
		t.Errorf("series unplayed: %+v", ud)
	}
	if ud = sendUserData(t, h, "GET", "/UserItems/"+eps[0]+"/UserData", ""); ud.Played || ud.PlayCount != 0 || ud.LastPlayedDate != nil {
		t.Errorf("episode after unplayed: %+v", ud)
	}
}

func TestFavoritesAndUpdate(t *testing.T) {
	h, d := newIntegrationServer(t)
	seedRecon(t, d)
	seedCaptureToken(t, d)
	execSQL(t, `DELETE FROM user_data`)

	if ud := sendUserData(t, h, "POST", "/UserFavoriteItems/"+mfGhost, ""); !ud.IsFavorite {
		t.Errorf("favourite: %+v", ud)
	}
	var res struct{ Items []struct{ Name string } }
	getJSON(t, h, "/Items?recursive=true&isFavorite=true", &res)
	if len(res.Items) != 1 || res.Items[0].Name != "MF GHOST" {
		t.Errorf("favourites: %+v", res.Items)
	}
	if ud := sendUserData(t, h, "DELETE", "/UserFavoriteItems/"+mfGhost, ""); ud.IsFavorite {
		t.Errorf("unfavourite: %+v", ud)
	}

	// Only the fields sent change (camelCase body).
	ud := sendUserData(t, h, "POST", "/UserItems/"+fockers+"/UserData", `{"playbackPositionTicks":1234,"isFavorite":true,"rating":7.5}`)
	if ud.PlaybackPositionTicks != 1234 || !ud.IsFavorite || ud.Played || ud.Rating == nil || *ud.Rating != 7.5 {
		t.Errorf("update: %+v", ud)
	}
	ud = sendUserData(t, h, "POST", "/UserItems/"+fockers+"/UserData", `{"Played":true,"LastPlayedDate":"2026-03-04T05:06:07Z"}`)
	if !ud.Played || ud.PlaybackPositionTicks != 1234 || !ud.IsFavorite || ud.LastPlayedDate == nil {
		t.Errorf("second update keeps the first: %+v", ud)
	}
}

func TestUserDataErrors(t *testing.T) {
	h, d := newIntegrationServer(t)
	seedRecon(t, d)
	seedCaptureToken(t, d)
	admin := `MediaBrowser Token="` + captureToken + `"`
	for _, p := range []string{"/UserPlayedItems/00000000000000000000000000000042", "/UserFavoriteItems/not-an-id"} {
		if rec := call(t, h, "POST", p, admin, ""); rec.Code != 404 {
			t.Errorf("%s = %d", p, rec.Code)
		}
	}
	if rec := call(t, h, "POST", "/UserPlayedItems/"+fockers+"?datePlayed=yesterday", admin, ""); rec.Code != 400 {
		t.Errorf("bad datePlayed = %d", rec.Code)
	}
	if rec := call(t, h, "POST", "/UserFavoriteItems/"+fockers, "", ""); rec.Code != 401 {
		t.Errorf("anonymous = %d", rec.Code)
	}
	if _, err := d.Queries.CreateUser(t.Context(), db.CreateUserParams{Name: "kid"}); err != nil {
		t.Fatal(err)
	}
	rec := call(t, h, "POST", "/Users/AuthenticateByName", `MediaBrowser Client="C", DeviceId="kid-1", Device="P", Version="1"`, `{"Username":"kid","Pw":""}`)
	var login struct{ AccessToken string }
	_ = json.Unmarshal(rec.Body.Bytes(), &login)
	kid := `MediaBrowser Token="` + login.AccessToken + `"`
	if rec := call(t, h, "POST", "/Users/"+strings.ReplaceAll(captureUserID, "-", "")+"/FavoriteItems/"+fockers, kid, ""); rec.Code != 403 {
		t.Errorf("non-admin writing another user's data = %d", rec.Code)
	}
}
