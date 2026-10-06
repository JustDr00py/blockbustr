package stremio

import (
	"encoding/json"
	"strconv"
	"strings"
)

// Manifest is an addon's manifest.json.
type Manifest struct {
	ID            string        `json:"id"`
	Version       string        `json:"version"`
	Name          string        `json:"name"`
	Description   string        `json:"description"`
	Types         []string      `json:"types"`
	Resources     []Resource    `json:"resources"`
	Catalogs      []CatalogDef  `json:"catalogs"`
	IDPrefixes    []string      `json:"idPrefixes"`
	BehaviorHints ManifestHints `json:"behaviorHints"`
}

// ManifestHints are the manifest's behaviorHints.
type ManifestHints struct {
	Adult                 bool `json:"adult"`
	Configurable          bool `json:"configurable"`
	ConfigurationRequired bool `json:"configurationRequired"`
}

// Resource is one entry of manifest.resources: either a bare name
// ("stream"), which uses the manifest's types and idPrefixes, or an object
// narrowing them.
type Resource struct {
	Name       string   `json:"name"`
	Types      []string `json:"types"`
	IDPrefixes []string `json:"idPrefixes"`
}

// UnmarshalJSON accepts both forms.
func (r *Resource) UnmarshalJSON(b []byte) error {
	var name string
	if json.Unmarshal(b, &name) == nil {
		*r = Resource{Name: name}
		return nil
	}
	type plain Resource
	return json.Unmarshal(b, (*plain)(r))
}

// Supports reports whether the addon serves resource ("catalog", "meta",
// "stream", "subtitles") for a title of typ with id.
func (m Manifest) Supports(resource, typ, id string) bool {
	for _, r := range m.Resources {
		if r.Name != resource {
			continue
		}
		types, prefixes := r.Types, r.IDPrefixes
		if types == nil {
			types = m.Types
		}
		if prefixes == nil {
			prefixes = m.IDPrefixes
		}
		if !contains(types, typ) {
			continue
		}
		if len(prefixes) == 0 {
			return true
		}
		for _, p := range prefixes {
			if strings.HasPrefix(id, p) {
				return true
			}
		}
	}
	return false
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// CatalogDef is one entry of manifest.catalogs.
type CatalogDef struct {
	Type  string     `json:"type"`
	ID    string     `json:"id"`
	Name  string     `json:"name"`
	Extra []ExtraDef `json:"extra"`
	// Legacy form of Extra (older addons).
	ExtraSupported []string `json:"extraSupported"`
	ExtraRequired  []string `json:"extraRequired"`
}

// ExtraDef declares one extra a catalog accepts.
type ExtraDef struct {
	Name       string   `json:"name"`
	IsRequired bool     `json:"isRequired"`
	Options    []string `json:"options"`
}

// Accepts reports whether the catalog takes extra name (search, genre, skip).
func (c CatalogDef) Accepts(name string) bool {
	for _, e := range c.Extra {
		if e.Name == name {
			return true
		}
	}
	return contains(c.ExtraSupported, name)
}

// Requires lists the extras the catalog can't be fetched without (a
// search-only catalog requires "search").
func (c CatalogDef) Requires() []string {
	var out []string
	for _, e := range c.Extra {
		if e.IsRequired {
			out = append(out, e.Name)
		}
	}
	for _, n := range c.ExtraRequired {
		if !contains(out, n) {
			out = append(out, n)
		}
	}
	return out
}

// Extra are the optional catalog arguments.
type Extra struct {
	Search string
	Genre  string
	Skip   int
}

// Meta is a title (catalog entries carry a subset; meta responses all of it).
type Meta struct {
	ID          string   `json:"id"`
	Type        string   `json:"type"`
	Name        string   `json:"name"`
	Poster      string   `json:"poster"`
	PosterShape string   `json:"posterShape"`
	Background  string   `json:"background"`
	Logo        string   `json:"logo"`
	Description string   `json:"description"`
	ReleaseInfo Text     `json:"releaseInfo"` // "2021", "2019–2023", "2024–"
	Released    string   `json:"released"`    // ISO 8601
	Runtime     Text     `json:"runtime"`     // "155 min"
	IMDbRating  Text     `json:"imdbRating"`
	Genres      []string `json:"genres"`
	Director    []string `json:"director"`
	Cast        []string `json:"cast"`
	Country     Text     `json:"country"`
	Website     string   `json:"website"`
	IMDbID      string   `json:"imdb_id"`
	MovieDBID   Text     `json:"moviedb_id"`
	Videos      []Video  `json:"videos"`
}

// Year is the first year in ReleaseInfo, or Released's; 0 if neither.
func (m Meta) Year() int {
	for _, s := range []string{string(m.ReleaseInfo), m.Released} {
		if len(s) >= 4 {
			if y, err := strconv.Atoi(s[:4]); err == nil {
				return y
			}
		}
	}
	return 0
}

// Video is an episode of a series meta (or a movie's single video).
type Video struct {
	ID        string `json:"id"` // "tt0944947:1:2"
	Title     string `json:"title"`
	Name      string `json:"name"` // some addons use name instead of title
	Season    int    `json:"season"`
	Episode   int    `json:"episode"`
	Number    int    `json:"number"` // older addons
	Released  string `json:"released"`
	Thumbnail string `json:"thumbnail"`
	Overview  string `json:"overview"`
}

// EpisodeNumber is Episode, or the legacy Number.
func (v Video) EpisodeNumber() int {
	if v.Episode != 0 {
		return v.Episode
	}
	return v.Number
}

// Stream is one playable offer for a title.
type Stream struct {
	Name        string      `json:"name"`        // short label: "Torrentio\n4k"
	Title       string      `json:"title"`       // details: release name, size, seeders
	Description string      `json:"description"` // newer addons' replacement for title
	URL         string      `json:"url"`
	InfoHash    string      `json:"infoHash"`
	FileIdx     *int        `json:"fileIdx"`
	ExternalURL string      `json:"externalUrl"`
	YtID        string      `json:"ytId"`
	Sources     []string    `json:"sources"` // trackers / DHT for InfoHash
	Hints       StreamHints `json:"behaviorHints"`
}

// StreamHints are a stream's behaviorHints.
type StreamHints struct {
	BingeGroup  string `json:"bingeGroup"`
	Filename    string `json:"filename"`
	VideoSize   int64  `json:"videoSize"`
	NotWebReady bool   `json:"notWebReady"`
}

// Details is Description, or Title for addons that still use it.
func (s Stream) Details() string {
	if s.Description != "" {
		return s.Description
	}
	return s.Title
}

// Playable reports whether blockbustr can play s: a direct URL or a torrent
// (through debrid). External links and YouTube ids are not playable.
func (s Stream) Playable() bool { return s.URL != "" || s.InfoHash != "" }

// Subtitle is one subtitle track for a title.
type Subtitle struct {
	ID   string `json:"id"`
	URL  string `json:"url"`
	Lang string `json:"lang"` // ISO 639-2 usually ("eng"), sometimes a name
}

// Text is a string field some addons send as a number (imdbRating 7.5,
// releaseInfo 2021, moviedb_id 603).
type Text string

// UnmarshalJSON accepts a string, a number or null.
func (t *Text) UnmarshalJSON(b []byte) error {
	var s string
	if json.Unmarshal(b, &s) == nil {
		*t = Text(s)
		return nil
	}
	var n json.Number
	if err := json.Unmarshal(b, &n); err != nil {
		return err
	}
	*t = Text(n.String())
	return nil
}
