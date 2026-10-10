package metadata

import (
	"testing"

	"github.com/sysadmin/blockbustr/internal/metadata/tmdb"
)

func TestArtworkSkipsSVGLogos(t *testing.T) {
	im := tmdb.Images{Logos: []tmdb.Image{
		{FilePath: "/best.svg", Language: "en", VoteAverage: 9},
		{FilePath: "/ok.png", Language: "en", VoteAverage: 5},
	}}
	var logo string
	for _, i := range artwork(im, "", "", "en") {
		if i.Type == "Logo" {
			logo = i.URL
		}
	}
	if logo != tmdb.ImageURL("/ok.png") {
		t.Errorf("logo = %q, want the PNG", logo)
	}

	only := tmdb.Images{Logos: []tmdb.Image{{FilePath: "/only.SVG"}}}
	for _, i := range artwork(only, "", "", "en") {
		if i.Type == "Logo" {
			t.Errorf("an SVG-only logo should be skipped, got %q", i.URL)
		}
	}
}
