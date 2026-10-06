package strm

import (
	"path/filepath"
	"strings"
)

// extrasDirs are folder names (normalized: lowercase, separators as spaces)
// that releases and Jellyfin's own conventions use for bonus material. A
// video anywhere under one of these is never a main feature, however large
// — a feature-length storyboard or making-of can easily clear
// extrasSizeRatio. "Specials" is deliberately absent: it's TV season 0.
var extrasDirs = map[string]bool{
	"extras": true, "extra": true, "bonus": true, "bonus features": true, "bonus material": true,
	"featurettes": true, "featurette": true, "behind the scenes": true, "making of": true,
	"deleted scenes": true, "interviews": true, "scenes": true, "shorts": true,
	"trailers": true, "trailer": true, "samples": true, "sample": true,
}

// extrasSuffixes are Jellyfin's "<name>-<type>" extras file-name suffixes.
var extrasSuffixes = []string{
	"-trailer", "-sample", "-featurette", "-behindthescenes", "-deleted",
	"-deletedscene", "-interview", "-scene", "-short", "-extra",
}

// IsExtra reports whether a file is bonus material (trailer, featurette,
// sample, …) by its folder or file name, so the scanner keeps it out of the
// main library. Paths may be absolute or relative to the library root.
func IsExtra(path string) bool {
	parts := strings.Split(filepath.ToSlash(path), "/")
	norm := strings.NewReplacer(".", " ", "_", " ", "-", " ")
	for _, dir := range parts[:len(parts)-1] {
		if extrasDirs[strings.Join(strings.Fields(norm.Replace(strings.ToLower(dir))), " ")] {
			return true
		}
	}
	stem := strings.ToLower(strings.TrimSuffix(parts[len(parts)-1], filepath.Ext(path)))
	if stem == "sample" {
		return true
	}
	for _, suf := range extrasSuffixes {
		if strings.HasSuffix(stem, suf) {
			return true
		}
	}
	return false
}
