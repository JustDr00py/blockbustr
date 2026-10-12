// Package segments finds the parts of an episode a player can skip
// (/MediaSegments): its intro, credits, recap and preview, from the
// version's chapter markers or, for anime, from AniSkip.
package segments

import (
	"regexp"
	"strings"
	"time"

	"github.com/sysadmin/blockbustr/internal/media"
)

// Type is a segment's kind, as Jellyfin's MediaSegmentType names it.
type Type string

// Segment types.
const (
	Intro   Type = "Intro"
	Outro   Type = "Outro"
	Recap   Type = "Recap"
	Preview Type = "Preview"
)

// Segment is a skippable part of a file.
type Segment struct {
	Type       Type
	Start, End time.Duration
}

// Sanity limits: an intro or credits shorter or longer is a mislabelled
// chapter, and an intro starts in the first part of the episode.
const (
	minTheme     = 10 * time.Second
	maxTheme     = 4 * time.Minute
	introWithin  = 0.4 // of the runtime
	minSegment   = time.Second
	maxGapToJoin = time.Second // same-type chapters this close are one segment
)

// Chapter names by type, matched whole after lower-casing and trimming.
var chapterNames = []struct {
	t  Type
	re *regexp.Regexp
}{
	{Intro, regexp.MustCompile(`^(opening( credits| song| theme)?|op ?\d*|intro|theme( song)?)$`)},
	{Outro, regexp.MustCompile(`^(ending( credits| song| theme)?|ed ?\d*|credits|end credits|closing credits|outro)$`)},
	{Recap, regexp.MustCompile(`^(recap|previously( on\b.*)?)$`)},
	{Preview, regexp.MustCompile(`^(preview|next episode( preview)?|next time|next ep(isode)?)$`)},
}

// chapterType is the segment type a chapter's name says; "" for one that
// says none ("Chapter 1", "00:01:30.000", "Part A").
func chapterType(name string) Type {
	n := strings.ToLower(strings.TrimSpace(name))
	for _, c := range chapterNames {
		if c.re.MatchString(n) {
			return c.t
		}
	}
	return ""
}

// FromChapters reads a file's segments from its chapter markers: a chapter
// runs to the next one's start (the last to runtime; without a runtime it
// has no end and gives none). Chapters whose names say no segment type are
// skipped, neighbours of one type are joined, and intros and credits
// outside the sanity limits are dropped.
func FromChapters(chapters []media.Chapter, runtime time.Duration) []Segment {
	var out []Segment
	for i, c := range chapters {
		t := chapterType(c.Title)
		if t == "" {
			continue
		}
		end := runtime
		if i+1 < len(chapters) {
			end = chapters[i+1].Start
		}
		if end-c.Start < minSegment {
			continue
		}
		if n := len(out); n > 0 && out[n-1].Type == t && c.Start-out[n-1].End <= maxGapToJoin {
			out[n-1].End = end
			continue
		}
		out = append(out, Segment{Type: t, Start: c.Start, End: end})
	}
	kept := out[:0]
	for _, s := range out {
		if sane(s, runtime) {
			kept = append(kept, s)
		}
	}
	return kept
}

// sane applies the limits to a segment of a file of runtime (0: unknown).
func sane(s Segment, runtime time.Duration) bool {
	if s.End <= s.Start || (runtime > 0 && s.End > runtime+time.Second) {
		return false
	}
	switch s.Type {
	case Intro, Outro:
		if d := s.End - s.Start; d < minTheme || d > maxTheme {
			return false
		}
	}
	if s.Type == Intro && runtime > 0 && float64(s.Start) > introWithin*float64(runtime) {
		return false
	}
	return true
}
