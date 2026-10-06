package library

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/text/language"

	"github.com/sysadmin/blockbustr/internal/media"
)

// Sidecar subtitles (TASKS P2.8, DESIGN §6): text subtitle files next to a
// video or .strm, named after it: "Movie.srt", "Movie.en.srt",
// "Movie.eng.forced.ass", "Movie.en.sdh.srt", "Movie.default.vtt". They
// become external Subtitle streams numbered after the file's own streams.

// sidecarCodecs maps subtitle file extensions to ffprobe codec names.
var sidecarCodecs = map[string]string{".srt": "subrip", ".ass": "ass", ".ssa": "ssa", ".vtt": "webvtt"}

// findSidecars lists the subtitle files belonging to media file path, in
// name order (so stream numbering is stable across scans).
func findSidecars(path string) []media.Stream {
	dir := filepath.Dir(path)
	stem := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		n := e.Name()
		ext := strings.ToLower(filepath.Ext(n))
		if e.IsDir() || sidecarCodecs[ext] == "" || !strings.HasPrefix(n, stem) {
			continue
		}
		// Exactly the stem, or the stem followed by ".tags".
		if rest := strings.TrimSuffix(n[len(stem):], filepath.Ext(n)); rest == "" || strings.HasPrefix(rest, ".") {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	out := make([]media.Stream, 0, len(names))
	for _, n := range names {
		ext := filepath.Ext(n)
		s := media.Stream{Type: media.StreamSubtitle, Codec: sidecarCodecs[strings.ToLower(ext)], IsExternal: true, Path: filepath.Join(dir, n)}
		for _, tag := range strings.Split(strings.Trim(strings.TrimSuffix(n[len(stem):], ext), "."), ".") {
			switch t := strings.ToLower(tag); t {
			case "":
			case "forced", "foreign":
				s.IsForced = true
			case "default":
				s.IsDefault = true
			case "sdh", "hi", "cc":
				s.IsHearingImpaired = true
			default:
				if s.Language == "" {
					s.Language = languageCode(t)
				}
			}
		}
		out = append(out, s)
	}
	return out
}

// languageCode turns a file name tag ("en", "eng", "pt-BR") into the ISO
// 639-2 code ffprobe uses for embedded streams; "" if it isn't a language.
func languageCode(tag string) string {
	if len(tag) < 2 {
		return ""
	}
	t, err := language.Parse(tag)
	if err != nil {
		return ""
	}
	base, conf := t.Base()
	if conf == language.No {
		return ""
	}
	return base.ISO3()
}

// sidecarSignature changes whenever path's sidecars are added, removed or
// edited; it's folded into the source etag so the next scan re-reads them.
func sidecarSignature(path string) string {
	subs := findSidecars(path)
	if len(subs) == 0 {
		return ""
	}
	h := sha256.New()
	for _, s := range subs {
		if fi, err := os.Stat(s.Path); err == nil {
			_, _ = fmt.Fprintf(h, "%s|%d|%d\n", filepath.Base(s.Path), fi.Size(), fi.ModTime().UnixNano()) // hash writes don't fail
		}
	}
	return "-s" + hex.EncodeToString(h.Sum(nil)[:6])
}

// withSidecars appends path's sidecar subtitles to info's streams, numbered
// after the highest stream index.
func withSidecars(info *media.Info, path string) *media.Info {
	subs := findSidecars(path)
	if info == nil || len(subs) == 0 {
		return info
	}
	next := 0
	for _, s := range info.Streams {
		next = max(next, s.Index+1)
	}
	out := *info
	out.Streams = append([]media.Stream(nil), info.Streams...)
	for i, s := range subs {
		s.Index = next + i
		out.Streams = append(out.Streams, s)
	}
	return &out
}
