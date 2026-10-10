package jfapi

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sysadmin/blockbustr/internal/jfapi/dto"
)

// jellyfin-roku's PlaybackInfo body: the AudioChannels condition's Value is
// a bare number and MaxAudioChannels a string, which real Jellyfin accepts.
func TestDecodeJSONCoercesScalarsLikeJellyfin(t *testing.T) {
	body := `{"UserId":"233c59f49f9c4913a97fbdda576cc9fd","StartTimeTicks":"0","IsPlayback":true,
		"MaxStreamingBitrate":"120000000","EnableDirectPlay":"true",
		"DeviceProfile":{"MaxStaticBitrate":100000000,
		"TranscodingProfiles":[{"Container":"ts","MaxAudioChannels":6,"MinSegments":1,"BreakOnNonKeyFrames":false}],
		"CodecProfiles":[{"Type":"VideoAudio","Codec":"aac","Conditions":[
			{"Condition":"LessThanEqual","Property":"AudioChannels","Value":6,"IsRequired":true},
			{"Condition":"Equals","Property":"IsAnamorphic","Value":"true","IsRequired":false}]}]}}`
	r := httptest.NewRequestWithContext(t.Context(), "POST", "/Items/x/PlaybackInfo", strings.NewReader(body))
	var b dto.PlaybackInfoDto
	if err := DecodeJSON(httptest.NewRecorder(), r, &b); err != nil {
		t.Fatalf("DecodeJSON: %v", err)
	}
	if b.UserId == nil || b.StartTimeTicks == nil || *b.StartTimeTicks != 0 {
		t.Errorf("UserId/StartTimeTicks not decoded: %+v %+v", b.UserId, b.StartTimeTicks)
	}
	if b.MaxStreamingBitrate == nil || *b.MaxStreamingBitrate != 120000000 {
		t.Errorf("MaxStreamingBitrate = %v, want 120000000", b.MaxStreamingBitrate)
	}
	if b.EnableDirectPlay == nil || !*b.EnableDirectPlay {
		t.Errorf("EnableDirectPlay = %v, want true", b.EnableDirectPlay)
	}
	p := b.DeviceProfile
	if got := *(*p.TranscodingProfiles)[0].MaxAudioChannels; got != "6" {
		t.Errorf("MaxAudioChannels = %q, want \"6\"", got)
	}
	conds := *(*p.CodecProfiles)[0].Conditions
	if got := *conds[0].Value; got != "6" {
		t.Errorf("AudioChannels Value = %q, want \"6\"", got)
	}
	if got := *conds[1].Value; got != "true" {
		t.Errorf("IsAnamorphic Value = %q, want \"true\"", got)
	}
}

func TestDecodeJSONStillRejectsBadBodies(t *testing.T) {
	for _, body := range []string{`{"MaxStreamingBitrate":"fast"}`, `{"EnableDirectPlay":"maybe"}`, `{not json`} {
		r := httptest.NewRequestWithContext(t.Context(), "POST", "/", strings.NewReader(body))
		var b dto.PlaybackInfoDto
		if err := DecodeJSON(httptest.NewRecorder(), r, &b); err == nil {
			t.Errorf("DecodeJSON(%s) = nil, want an error", body)
		}
	}
}

func TestDecodeJSONEmptyBody(t *testing.T) {
	r := httptest.NewRequestWithContext(t.Context(), "POST", "/", strings.NewReader("  "))
	var b dto.PlaybackInfoDto
	if err := DecodeJSON(httptest.NewRecorder(), r, &b); err != nil {
		t.Fatalf("empty body: %v", err)
	}
}
