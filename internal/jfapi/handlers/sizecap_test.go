package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/sysadmin/blockbustr/internal/jfapi/dto"
	"github.com/sysadmin/blockbustr/internal/resolve"
	"github.com/sysadmin/blockbustr/internal/stremio"
	"github.com/sysadmin/blockbustr/internal/testutil"
)

// A user's MaxFileSizeGB caps the addon versions they're offered and able
// to play by size, at every height: a 30 GB plan keeps a 25 GB 4K but not
// an 80 GB one, a 100 GB plan keeps both, and a version of unknown size is
// offered to both.
func TestCatalogVersionsSizeCapped(t *testing.T) {
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("bytes")) }))
	t.Cleanup(cdn.Close)
	const gb = 1 << 30
	fs := &fakeStreams{offers: map[string][]stremio.Offer{"movie/tt0133093": {
		{Addon: "A", Stream: stremio.Stream{Name: "4k", URL: cdn.URL + "/big.mkv", Hints: stremio.StreamHints{VideoSize: 80 * gb}}},
		{Addon: "A", Stream: stremio.Stream{Name: "4k", URL: cdn.URL + "/small.mkv", Hints: stremio.StreamHints{VideoSize: 25 * gb}}},
		{Addon: "A", Stream: stremio.Stream{Name: "1080p", URL: cdn.URL + "/hd.mkv", Hints: stremio.StreamHints{VideoSize: 10 * gb}}},
		{Addon: "A", Stream: stremio.Stream{Name: "1080p", URL: cdn.URL + "/unknown.mkv"}},
	}}}
	sf := newSyncFixture(t, func(d *Deps) {
		d.Streams = fs
		d.Resolver = &resolve.Resolver{Cache: d.Cache, Log: testutil.Discard()}
	})
	sf.syncAll(t)
	matrix := sf.children(t, sf.views(t)["Popular Movies"].Id)[0]
	itemID, err := dto.ParseID(matrix.Id)
	if err != nil {
		t.Fatal(err)
	}
	big := dto.IDFromUUID(uuid.NewSHA1(itemID.UUID(), []byte("url:"+cdn.URL+"/big.mkv"))).String()
	admin := `MediaBrowser Token="` + captureToken + `"`
	newUser := func(name, policy string) string {
		t.Helper()
		rec := call(t, sf.h, "POST", "/Users/New", admin, `{"Name":"`+name+`","Password":"pw"}`)
		var u struct{ Id string }
		if json.Unmarshal(rec.Body.Bytes(), &u) != nil || u.Id == "" {
			t.Fatalf("create %s: %d %s", name, rec.Code, rec.Body)
		}
		if rec := call(t, sf.h, "POST", "/Users/"+u.Id+"/Policy", admin, policy); rec.Code != 204 {
			t.Fatalf("policy %s: %d %s", name, rec.Code, rec.Body)
		}
		return u.Id
	}
	basicID := newUser("basic", `{"MaxFileSizeGB":30,"EnableMediaPlayback":true}`)
	newUser("premium", `{"MaxFileSizeGB":100,"EnableMediaPlayback":true}`)
	basic, premium := userClient(t, sf, "basic", "pw"), userClient(t, sf, "premium", "pw")

	var u struct{ Policy struct{ MaxFileSizeGB int } }
	if rec := call(t, sf.h, "GET", "/Users/"+basicID, admin, ""); json.Unmarshal(rec.Body.Bytes(), &u) != nil || u.Policy.MaxFileSizeGB != 30 {
		t.Errorf("stored cap reads back as %d: %s", u.Policy.MaxFileSizeGB, rec.Body)
	}

	if names := versionNames(t, sf.h, matrix.Id, premium, `{}`); len(names) != 4 || !strings.Contains(strings.Join(names, "|"), "80.0 GB") {
		t.Errorf("premium's versions: %v", names)
	}
	rec := call(t, sf.h, "POST", "/Items/"+matrix.Id+"/PlaybackInfo", basic, `{}`)
	var pb playbackSources
	if json.Unmarshal(rec.Body.Bytes(), &pb) != nil || len(pb.MediaSources) != 3 {
		t.Fatalf("basic's PlaybackInfo: %d %s", rec.Code, rec.Body)
	}
	if names := versionNames(t, sf.h, matrix.Id, basic, `{}`); counted(names, "2160p") != 1 {
		t.Errorf("basic should keep the 25 GB 4K: %v", names)
	}
	if code := call(t, sf.h, "GET", "/Videos/"+matrix.Id+"/stream?static=true&MediaSourceId="+big+"&PlaySessionId="+pb.PlaySessionId, "", "").Code; code != 404 {
		t.Errorf("80 GB stream in basic's play session: %d", code)
	}
	if rec := call(t, sf.h, "GET", "/Items/"+matrix.Id+"/Download?mediaSourceId="+big, basic, ""); rec.Code != 404 {
		t.Errorf("basic's 80 GB download: %d", rec.Code)
	}
	for _, n := range versionNames(t, sf.h, matrix.Id, basic, `{"MediaSourceId":"`+big+`"}`) {
		if strings.Contains(n, "80.0 GB") {
			t.Errorf("80 GB offered to basic asking for it: %q", n)
		}
	}
}
