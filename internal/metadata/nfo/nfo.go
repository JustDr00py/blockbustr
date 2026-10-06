// Package nfo reads Kodi-style .nfo metadata files (DESIGN §6: a local .nfo
// wins over TMDB). Supported roots: <movie>, <tvshow>, <episodedetails>.
// A "URL-only" nfo (a bare IMDb or TMDB link) yields just the ids.
package nfo

import (
	"bytes"
	"encoding/xml"
	"errors"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
)

// MaxSize caps how much of an .nfo is read.
const MaxSize = 1 << 20

// Info is the metadata found in an .nfo. Zero values mean "not present".
type Info struct {
	Kind          string // "movie", "tvshow", "episodedetails"; "" for a URL-only nfo
	Title         string
	OriginalTitle string
	SortTitle     string
	Year          int
	Premiered     string // YYYY-MM-DD
	Plot          string
	Tagline       string
	Certification string // <mpaa>, e.g. "PG-13" or "US:PG-13" normalised to "PG-13"
	Rating        float64
	Season        int
	Episode       int
	TMDBID        string
	IMDbID        string
	TVDBID        string
	Genres        []string
	Studios       []string
	Directors     []string
	Writers       []string
	Actors        []Actor
	Posters       []string // <thumb aspect="poster"> (or no aspect)
	Fanart        []string // <fanart><thumb>
}

// Actor is one <actor> entry.
type Actor struct {
	Name, Role, Thumb string
	Order             int
}

type rawThumb struct {
	Aspect string `xml:"aspect,attr"`
	URL    string `xml:",chardata"`
}

type rawDoc struct {
	XMLName       xml.Name
	Title         string `xml:"title"`
	OriginalTitle string `xml:"originaltitle"`
	SortTitle     string `xml:"sorttitle"`
	Year          string `xml:"year"`
	Premiered     string `xml:"premiered"`
	Aired         string `xml:"aired"`
	ReleaseDate   string `xml:"releasedate"`
	Plot          string `xml:"plot"`
	Outline       string `xml:"outline"`
	Tagline       string `xml:"tagline"`
	MPAA          string `xml:"mpaa"`
	Rating        string `xml:"rating"`
	Season        string `xml:"season"`
	Episode       string `xml:"episode"`
	TMDBID        string `xml:"tmdbid"`
	IMDbID        string `xml:"imdbid"`
	TVDBID        string `xml:"tvdbid"`
	ID            string `xml:"id"`
	Ratings       struct {
		Rating []struct {
			Name    string `xml:"name,attr"`
			Default bool   `xml:"default,attr"`
			Value   string `xml:"value"`
		} `xml:"rating"`
	} `xml:"ratings"`
	UniqueIDs []struct {
		Type  string `xml:"type,attr"`
		Value string `xml:",chardata"`
	} `xml:"uniqueid"`
	Genres    []string   `xml:"genre"`
	Studios   []string   `xml:"studio"`
	Directors []string   `xml:"director"`
	Credits   []string   `xml:"credits"`
	Thumbs    []rawThumb `xml:"thumb"`
	Fanart    struct {
		Thumbs []rawThumb `xml:"thumb"`
	} `xml:"fanart"`
	Actors []struct {
		Name  string `xml:"name"`
		Role  string `xml:"role"`
		Order string `xml:"order"`
		Thumb string `xml:"thumb"`
	} `xml:"actor"`
}

var (
	imdbRe = regexp.MustCompile(`\btt\d{7,9}\b`)
	tmdbRe = regexp.MustCompile(`themoviedb\.org/(?:movie|tv)/(\d+)`)
)

// ErrNotNFO means the file is neither nfo XML nor a recognisable link.
var ErrNotNFO = errors.New("nfo: no metadata found")

// ReadFile parses the .nfo at path.
func ReadFile(path string) (Info, error) {
	f, err := os.Open(path)
	if err != nil {
		return Info{}, err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, MaxSize))
	if err != nil {
		return Info{}, err
	}
	return Parse(data)
}

// Parse decodes nfo content.
func Parse(data []byte) (Info, error) {
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || trimmed[0] != '<' {
		return urlOnly(string(trimmed))
	}
	var raw rawDoc
	dec := xml.NewDecoder(bytes.NewReader(trimmed))
	dec.Strict = false // real-world nfo files are often slightly malformed
	if err := dec.Decode(&raw); err != nil {
		// Unparseable XML: the ids from any links are still worth having.
		if info, uerr := urlOnly(string(trimmed)); uerr == nil {
			return info, nil
		}
		return Info{}, err
	}
	kind := strings.ToLower(raw.XMLName.Local)
	switch kind {
	case "movie", "tvshow", "episodedetails":
	default:
		return Info{}, ErrNotNFO
	}
	info := Info{
		Kind:          kind,
		Title:         clean(raw.Title),
		OriginalTitle: clean(raw.OriginalTitle),
		SortTitle:     clean(raw.SortTitle),
		Year:          atoi(raw.Year),
		Premiered:     date(first(raw.Premiered, raw.Aired, raw.ReleaseDate)),
		Plot:          clean(first(raw.Plot, raw.Outline)),
		Tagline:       clean(raw.Tagline),
		Certification: certification(raw.MPAA),
		Season:        atoi(raw.Season),
		Episode:       atoi(raw.Episode),
		TMDBID:        clean(raw.TMDBID),
		IMDbID:        clean(raw.IMDbID),
		TVDBID:        clean(raw.TVDBID),
		Genres:        splitList(raw.Genres),
		Studios:       splitList(raw.Studios),
		Directors:     splitList(raw.Directors),
		Writers:       splitList(raw.Credits),
	}
	info.Rating = rating(raw)
	for _, u := range raw.UniqueIDs {
		v := clean(u.Value)
		switch strings.ToLower(u.Type) {
		case "tmdb":
			info.TMDBID = first(info.TMDBID, v)
		case "imdb":
			info.IMDbID = first(info.IMDbID, v)
		case "tvdb":
			info.TVDBID = first(info.TVDBID, v)
		}
	}
	if id := clean(raw.ID); id != "" {
		if imdbRe.MatchString(id) {
			info.IMDbID = first(info.IMDbID, id)
		} else if kind == "tvshow" {
			info.TVDBID = first(info.TVDBID, id) // Kodi's legacy <id> for shows is TVDB
		}
	}
	// Tools often append the source link after the XML; use it for missing ids.
	if ids, err := urlOnly(string(trimmed)); err == nil {
		info.IMDbID = first(info.IMDbID, ids.IMDbID)
		info.TMDBID = first(info.TMDBID, ids.TMDBID)
	}
	if info.Year == 0 && len(info.Premiered) >= 4 {
		info.Year = atoi(info.Premiered[:4])
	}
	for i, a := range raw.Actors {
		order := i
		if o, err := strconv.Atoi(strings.TrimSpace(a.Order)); err == nil {
			order = o
		}
		if name := clean(a.Name); name != "" {
			info.Actors = append(info.Actors, Actor{Name: name, Role: clean(a.Role), Thumb: clean(a.Thumb), Order: order})
		}
	}
	for _, t := range raw.Thumbs {
		if u := clean(t.URL); u != "" && (t.Aspect == "" || strings.EqualFold(t.Aspect, "poster")) {
			info.Posters = append(info.Posters, u)
		}
	}
	for _, t := range raw.Fanart.Thumbs {
		if u := clean(t.URL); u != "" {
			info.Fanart = append(info.Fanart, u)
		}
	}
	return info, nil
}

func urlOnly(s string) (Info, error) {
	var info Info
	if m := imdbRe.FindString(s); m != "" {
		info.IMDbID = m
	}
	if m := tmdbRe.FindStringSubmatch(s); m != nil {
		info.TMDBID = m[1]
	}
	if info.IMDbID == "" && info.TMDBID == "" {
		return Info{}, ErrNotNFO
	}
	return info, nil
}

func rating(raw rawDoc) float64 {
	var fallback float64
	for _, r := range raw.Ratings.Rating {
		v, err := strconv.ParseFloat(strings.TrimSpace(r.Value), 64)
		if err != nil {
			continue
		}
		if r.Default {
			return v
		}
		if fallback == 0 {
			fallback = v
		}
	}
	if v, err := strconv.ParseFloat(strings.TrimSpace(raw.Rating), 64); err == nil && v > 0 {
		return v
	}
	return fallback
}

// certification normalises "Rated PG-13", "US:PG-13" and "PG-13" to "PG-13".
func certification(s string) string {
	s = clean(s)
	s = strings.TrimPrefix(s, "Rated ")
	if _, after, ok := strings.Cut(s, ":"); ok && len(s) > 3 && s[2] == ':' {
		s = after
	}
	return strings.TrimSpace(s)
}

// splitList flattens repeated tags and "A / B" lists.
func splitList(vals []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, v := range vals {
		for _, part := range strings.Split(v, " / ") {
			if p := clean(part); p != "" && !seen[strings.ToLower(p)] {
				seen[strings.ToLower(p)] = true
				out = append(out, p)
			}
		}
	}
	return out
}

func clean(s string) string { return strings.TrimSpace(s) }

func first(vals ...string) string {
	for _, v := range vals {
		if v = clean(v); v != "" {
			return v
		}
	}
	return ""
}

func atoi(s string) int {
	n, _ := strconv.Atoi(strings.TrimSpace(s))
	return n
}

var dateRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}`)

func date(s string) string { return dateRe.FindString(clean(s)) }
