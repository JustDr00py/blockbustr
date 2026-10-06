package stremio

import (
	"os"
	"testing"
)

// TestLive talks to the real Cinemeta and (unconfigured) Torrentio. It only
// runs with BLOCKBUSTR_LIVE_STREMIO=1, never in CI.
func TestLive(t *testing.T) {
	if os.Getenv("BLOCKBUSTR_LIVE_STREMIO") == "" {
		t.Skip("BLOCKBUSTR_LIVE_STREMIO not set")
	}
	c := &Client{}
	cinemeta, err := BaseURL("https://v3-cinemeta.strem.io/manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	m, err := c.Manifest(t.Context(), cinemeta)
	if err != nil || !m.Supports("meta", "series", "tt0944947") {
		t.Fatalf("cinemeta manifest: %v", err)
	}
	metas, err := c.Catalog(t.Context(), cinemeta, "movie", "top", Extra{Search: "the matrix"})
	if err != nil || len(metas) == 0 || metas[0].ID != "tt0133093" {
		t.Errorf("search: %d results, %v", len(metas), err)
	}
	page2, err := c.Catalog(t.Context(), cinemeta, "movie", "top", Extra{Skip: 100})
	if err != nil || len(page2) == 0 {
		t.Errorf("skip=100: %d, %v", len(page2), err)
	}
	got, err := c.Meta(t.Context(), cinemeta, "series", "tt0944947")
	if err != nil || len(got.Videos) < 70 {
		t.Errorf("meta: %d videos, %v", len(got.Videos), err)
	}

	torrentio, _ := BaseURL("stremio://torrentio.strem.fun/manifest.json")
	tm, err := c.Manifest(t.Context(), torrentio)
	if err != nil || !tm.Supports("stream", "series", "tt0944947:1:2") {
		t.Fatalf("torrentio manifest: %v", err)
	}
	streams, err := c.Streams(t.Context(), torrentio, "series", "tt0944947:1:2")
	if err != nil || len(streams) == 0 || !streams[0].Playable() {
		t.Errorf("streams: %d, %v", len(streams), err)
	}
	t.Logf("cinemeta: %d search results, %d on page 2, %d GoT videos; torrentio: %d streams for S01E02", len(metas), len(page2), len(got.Videos), len(streams))
}
