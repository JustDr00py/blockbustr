package library

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/sysadmin/blockbustr/internal/strm"
)

var folderYearRe = regexp.MustCompile(`^(.*?)\s*[\(\[](\d{4})[\)\]]\s*$`)

// folderTitle splits a movie or series folder name like "Severance (2022)"
// into title and year. Unlike strm.Parse (built for release file names) it
// leaves everything else alone, so "The Office (US)" stays intact.
func folderTitle(dir string) (string, int) {
	name := strings.TrimSpace(dir)
	if m := folderYearRe.FindStringSubmatch(name); m != nil {
		if y, _ := strconv.Atoi(m[2]); y >= 1880 && y <= 2100 && strings.TrimSpace(m[1]) != "" {
			return strings.TrimSpace(m[1]), y
		}
	}
	return name, 0
}

var leadingArticle = regexp.MustCompile(`^(?i)(the|a|an)\s+`)

// SortName is the browse sort key: lower-case, without a leading English
// article ("The Matrix" sorts under m).
func SortName(name string) string {
	s := strings.ToLower(strings.TrimSpace(name))
	if t := leadingArticle.ReplaceAllString(s, ""); t != "" {
		s = t
	}
	return strings.Join(strings.Fields(s), " ")
}

// episodeMarker finds the SxxEyy code; what follows " - " after it is the
// episode title, up to the first "[" (release tags) in names such as
// "MF Ghost (2023) - S01E01.001 - The Challenger from England [Bluray-1080p]…".
var episodeMarker = regexp.MustCompile(`(?i)\bS\d{1,2}E\d{1,3}(?:[-E]\d{1,3})*(?:\.\d+)?\b`)

func episodeTitle(fileName string) string {
	stem := strings.TrimSuffix(fileName, filepath.Ext(fileName))
	loc := episodeMarker.FindStringIndex(stem)
	if loc == nil {
		return ""
	}
	rest := stem[loc[1]:]
	sep := strings.Index(rest, " - ")
	if sep < 0 {
		return ""
	}
	title := rest[sep+3:]
	if i := strings.IndexAny(title, "[{"); i >= 0 {
		title = title[:i]
	}
	return strings.TrimSpace(strings.TrimRight(strings.TrimSpace(title), "-."))
}

// seasonName follows Jellyfin's naming for seasons.
func seasonName(n int) string {
	if n == 0 {
		return "Specials"
	}
	return fmt.Sprintf("Season %d", n)
}

// seasonFromDir maps a "Season 02" / "S02" / "Specials" folder to its number.
func seasonFromDir(relPath string) (int, bool) {
	if strings.EqualFold(filepath.Base(filepath.Dir(relPath)), "specials") {
		return 0, true
	}
	return strm.SeasonFromPath(relPath)
}

// containerName maps ffprobe's format_name to the short names Jellyfin uses
// in MediaSource.Container ("mkv", "mp4", "ts", …).
func containerName(formatName string) string {
	switch first, _, _ := strings.Cut(formatName, ","); first {
	case "matroska":
		return "mkv"
	case "mov":
		return "mp4"
	case "mpegts":
		return "ts"
	case "":
		return ""
	default:
		return first
	}
}
