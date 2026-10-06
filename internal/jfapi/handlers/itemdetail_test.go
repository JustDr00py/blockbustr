package handlers

import (
	"encoding/json"
	"fmt"
	"testing"
)

const mfGhostE1 = "551ae82823de5533467c046377c55afb"

func TestItemDetailValues(t *testing.T) {
	h, d := newIntegrationServer(t)
	want := seedRecon(t, d)[mfGhostE1]
	seedCaptureToken(t, d)
	var got struct {
		Name, SeriesName string
		MediaSources     []struct {
			Id, Path, Protocol         string
			DefaultAudioStreamIndex    int
			DefaultSubtitleStreamIndex int
			MediaStreams               []struct {
				Index                  int
				DisplayTitle, TimeBase string
			}
		}
		MediaStreams []struct{ DisplayTitle string }
	}
	getJSON(t, h, "/Items/"+mfGhostE1, &got)
	if got.Name != "The Challenger from England" || got.SeriesName != "MF GHOST" || len(got.MediaSources) != 1 {
		t.Fatalf("episode: %+v", got)
	}
	ms := got.MediaSources[0]
	if ms.Id != mfGhostE1 || ms.Protocol != "File" || ms.DefaultAudioStreamIndex != 1 || ms.DefaultSubtitleStreamIndex != 3 {
		t.Errorf("source: id %s protocol %s audio %d subtitle %d", ms.Id, ms.Protocol, ms.DefaultAudioStreamIndex, ms.DefaultSubtitleStreamIndex)
	}
	captured := objs(objs(want, "MediaSources")[0], "MediaStreams")
	if len(ms.MediaStreams) != len(captured) || len(got.MediaStreams) != len(captured) {
		t.Fatalf("%d streams, want %d", len(ms.MediaStreams), len(captured))
	}
	for i, s := range ms.MediaStreams {
		if s.DisplayTitle != str(captured[i], "DisplayTitle") || s.TimeBase != "1/1000" {
			t.Errorf("stream %d: %q (%s), want %q", s.Index, s.DisplayTitle, s.TimeBase, str(captured[i], "DisplayTitle"))
		}
	}

	var anc []struct{ Type, Name string }
	getJSON(t, h, "/Items/"+mfGhostE1+"/Ancestors", &anc)
	var chain []string
	for _, a := range anc {
		chain = append(chain, a.Type)
	}
	if fmt.Sprint(chain) != "[Season Series CollectionFolder UserRootFolder]" || anc[1].Name != "MF GHOST" {
		t.Errorf("ancestors: %+v", anc)
	}

	var root struct{ Type, Name string }
	getJSON(t, h, "/Items/"+rootFolderID(d.ServerID).String(), &root)
	if root.Type != "UserRootFolder" || root.Name != "Media Folders" {
		t.Errorf("root: %+v", root)
	}

	auth := `MediaBrowser Token="` + captureToken + `"`
	for _, p := range []string{"/Items/not-an-id", "/Items/00000000000000000000000000000001", "/Items/00000000000000000000000000000001/Ancestors"} {
		if rec := call(t, h, "GET", p, auth, ""); rec.Code != 404 {
			t.Errorf("%s = %d", p, rec.Code)
		}
	}
	execSQL(t, `UPDATE items SET missing_since = now() WHERE id = $1`, mfGhostE1)
	if rec := call(t, h, "GET", "/Items/"+mfGhostE1, auth, ""); rec.Code != 404 {
		t.Errorf("missing item = %d", rec.Code)
	}
}

// An unprobed .strm with no media source row yet still gets one source, so
// clients show a Play button (DESIGN §7.4).
func TestItemDetailPlaceholderSource(t *testing.T) {
	h, d := newIntegrationServer(t)
	seedRecon(t, d)
	seedCaptureToken(t, d)
	const luca = "84088e11eb6351255c08507602d79a0f"
	execSQL(t, `DELETE FROM media_sources WHERE item_id = $1`, luca)
	execSQL(t, `UPDATE items SET source_kind = 'strm', strm_url = 'http://debrid.example/stream/1', path = '/media/Movies/Luca (2021)/Luca (2021).strm' WHERE id = $1`, luca)
	var got struct {
		MediaSources []struct {
			Id, Path, Protocol, Container, Name string
			IsRemote                            bool
			MediaStreams                        []any
		}
	}
	getJSON(t, h, "/Items/"+luca, &got)
	if len(got.MediaSources) != 1 {
		t.Fatalf("sources: %+v", got)
	}
	s := got.MediaSources[0]
	// Path is this server's stream URL, never the .strm target (DESIGN §6).
	if s.Id != luca || s.Protocol != "Http" || !s.IsRemote || s.Path != "http://example.com/Videos/"+luca+"/stream?static=true&MediaSourceId="+luca || s.Container != "strm" ||
		s.Name != "Luca (2021)" || s.MediaStreams == nil || len(s.MediaStreams) != 0 {
		t.Errorf("placeholder: %+v", s)
	}
}

// Expected differences from Jellyfin in item details, beyond itemIgnores'
// (same reasons, here at the top level):
//   - MediaStreams[*].Score: Jellyfin's per-user subtitle ranking, not modelled.
//   - MediaSources[*].MediaAttachments: fonts embedded in Matroska aren't
//     probed yet (empty list).
//   - MediaSources[*].Size of an unprobed .strm: Jellyfin reports the .strm
//     file's own size (80 bytes); blockbustr leaves it out until probed.
//   - DefaultSubtitleStreamIndex: Jellyfin sends -1 or nothing for the same
//     kind of item across captures (it follows the user's subtitle settings
//     at the time); blockbustr always sends -1 when no subtitle is default.
var detailIgnores = []string{
	"$.People[*].ImageBlurHashes", "$.UserData.LastPlayedDate", "$.UserData.PlayedPercentage",
	"$.MediaStreams[*].Score", "$.MediaSources[*].MediaStreams[*].Score", "$.MediaSources[*].MediaAttachments",
	"$.MediaSources[*].Size", "$.PrimaryImageAspectRatio", "$.MediaSources[*].DefaultSubtitleStreamIndex",
	"$[*].PrimaryImageAspectRatio", "$[*].ImageTags", "$[*].ImageBlurHashes", // Ancestors: library collages
}

// captureKind sorts detail captures by what the recorded item was.
func captureKind(c capture) string {
	var b struct {
		Type         string
		MediaSources []struct{ MediaStreams []any }
	}
	_ = json.Unmarshal(c.Response.Body, &b)
	switch {
	case b.Type == "CollectionFolder":
		return "library"
	case len(b.MediaSources) > 0 && len(b.MediaSources[0].MediaStreams) == 0:
		return "unprobed"
	}
	return "item"
}

func detailCaptures(t *testing.T, kind string) []capture {
	var out []capture
	for _, e := range []string{"GET /Items/{id}", "GET /Users/{id}/Items/{id}", "GET /Items/{id}/Ancestors"} {
		for _, c := range capturesFor(t, e) {
			if captureKind(c) == kind {
				out = append(out, c)
			}
		}
	}
	return out
}

func TestItemDetailContract(t *testing.T) {
	h, d := newIntegrationServer(t)
	seedRecon(t, d)
	seedCaptureToken(t, d)
	checkContractOn(t, h, detailCaptures(t, "item"), detailIgnores...)
	// Jellyfin generates a collage as the Movies library image; blockbustr
	// has no library artwork.
	checkContractOn(t, h, detailCaptures(t, "library"), append(detailIgnores, "$.ImageTags", "$.ImageBlurHashes")...)
}

// Findroid's first Luca captures predate Jellyfin probing the .strm: replay
// them against an unprobed Luca. RunTimeTicks differs on purpose: blockbustr
// already has the TMDB runtime, Jellyfin only learns it from the probe.
func TestItemDetailUnprobedContract(t *testing.T) {
	h, d := newIntegrationServer(t)
	seedRecon(t, d)
	seedCaptureToken(t, d)
	caps := detailCaptures(t, "unprobed")
	if len(caps) == 0 {
		t.Fatal("no unprobed captures")
	}
	execSQL(t, `DELETE FROM media_streams WHERE media_source_id IN (SELECT id FROM media_sources WHERE protocol = 'Http')`)
	execSQL(t, `UPDATE media_sources SET container = NULL, size = NULL, bitrate = NULL, runtime_ticks = NULL WHERE protocol = 'Http'`)
	checkContractOn(t, h, caps, append(detailIgnores, "$.RunTimeTicks")...)
}
