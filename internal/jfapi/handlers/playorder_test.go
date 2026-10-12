package handlers

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/sysadmin/blockbustr/internal/media"
	"github.com/sysadmin/blockbustr/internal/store/pg/db"
)

// iosProfile plays H.264 and HEVC (not AV1) in MP4 and MKV, as Streamyfin
// on an iPhone does.
const iosProfile = `{"MaxStreamingBitrate":120000000,
	"DirectPlayProfiles":[{"Type":"Video","Container":"mkv,mp4","VideoCodec":"h264,hevc","AudioCodec":"aac,ac3,eac3"}],
	"TranscodingProfiles":[{"Type":"Video","Container":"ts","VideoCodec":"h264","AudioCodec":"aac","Protocol":"hls"}]}`

// The order a client gets: versions expected to play as is (probed, or by
// their label when unprobed) in rank order, then those that need the
// transcoder. Being probed never puts a version ahead; a failed probe never
// puts it behind (production: the one version whose probe got through a
// burst of 400s, a 720p, was put ahead of the 1080p the user picked).
func TestPlayableFirst(t *testing.T) {
	var p media.DeviceProfile
	if err := json.Unmarshal([]byte(iosProfile), &p); err != nil {
		t.Fatal(err)
	}
	row := func(name string) db.MediaSource {
		return db.MediaSource{ID: uuid.New(), Name: name, Container: ptr("mkv"), IsRemote: true}
	}
	video := func(codec string) []db.MediaStream {
		return []db.MediaStream{
			{Idx: 0, Type: "Video", Codec: ptr(codec), Width: ptr(int32(1280)), Height: ptr(int32(720)), IsDefault: true},
			{Idx: 1, Type: "Audio", Codec: ptr("aac"), Channels: ptr(int32(2)), IsDefault: true},
		}
	}
	order := func(rows []db.MediaSource, streams map[uuid.UUID][]db.MediaStream) string {
		t.Helper()
		var names []string
		for _, r := range playableFirst(rows, streams, p, media.PlayOptions{}) {
			names = append(names, strings.Fields(r.Name)[0]+" "+strings.Fields(r.Name)[1])
		}
		return strings.Join(names, ", ")
	}
	a, b, c := row("1080p HEVC • FLAC • 4.4 GB"), row("1080p H.264 • 2.0 GB"), row("720p H.264 • AAC • 0.3 GB")
	rows := []db.MediaSource{a, b, c} // rank order

	// Only the 720p is probed (it plays directly): rank order stays.
	streams := map[uuid.UUID][]db.MediaStream{c.ID: video("h264")}
	if got := order(rows, streams); got != "1080p HEVC, 1080p H.264, 720p H.264" {
		t.Errorf("unprobed versions the client plays: %s; want rank order", got)
	}
	// A's probe says it needs the transcoder: it goes last.
	streams[a.ID] = video("mpeg2video")
	if got := order(rows, streams); got != "1080p H.264, 720p H.264, 1080p HEVC" {
		t.Errorf("A probed as needing a transcode: %s", got)
	}
	// Unprobed, but its label names a codec the client doesn't play.
	av1 := row("2160p AV1 • 9.0 GB")
	delete(streams, a.ID)
	if got := order([]db.MediaSource{av1, b, c}, streams); got != "1080p H.264, 720p H.264, 2160p AV1" {
		t.Errorf("AV1 by its label: %s", got)
	}
	// A label without a codec: its rank decides.
	plain := row("1080p • 3.0 GB")
	if got := order([]db.MediaSource{plain, c}, streams); got != "1080p •, 720p H.264" {
		t.Errorf("no codec in the label: %s", got)
	}
}

func TestLabelVideo(t *testing.T) {
	for name, want := range map[string]struct {
		codec  string
		height int
	}{
		"1080p HEVC • FLAC • 4.4 GB":   {"hevc", 1080},
		"720p H.264 • AAC • 0.3 GB":    {"h264", 720},
		"2160p HDR AV1 • 9 GB • Comet": {"av1", 2160},
		"1080p • 3.0 GB":               {"", 1080},
		"Torrentio":                    {"", 0},
	} {
		if codec, height := labelVideo(name); codec != want.codec || height != want.height {
			t.Errorf("labelVideo(%q) = %q %d, want %q %d", name, codec, height, want.codec, want.height)
		}
	}
}
