package segments

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The community mappings an anime episode's MyAnimeList id comes from:
// Anime-Lists' TVDB mapping gives each AniDB anime its TVDB series, season
// and episode offset (one TVDB season is often two AniDB/MAL entries, a
// cour each: SAKAMOTO DAYS' episodes 12–22 are another MAL anime's 1–11),
// and Fribb's list gives AniDB → MAL and the IMDb and TMDB ids of TVDB
// series. Only series in them are anime as far as AniSkip goes.
const (
	AnimeListTVDBURL = "https://raw.githubusercontent.com/Anime-Lists/anime-lists/master/anime-list-master.xml"
	AnimeListIDsURL  = "https://raw.githubusercontent.com/Fribb/anime-lists/master/anime-list-full.json"
)

// animeListRefresh is how often the mappings are fetched again.
const animeListRefresh = 24 * time.Hour

// AnimeList maps a series' episode (by its IMDb or TMDB id and TVDB-style
// season and episode numbers, as Stremio numbers them) to a MAL anime and
// episode. The zero value maps nothing until Load.
type AnimeList struct {
	// TVDBURL and IDsURL are the mappings' URLs (tests); empty: the above.
	TVDBURL, IDsURL string
	HTTP            *http.Client
	Log             *slog.Logger

	mu    sync.RWMutex
	cours map[int][]cour // by TVDB series id
	imdb  map[string]int // IMDb series id → TVDB id
	tmdb  map[int]int    // TMDB TV id → TVDB id
}

// cour is a MAL anime as part of a TVDB season: its episodes are the
// season's from offset+1.
type cour struct {
	season, offset, mal int
}

// Lookup finds a series episode's MAL anime and episode number there. ok
// is false for a series not in the mappings (not anime), or a season or
// episode none of its anime covers.
func (l *AnimeList) Lookup(imdb string, tmdb, season, episode int) (mal, malEpisode int, ok bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	tvdb, found := l.imdb[imdb]
	if !found && tmdb > 0 {
		tvdb, found = l.tmdb[tmdb]
	}
	if !found || episode <= 0 {
		return 0, 0, false
	}
	best := -1
	for i, c := range l.cours[tvdb] {
		if c.season == season && c.offset < episode && (best < 0 || c.offset > l.cours[tvdb][best].offset) {
			best = i
		}
	}
	if best < 0 {
		return 0, 0, false
	}
	c := l.cours[tvdb][best]
	return c.mal, episode - c.offset, true
}

// Run loads the mappings now and every animeListRefresh until ctx ends; a
// failed load keeps the last good one.
func (l *AnimeList) Run(ctx context.Context) {
	for {
		if err := l.Load(ctx); err != nil && ctx.Err() == nil && l.Log != nil {
			l.Log.WarnContext(ctx, "anime mappings not loaded; AniSkip skip times wait for the next try", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(animeListRefresh):
		}
	}
}

// Load fetches and parses both mappings and replaces the current ones.
func (l *AnimeList) Load(ctx context.Context) error {
	tvdbURL, idsURL := l.TVDBURL, l.IDsURL
	if tvdbURL == "" {
		tvdbURL = AnimeListTVDBURL
	}
	if idsURL == "" {
		idsURL = AnimeListIDsURL
	}
	var ids []fribbEntry
	if err := l.get(ctx, idsURL, func(r io.Reader) error { return json.NewDecoder(r).Decode(&ids) }); err != nil {
		return err
	}
	var master struct {
		Anime []struct {
			AniDB  int    `xml:"anidbid,attr"`
			TVDB   string `xml:"tvdbid,attr"` // a number, or "movie", "unknown"…
			Season string `xml:"defaulttvdbseason,attr"`
			Offset string `xml:"episodeoffset,attr"`
		} `xml:"anime"`
	}
	if err := l.get(ctx, tvdbURL, func(r io.Reader) error { return xml.NewDecoder(r).Decode(&master) }); err != nil {
		return err
	}

	malOf := make(map[int]int, len(ids))
	imdb, tmdb := map[string]int{}, map[int]int{}
	for _, e := range ids {
		if e.AniDB > 0 && e.MAL > 0 {
			malOf[e.AniDB] = e.MAL
		}
		if e.TVDB <= 0 {
			continue
		}
		for _, id := range e.IMDb {
			if strings.HasPrefix(id, "tt") {
				imdb[id] = e.TVDB
			}
		}
		if e.TMDB.TV > 0 {
			tmdb[e.TMDB.TV] = e.TVDB
		}
	}
	cours := map[int][]cour{}
	for _, a := range master.Anime {
		tvdb, err1 := strconv.Atoi(a.TVDB)
		season, err2 := strconv.Atoi(a.Season) // "a" (absolute numbering) isn't mapped
		mal := malOf[a.AniDB]
		if err1 != nil || err2 != nil || tvdb <= 0 || season <= 0 || mal == 0 {
			continue
		}
		offset, _ := strconv.Atoi(a.Offset)
		cours[tvdb] = append(cours[tvdb], cour{season: season, offset: offset, mal: mal})
	}
	if len(cours) == 0 {
		return fmt.Errorf("anime mappings: none parsed")
	}
	l.mu.Lock()
	l.cours, l.imdb, l.tmdb = cours, imdb, tmdb
	l.mu.Unlock()
	if l.Log != nil {
		l.Log.InfoContext(ctx, "anime mappings loaded", "series", len(cours))
	}
	return nil
}

// fribbEntry is an anime-list-full.json entry's ids.
type fribbEntry struct {
	MAL   int      `json:"mal_id"`
	AniDB int      `json:"anidb_id"`
	TVDB  int      `json:"tvdb_id"`
	IMDb  imdbList `json:"imdb_id"`
	TMDB  struct {
		TV int `json:"tv"`
	} `json:"themoviedb_id"`
}

// imdbList is imdb_id: a list of ids, or null.
type imdbList []string

func (l *imdbList) UnmarshalJSON(b []byte) error {
	var list []string
	if err := json.Unmarshal(b, &list); err == nil {
		*l = list
		return nil
	}
	var one string
	if err := json.Unmarshal(b, &one); err == nil && one != "" {
		*l = []string{one}
	}
	return nil
}

func (l *AnimeList) get(ctx context.Context, url string, decode func(io.Reader) error) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	c := l.HTTP
	if c == nil {
		c = http.DefaultClient
	}
	resp, err := c.Do(req)
	if err != nil {
		return fmt.Errorf("anime mappings: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("anime mappings: %s answered %s", req.URL.Host, resp.Status)
	}
	if err := decode(resp.Body); err != nil {
		return fmt.Errorf("anime mappings: %s: %w", req.URL.Host, err)
	}
	return nil
}
