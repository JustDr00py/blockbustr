package handlers

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"github.com/sysadmin/blockbustr/internal/store/pg/db"
)

const tvAuth = `MediaBrowser Client="Jellyfin for Android", Device="Living room TV", DeviceId="tv-1", Version="2.7.3"`

type qcResult struct {
	Authenticated          bool
	Secret, Code, DeviceId string
	DeviceName, AppName    string
}

func TestQuickConnectContract(t *testing.T) {
	h, d := newIntegrationServer(t)
	seedCaptureToken(t, d)
	checkContractOn(t, h, capturesFor(t, "GET /QuickConnect/Enabled"))
	initiates := checkContractOn(t, h, capturesFor(t, "POST /QuickConnect/Initiate"))
	first := initiates[0].(map[string]any)
	secret, code := first["Secret"].(string), first["Code"].(string)

	// Connect and AuthenticateWithQuickConnect replayed with our secret
	// (the captured one was Jellyfin's).
	connects := capturesFor(t, "GET /QuickConnect/Connect")[:1]
	connects[0].Request.Query = map[string][]string{"Secret": {secret}}
	checkContractOn(t, h, connects)

	if rec := call(t, h, "POST", "/QuickConnect/Authorize?code="+code, `MediaBrowser Token="`+captureToken+`"`, ""); rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != "true" {
		t.Fatalf("authorize: %d %s", rec.Code, rec.Body)
	}
	logins := capturesFor(t, "POST /Users/AuthenticateWithQuickConnect")
	logins[0].Request.Body = json.RawMessage(`{"Secret":"` + secret + `"}`)
	checkContractOn(t, h, logins)
}

func TestQuickConnectFlow(t *testing.T) {
	h, d := newIntegrationServer(t)
	seedCaptureToken(t, d)
	admin := `MediaBrowser Token="` + captureToken + `"`

	rec := call(t, h, "POST", "/QuickConnect/Initiate", tvAuth, "")
	var req qcResult
	if err := json.Unmarshal(rec.Body.Bytes(), &req); err != nil || rec.Code != 200 {
		t.Fatalf("initiate: %d %s", rec.Code, rec.Body)
	}
	if !regexp.MustCompile(`^[0-9A-F]{64}$`).MatchString(req.Secret) || !regexp.MustCompile(`^\d{6}$`).MatchString(req.Code) ||
		req.Authenticated || req.DeviceId != "tv-1" || req.DeviceName != "Living room TV" || req.AppName != "Jellyfin for Android" {
		t.Fatalf("initiate result: %+v", req)
	}
	poll := func() qcResult {
		t.Helper()
		var r qcResult
		getJSON(t, h, "/QuickConnect/Connect?secret="+strings.ToLower(req.Secret), &r) // any case
		return r
	}
	if poll().Authenticated {
		t.Fatal("authenticated before approval")
	}
	// Not approved yet: no login.
	if rec := call(t, h, "POST", "/Users/AuthenticateWithQuickConnect", tvAuth, `{"Secret":"`+req.Secret+`"}`); rec.Code != 401 {
		t.Errorf("redeem before approval = %d", rec.Code)
	}
	wrong := "000000"
	if req.Code == wrong {
		wrong = "000001"
	}
	if rec := call(t, h, "POST", "/QuickConnect/Authorize?code="+wrong, admin, ""); rec.Code != 404 {
		t.Errorf("wrong code = %d", rec.Code)
	}
	if rec := call(t, h, "POST", "/QuickConnect/Authorize?code="+req.Code, admin, ""); rec.Code != 200 {
		t.Fatalf("authorize = %d", rec.Code)
	}
	if !poll().Authenticated {
		t.Fatal("not authenticated after approval")
	}

	// The TV logs in as the approving user, on its own device.
	rec = call(t, h, "POST", "/Users/AuthenticateWithQuickConnect", tvAuth, `{"secret":"`+req.Secret+`"}`) // camelCase body
	var login struct {
		AccessToken string
		User        struct{ Name string }
		SessionInfo struct{ DeviceId, DeviceName string }
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &login); err != nil || rec.Code != 200 || login.User.Name != "user1" ||
		login.SessionInfo.DeviceId != "tv-1" || login.SessionInfo.DeviceName != "Living room TV" {
		t.Fatalf("login: %d %s", rec.Code, rec.Body)
	}
	if rec := call(t, h, "GET", "/Users/Me", `MediaBrowser Token="`+login.AccessToken+`"`, ""); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"Name":"user1"`) {
		t.Errorf("the new token: %d %s", rec.Code, rec.Body)
	}
	// Single use: the request is gone.
	if rec := call(t, h, "POST", "/Users/AuthenticateWithQuickConnect", tvAuth, `{"Secret":"`+req.Secret+`"}`); rec.Code != 401 {
		t.Errorf("second redeem = %d", rec.Code)
	}
	if rec := call(t, h, "GET", "/QuickConnect/Connect?secret="+req.Secret, "", ""); rec.Code != 404 {
		t.Errorf("poll after use = %d", rec.Code)
	}
}

func TestQuickConnectAccess(t *testing.T) {
	h, d := newIntegrationServer(t)
	seedCaptureToken(t, d)
	if rec := call(t, h, "POST", "/QuickConnect/Initiate", "", ""); rec.Code != 400 {
		t.Errorf("initiate without device info = %d", rec.Code)
	}
	rec := call(t, h, "POST", "/QuickConnect/Initiate", tvAuth, "")
	var req qcResult
	_ = json.Unmarshal(rec.Body.Bytes(), &req)
	if rec := call(t, h, "POST", "/QuickConnect/Authorize?code="+req.Code, "", ""); rec.Code != 401 {
		t.Errorf("anonymous approval = %d", rec.Code)
	}
	// A non-admin can approve for themselves, not for someone else.
	kidUser, err := d.Queries.CreateUser(t.Context(), db.CreateUserParams{Name: "kid"})
	if err != nil {
		t.Fatal(err)
	}
	rec = call(t, h, "POST", "/Users/AuthenticateByName", `MediaBrowser Client="C", DeviceId="kid-1", Device="P", Version="1"`, `{"Username":"kid","Pw":""}`)
	var login struct{ AccessToken string }
	_ = json.Unmarshal(rec.Body.Bytes(), &login)
	kid := `MediaBrowser Token="` + login.AccessToken + `"`
	admin, err := d.Queries.GetUserByName(t.Context(), "user1")
	if err != nil {
		t.Fatal(err)
	}
	if rec := call(t, h, "POST", "/QuickConnect/Authorize?code="+req.Code+"&userId="+strings.ReplaceAll(admin.ID.String(), "-", ""), kid, ""); rec.Code != 403 {
		t.Errorf("non-admin approving as the admin = %d", rec.Code)
	}
	if rec := call(t, h, "POST", "/QuickConnect/Authorize?code="+req.Code, kid, ""); rec.Code != 200 {
		t.Fatalf("approving for themselves = %d", rec.Code)
	}
	rec = call(t, h, "POST", "/Users/AuthenticateWithQuickConnect", tvAuth, `{"Secret":"`+req.Secret+`"}`)
	var res struct{ User struct{ Id string } }
	_ = json.Unmarshal(rec.Body.Bytes(), &res)
	if rec.Code != 200 || res.User.Id != strings.ReplaceAll(kidUser.ID.String(), "-", "") {
		t.Errorf("logs in as the approver: %d %s", rec.Code, rec.Body)
	}
	if rec := call(t, h, "POST", "/Users/AuthenticateWithQuickConnect", tvAuth, `{}`); rec.Code != 400 {
		t.Errorf("no secret = %d", rec.Code)
	}
	if rec := call(t, h, "GET", "/QuickConnect/Connect?secret=nope", "", ""); rec.Code != 404 {
		t.Errorf("unknown secret = %d", rec.Code)
	}
}
