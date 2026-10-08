package handlers

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/sysadmin/blockbustr/internal/store/pg/db"
)

func TestDownload(t *testing.T) {
	h, d := newIntegrationServer(t)
	seedRecon(t, d)
	seedCaptureToken(t, d)
	user := `MediaBrowser Token="` + captureToken + `"`
	const mkii = "1f77a87a9603bff7d1589d1e076f060c"
	p := filepath.Join(t.TempDir(), "Mortal Kombat II.mp4")
	if err := os.WriteFile(p, []byte("0123456789"), 0o644); err != nil {
		t.Fatal(err)
	}
	execSQL(t, `UPDATE media_sources SET path_or_url = $1 WHERE item_id = $2`, p, mkii)
	canDownload := func() bool {
		t.Helper()
		var it struct{ CanDownload bool }
		rec := call(t, h, "GET", "/Items/"+mkii+"?Fields=CanDownload", user, "")
		if err := json.Unmarshal(rec.Body.Bytes(), &it); rec.Code != 200 || err != nil {
			t.Fatalf("item: %d %v", rec.Code, err)
		}
		return it.CanDownload
	}

	// Allowed by the default policy: the file as it is, as an attachment.
	if !canDownload() {
		t.Error("CanDownload false under the default policy")
	}
	rec := call(t, h, "GET", "/Items/"+mkii+"/Download", user, "")
	if rec.Code != 200 || rec.Body.String() != "0123456789" {
		t.Errorf("download: %d %q", rec.Code, rec.Body)
	}
	if got, want := rec.Header().Get("Content-Disposition"), `attachment; filename="Mortal Kombat II.mp4"`; got != want {
		t.Errorf("Content-Disposition %q, want %q", got, want)
	}
	if rec := call(t, h, "GET", "/Items/"+mkii+"/Download", "", ""); rec.Code != 401 {
		t.Errorf("without a token: %d", rec.Code)
	}
	if rec := call(t, h, "GET", "/Items/"+mfGhost+"/Download", user, ""); rec.Code != 404 {
		t.Errorf("a series: %d", rec.Code)
	}

	// Turned off in the policy: refused, and clients hide the button.
	if rec := call(t, h, "POST", "/Users/"+captureUserID+"/Policy", user, `{"EnableContentDownloading":false}`); rec.Code != 204 {
		t.Fatalf("policy: %d %s", rec.Code, rec.Body)
	}
	if canDownload() {
		t.Error("CanDownload true with downloading off")
	}
	if rec := call(t, h, "GET", "/Items/"+mkii+"/Download", user, ""); rec.Code != 403 {
		t.Errorf("download with downloading off: %d", rec.Code)
	}
}

func TestDownloadName(t *testing.T) {
	two, three, year := int32(2), int32(3), int32(1999)
	mkv, probed := "MKV", "mov,mp4,m4a"
	for _, c := range []struct {
		it   db.Item
		src  db.MediaSource
		want string
	}{
		{db.Item{Name: "Ignored"}, db.MediaSource{Protocol: "File", PathOrUrl: "/m/The Matrix (1999).mkv"}, "The Matrix (1999).mkv"},
		{db.Item{Type: "Movie", Name: "The Matrix", ProductionYear: &year}, db.MediaSource{IsRemote: true, Container: &mkv}, "The Matrix (1999).mkv"},
		{db.Item{Type: "Episode", Name: "What/Now?", ParentIndexNumber: &two, IndexNumber: &three}, db.MediaSource{IsRemote: true, Container: &probed}, "S02E03 - What_Now_.mkv"},
	} {
		if got := downloadName(c.it, c.src); got != c.want {
			t.Errorf("downloadName(%s) = %q, want %q", c.it.Name, got, c.want)
		}
	}
}
