package stremio

import (
	"context"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"
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

	col := &Collector{
		Registry: &Registry{Client: c},
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		addons: func(context.Context) ([]addonInfo, error) {
			return []addonInfo{testAddon("Torrentio", torrentio, 0, tm)}, nil
		},
	}
	offers, err := col.Collect(t.Context(), "movie", "tt0133093")
	if err != nil || len(offers) == 0 {
		t.Fatalf("collect: %d, %v", len(offers), err)
	}
	ranked := Rank(offers, Prefs{MaxHeight: 1080, Runtime: 136 * time.Minute, MaxBitrate: 20_000_000, Languages: []string{"en"}})
	for i, r := range ranked[:min(5, len(ranked))] {
		t.Logf("#%d score %d: %dp %s hdr=%v %.1f GB group=%q langs=%v", i+1, r.Score, r.Info.Height, r.Info.Codec, r.Info.HDR, float64(r.Info.Size)/(1<<30), r.Info.Group, r.Info.Languages)
	}
	if ranked[0].Info.Height != 1080 {
		t.Errorf("a 1080p-capped H.264 client got %dp first", ranked[0].Info.Height)
	}

	opensubs, _ := BaseURL("https://opensubtitles-v3.strem.io/manifest.json")
	om, err := c.Manifest(t.Context(), opensubs)
	if err != nil || !om.Supports("subtitles", "series", "tt0944947:1:2") {
		t.Fatalf("opensubtitles manifest: %v", err)
	}
	col.addons = func(context.Context) ([]addonInfo, error) {
		return []addonInfo{testAddon("OpenSubtitles", opensubs, 0, om)}, nil
	}
	subs, err := col.Subtitles(t.Context(), "series", "tt0944947:1:2")
	if err != nil || len(subs) == 0 || subs[0].Subtitle.URL == "" {
		t.Errorf("subtitles: %d, %v", len(subs), err)
	}
	t.Logf("opensubtitles: %d subtitles for S01E02", len(subs))
}
