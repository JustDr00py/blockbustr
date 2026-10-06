package handlers

import (
	"encoding/json"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/sysadmin/blockbustr/internal/cache"
)

// Expected differences from Jellyfin, beyond detailIgnores' (same reasons):
// none of the decision fields; see TestPlaybackInfoSameAsJellyfin.
var playbackIgnores = []string{
	"$.MediaSources[*].MediaStreams[*].Score", "$.MediaSources[*].MediaAttachments",
	"$.MediaSources[*].Size", "$.MediaSources[*].DefaultSubtitleStreamIndex",
}

type playbackResp struct {
	PlaySessionId string
	MediaSources  []struct {
		Id, Container, TranscodingUrl, TranscodingSubProtocol, TranscodingContainer string
		IsRemote                                                                    bool
		Bitrate                                                                     int64
		SupportsDirectPlay, SupportsDirectStream, SupportsTranscoding               bool
		MediaStreams                                                                []struct {
			Index                             int
			Type, DeliveryMethod, DeliveryUrl string
		}
	}
}

// remoteOverLimit is the intended divergence (DESIGN §8.3a, P2.4): Jellyfin
// ignores the bitrate limit for remote sources, blockbustr transcodes.
func remoteOverLimit(c capture, r playbackResp) bool {
	var b map[string]json.RawMessage
	_ = json.Unmarshal(c.Request.Body, &b)
	var limit int64
	for k, v := range b {
		if strings.EqualFold(k, "MaxStreamingBitrate") {
			_ = json.Unmarshal(v, &limit)
		}
	}
	return len(r.MediaSources) == 1 && r.MediaSources[0].IsRemote && limit > 0 && r.MediaSources[0].Bitrate > limit
}

func TestPlaybackInfoContract(t *testing.T) {
	h, d := newIntegrationServer(t)
	seedRecon(t, d)
	seedCaptureToken(t, d)
	var caps []capture
	for _, c := range capturesFor(t, "POST /Items/{id}/PlaybackInfo") {
		var want playbackResp
		_ = json.Unmarshal(c.Response.Body, &want)
		if !remoteOverLimit(c, want) {
			caps = append(caps, c)
		}
	}
	checkContractOn(t, h, caps, playbackIgnores...)
}

// Jellyfin's per-codec hints and per-request values aren't compared.
var skipURLParams = regexp.MustCompile(`^(PlaySessionId|DeviceId|[a-z0-9]+-[a-z]+)$`)

func TestPlaybackInfoSameAsJellyfin(t *testing.T) {
	h, d := newIntegrationServer(t)
	seedRecon(t, d)
	seedCaptureToken(t, d)
	hex32 := regexp.MustCompile(`^[0-9a-f]{32}$`)
	transcodes := 0
	for _, c := range capturesFor(t, "POST /Items/{id}/PlaybackInfo") {
		var want, got playbackResp
		_ = json.Unmarshal(c.Response.Body, &want)
		rec := replay(t, h, c)
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || rec.Code != 200 {
			t.Fatalf("%s: %d %s", c.Name, rec.Code, rec.Body)
		}
		if !hex32.MatchString(got.PlaySessionId) || got.PlaySessionId == want.PlaySessionId {
			t.Errorf("%s: PlaySessionId %q", c.Name, got.PlaySessionId)
		}
		var ps PlaySession
		if ok, err := d.Cache.GetJSON(t.Context(), cache.PlaySessionKey(got.PlaySessionId), &ps); !ok || err != nil || len(ps.Sources) != len(got.MediaSources) {
			t.Errorf("%s: stored session %v %v %+v", c.Name, ok, err, ps)
		}
		if len(got.MediaSources) != len(want.MediaSources) {
			t.Fatalf("%s: %d sources, Jellyfin %d", c.Name, len(got.MediaSources), len(want.MediaSources))
		}
		if remoteOverLimit(c, want) {
			g := got.MediaSources[0]
			if g.SupportsDirectPlay || !strings.Contains(g.TranscodingUrl, "ContainerBitrateExceedsLimit") {
				t.Errorf("%s: a remote source over the limit transcodes: %+v", c.Name, g)
			}
			continue
		}
		for i, w := range want.MediaSources {
			g := got.MediaSources[i]
			if g.Id != w.Id || g.Container != w.Container || g.SupportsDirectPlay != w.SupportsDirectPlay ||
				g.SupportsDirectStream != w.SupportsDirectStream || g.SupportsTranscoding != w.SupportsTranscoding ||
				g.TranscodingSubProtocol != w.TranscodingSubProtocol || g.TranscodingContainer != w.TranscodingContainer {
				t.Errorf("%s:\n  blockbustr %s %s DP/DS/TC %v/%v/%v %s %s\n  jellyfin   %s %s DP/DS/TC %v/%v/%v %s %s", c.Name,
					g.Id, g.Container, g.SupportsDirectPlay, g.SupportsDirectStream, g.SupportsTranscoding, g.TranscodingSubProtocol, g.TranscodingContainer,
					w.Id, w.Container, w.SupportsDirectPlay, w.SupportsDirectStream, w.SupportsTranscoding, w.TranscodingSubProtocol, w.TranscodingContainer)
			}
			for j, ws := range w.MediaStreams {
				gs := g.MediaStreams[j]
				if gs.DeliveryMethod != ws.DeliveryMethod || gs.DeliveryUrl != ws.DeliveryUrl {
					t.Errorf("%s: stream %d delivery %q %q, Jellyfin %q %q", c.Name, ws.Index, gs.DeliveryMethod, gs.DeliveryUrl, ws.DeliveryMethod, ws.DeliveryUrl)
				}
			}
			if w.TranscodingUrl == "" {
				if g.TranscodingUrl != "" {
					t.Errorf("%s: unexpected TranscodingUrl %s", c.Name, g.TranscodingUrl)
				}
				continue
			}
			transcodes++
			wu, _ := url.Parse(w.TranscodingUrl)
			gu, _ := url.Parse(g.TranscodingUrl)
			if gu == nil || gu.Path != wu.Path {
				t.Errorf("%s: TranscodingUrl %s, Jellyfin %s", c.Name, g.TranscodingUrl, w.TranscodingUrl)
				continue
			}
			wq, gq := wu.Query(), gu.Query()
			for k := range wq {
				if !skipURLParams.MatchString(k) && wq.Get(k) != gq.Get(k) {
					t.Errorf("%s: TranscodingUrl %s=%q, Jellyfin %q", c.Name, k, gq.Get(k), wq.Get(k))
				}
			}
			if gq.Get("PlaySessionId") != got.PlaySessionId {
				t.Errorf("%s: URL play session %s", c.Name, gq.Get("PlaySessionId"))
			}
		}
	}
	if transcodes != 2 {
		t.Errorf("compared %d transcodes, want 2", transcodes)
	}
}

func TestPlaybackInfoRequests(t *testing.T) {
	h, d := newIntegrationServer(t)
	seedRecon(t, d)
	seedCaptureToken(t, d)
	admin := `MediaBrowser Token="` + captureToken + `"`
	const mkii = "1f77a87a9603bff7d1589d1e076f060c"
	profile := `{"DeviceProfile":{"MaxStreamingBitrate":120000000,
		"DirectPlayProfiles":[{"Type":"Video","Container":"mp4,mkv","VideoCodec":"h264,hevc,av1","AudioCodec":"aac"}],
		"TranscodingProfiles":[{"Type":"Video","Container":"ts","Protocol":"hls","VideoCodec":"h264","AudioCodec":"aac"}]}`
	post := func(path, body string) playbackResp {
		t.Helper()
		rec := call(t, h, "POST", path, admin, body)
		if rec.Code != 200 {
			t.Fatalf("%s = %d %s", path, rec.Code, rec.Body)
		}
		var r playbackResp
		_ = json.Unmarshal(rec.Body.Bytes(), &r)
		return r
	}
	if r := post("/Items/"+mkii+"/PlaybackInfo", profile+`}`); len(r.MediaSources) != 1 || !r.MediaSources[0].SupportsDirectPlay {
		t.Errorf("direct play: %+v", r)
	}
	// The limit from the query string (camelCase body keys work too).
	r := post("/Items/"+mkii+"/PlaybackInfo?maxStreamingBitrate=3000000", profile+`, "enableDirectStream": true}`)
	if len(r.MediaSources) != 1 || r.MediaSources[0].SupportsDirectPlay || !strings.Contains(r.MediaSources[0].TranscodingUrl, "VideoBitrate=2839539") {
		t.Errorf("over the limit: %+v", r)
	}
	if r := post("/Items/"+mkii+"/PlaybackInfo", strings.Replace(profile, `{"DeviceProfile"`, `{"enableDirectPlay":false,"DeviceProfile"`, 1)+`}`); r.MediaSources[0].SupportsDirectPlay {
		t.Errorf("direct play disabled: %+v", r.MediaSources[0])
	}
	if r := post("/Items/"+mkii+"/PlaybackInfo?mediaSourceId=00000000000000000000000000000042", profile+`}`); len(r.MediaSources) != 0 {
		t.Errorf("unknown media source: %+v", r.MediaSources)
	}
	// GET has no profile: nothing is supported (clients that send none
	// stream /Videos/{id}/stream themselves, like Findroid).
	var g playbackResp
	getJSON(t, h, "/Items/"+mkii+"/PlaybackInfo?userId="+strings.ReplaceAll(captureUserID, "-", ""), &g)
	if len(g.MediaSources) != 1 || g.MediaSources[0].SupportsDirectPlay || g.MediaSources[0].SupportsTranscoding {
		t.Errorf("GET: %+v", g)
	}
	for path, want := range map[string]int{
		"/Items/" + mfGhost + "/PlaybackInfo":                  404, // a series isn't playable
		"/Items/00000000000000000000000000000042/PlaybackInfo": 404,
	} {
		if rec := call(t, h, "POST", path, admin, profile+`}`); rec.Code != want {
			t.Errorf("%s = %d", path, rec.Code)
		}
	}
	if rec := call(t, h, "POST", "/Items/"+mkii+"/PlaybackInfo", admin, `{"DeviceProfile":`); rec.Code != 400 {
		t.Errorf("bad body = %d", rec.Code)
	}
}
