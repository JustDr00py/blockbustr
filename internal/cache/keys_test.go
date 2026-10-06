package cache

import (
	"testing"

	"github.com/google/uuid"
)

func TestKeyNames(t *testing.T) {
	u := uuid.MustParse("bbbb0000-0000-0000-0000-000000000001")
	p := uuid.MustParse("cccc0000-0000-0000-0000-000000000002")
	cases := map[Key]Key{
		TokenKey([]byte{0xab, 0x01}):                         "tok:ab01",
		DeviceTokensKey("dev-1"):                             "devtok:dev-1",
		SessionKey("dev-1"):                                  "sess:dev-1",
		LatestKey(u, p):                                      "q:latest:bbbb0000-0000-0000-0000-000000000001:cccc0000-0000-0000-0000-000000000002",
		StremioStreamsKey("torrentio", "movie", "tt0133093"): "stremio:streams:torrentio:movie:tt0133093",
		ScanLockKey(u):                                       "lock:scan:bbbb0000-0000-0000-0000-000000000001",
		SearchKey("cinemeta", "movie", "  The   MATRIX "):    "search:cinemeta:movie:the matrix",
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}
}
