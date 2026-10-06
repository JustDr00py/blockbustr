package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/sysadmin/blockbustr/internal/events"
	"github.com/sysadmin/blockbustr/internal/jfapi"
	"github.com/sysadmin/blockbustr/internal/store/pg/db"
	"github.com/sysadmin/blockbustr/internal/testutil"
)

type socketFixture struct {
	srv *httptest.Server
	d   Deps
	bus *events.Bus
	hub *Hub
}

func newSocketFixture(t *testing.T) *socketFixture {
	t.Helper()
	_, d := newIntegrationServer(t)
	seedRecon(t, d)
	seedCaptureToken(t, d)
	f := &socketFixture{d: d, bus: &events.Bus{Cache: d.Cache}, hub: NewHub()}
	f.d.Events, f.d.Hub = f.bus, f.hub
	rt := jfapi.NewRouter(testutil.Discard(), jfapi.Options{LegacyAuth: true})
	Register(rt, f.d)
	f.srv = httptest.NewServer(rt)
	t.Cleanup(f.srv.Close)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = f.hub.Run(ctx, f.bus) }()
	time.Sleep(200 * time.Millisecond) // the subscription is up before anything is published
	return f
}

func (f *socketFixture) dial(t *testing.T, token string) *websocket.Conn {
	t.Helper()
	c, resp, err := websocket.Dial(t.Context(), "ws"+strings.TrimPrefix(f.srv.URL, "http")+"/socket?ApiKey="+token, nil)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.CloseNow() })
	return c
}

// next reads messages until one of kind arrives.
func next(t *testing.T, c *websocket.Conn, kind string) map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	for {
		_, data, err := c.Read(ctx)
		if err != nil {
			t.Fatalf("waiting for %s: %v", kind, err)
		}
		var m map[string]any
		_ = json.Unmarshal(data, &m)
		if m["MessageType"] == kind {
			return m
		}
	}
}

// capturedSocketMessage is the first captured server message of kind.
func capturedSocketMessage(t *testing.T, kind string) any {
	t.Helper()
	files, _ := filepath.Glob("../../../testdata/jellyfin/*/*/*-GET-socket.json")
	for _, f := range files {
		if strings.Contains(f, "/_raw/") {
			continue
		}
		raw, _ := os.ReadFile(f)
		var c struct {
			Websocket struct {
				Messages []struct {
					FromClient bool `json:"from_client"`
					Data       map[string]any
				}
			}
		}
		_ = json.Unmarshal(raw, &c)
		for _, m := range c.Websocket.Messages {
			if !m.FromClient && m.Data["MessageType"] == kind {
				return m.Data
			}
		}
	}
	t.Fatalf("no captured %s", kind)
	return nil
}

func TestSocketHandshakeAndKeepAlive(t *testing.T) {
	f := newSocketFixture(t)
	// No token: refused like Jellyfin (403, text).
	req, _ := http.NewRequestWithContext(t.Context(), "GET", f.srv.URL+"/socket", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Errorf("anonymous upgrade = %d", resp.StatusCode)
	}

	c := f.dial(t, captureToken)
	hello := next(t, c, "ForceKeepAlive")
	if hello["Data"] != float64(60) || !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(hello["MessageId"].(string)) {
		t.Errorf("ForceKeepAlive: %v", hello)
	}
	if err := sameShape("$", capturedSocketMessage(t, "ForceKeepAlive"), hello); err != nil {
		t.Error(err)
	}
	if err := c.Write(t.Context(), websocket.MessageText, []byte(`{"MessageType":"KeepAlive"}`)); err != nil {
		t.Fatal(err)
	}
	ka := next(t, c, "KeepAlive")
	if err := sameShape("$", capturedSocketMessage(t, "KeepAlive"), ka); err != nil {
		t.Error(err)
	}
	if f.hub.Connections() != 1 {
		t.Errorf("connections: %d", f.hub.Connections())
	}
}

func TestSocketUserDataChanged(t *testing.T) {
	f := newSocketFixture(t)
	mine := f.dial(t, captureToken)
	next(t, mine, "ForceKeepAlive")

	// Someone else's socket must not hear about it.
	if _, err := f.d.Queries.CreateUser(t.Context(), db.CreateUserParams{Name: "kid"}); err != nil {
		t.Fatal(err)
	}
	rec := call(t, f.srv.Config.Handler, "POST", "/Users/AuthenticateByName", `MediaBrowser Client="C", DeviceId="kid-1", Device="P", Version="1"`, `{"Username":"kid","Pw":""}`)
	var login struct{ AccessToken string }
	_ = json.Unmarshal(rec.Body.Bytes(), &login)
	theirs := f.dial(t, login.AccessToken)
	next(t, theirs, "ForceKeepAlive")

	authz := `MediaBrowser Token="` + captureToken + `"`
	call(t, f.srv.Config.Handler, "POST", "/UserFavoriteItems/"+fockers, authz, "")
	msg := next(t, mine, "UserDataChanged")
	data := msg["Data"].(map[string]any)
	first := data["UserDataList"].([]any)[0].(map[string]any)
	if data["UserId"] != strings.ReplaceAll(captureUserID, "-", "") || first["ItemId"] != fockers || first["IsFavorite"] != true {
		t.Errorf("UserDataChanged: %v", msg)
	}
	// PlayedPercentage/LastPlayedDate depend on the item's state then.
	if err := sameShapeIgnoring("$", capturedSocketMessage(t, "UserDataChanged"), msg, map[string]bool{
		"$.Data.UserDataList[*].PlayedPercentage": true, "$.Data.UserDataList[*].LastPlayedDate": true,
	}); err != nil {
		t.Error(err)
	}

	// An episode lists its season and series too, with their new counts.
	var ep string
	if err := testPool.QueryRow(t.Context(), `SELECT replace(id::text, '-', '') FROM items WHERE type = 'Episode' ORDER BY index_number LIMIT 1`).Scan(&ep); err != nil {
		t.Fatal(err)
	}
	call(t, f.srv.Config.Handler, "POST", "/UserPlayedItems/"+ep, authz, "")
	msg = next(t, mine, "UserDataChanged")
	ids := map[string]bool{}
	for _, u := range msg["Data"].(map[string]any)["UserDataList"].([]any) {
		ids[u.(map[string]any)["ItemId"].(string)] = true
	}
	if len(ids) != 3 || !ids[ep] || !ids[mfGhost] {
		t.Errorf("episode, season and series: %v", ids)
	}

	// The other user's socket got neither.
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	if _, data, err := theirs.Read(ctx); err == nil {
		t.Errorf("other user got %s", data)
	}
}

func TestSocketLibraryChangedAndShutdown(t *testing.T) {
	f := newSocketFixture(t)
	c := f.dial(t, captureToken)
	next(t, c, "ForceKeepAlive")
	var lib, folder uuid.UUID
	if err := testPool.QueryRow(t.Context(), `SELECT library_id, id FROM items WHERE type = 'CollectionFolder' ORDER BY name LIMIT 1`).Scan(&lib, &folder); err != nil {
		t.Fatal(err)
	}
	luca := uuid.MustParse("84088e11eb6351255c08507602d79a0f")
	f.bus.Publish(t.Context(), events.Event{Kind: events.LibraryChanged, Libraries: []uuid.UUID{lib}, Updated: []uuid.UUID{luca}})
	msg := next(t, c, "LibraryChanged")
	data := msg["Data"].(map[string]any)
	if data["ItemsUpdated"].([]any)[0] != "84088e11eb6351255c08507602d79a0f" || data["CollectionFolders"].([]any)[0] != strings.ReplaceAll(folder.String(), "-", "") {
		t.Errorf("LibraryChanged: %v", msg)
	}
	if err := sameShape("$", capturedSocketMessage(t, "LibraryChanged"), msg); err != nil {
		t.Error(err)
	}

	f.hub.Shutdown()
	bye := next(t, c, "ServerShuttingDown")
	if err := sameShape("$", capturedSocketMessage(t, "ServerShuttingDown"), bye); err != nil {
		t.Error(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if _, _, err := c.Read(ctx); err == nil {
		t.Error("socket still open after shutdown")
	}
}
