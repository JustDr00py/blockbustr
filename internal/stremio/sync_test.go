package stremio

import "testing"

func TestLibraryName(t *testing.T) {
	for _, c := range []struct{ typ, label, want string }{
		{"movie", "Popular Movies", "Popular Movies"}, // AIOStreams' TMDB catalogs say it already
		{"series", "Popular Shows", "Popular Shows"},
		{"movie", "Popular", "Popular Movies"}, // Cinemeta's don't
		{"series", "Popular", "Popular Shows"},
		{"series", "Trending TV", "Trending TV"},
		{"series", "Netflix Series", "Netflix Series"},
		{"movie", " Top Films ", "Top Films"},
		{"anime", "Seasonal", "Seasonal"}, // other kinds are left as they are
	} {
		if got := LibraryName(c.typ, c.label); got != c.want {
			t.Errorf("LibraryName(%q, %q) = %q, want %q", c.typ, c.label, got, c.want)
		}
	}
}
