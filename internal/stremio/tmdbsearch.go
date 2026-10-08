package stremio

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sysadmin/blockbustr/internal/cache"
	"github.com/sysadmin/blockbustr/internal/metadata/tmdb"
)

// TMDBSearch is TMDB as a search source (DESIGN §7.4): the primary one when
// a key is set. Results are TMDB-keyed, but stream and subtitle addons look
// titles up by IMDb id, so each is mapped through /external_ids (in
// parallel, cached for a month: the mapping never changes); results without
// an IMDb id are dropped, as they couldn't play.
type TMDBSearch struct {
	Client *tmdb.Client
	Cache  *cache.Cache
	// Max is how many results are mapped per search (default 12).
	Max int
}

// enabled: t can search (a client with a key; the key can be set later).
func (t *TMDBSearch) enabled() bool { return t != nil && t.Client.Enabled() }

// imdbTTL caches TMDB→IMDb mappings; a missing one is retried after a day
// (TMDB fills them in for new titles).
const (
	imdbTTL     = 30 * 24 * time.Hour
	imdbMissTTL = 24 * time.Hour
)

// Search returns movie and series results for term, keyed by IMDb id, in
// TMDB's order.
func (s *TMDBSearch) Search(ctx context.Context, term string) ([]found, error) {
	key := cache.SearchKey("tmdb", "multi", term)
	var out []found
	if s.Cache != nil {
		if ok, err := s.Cache.GetJSON(ctx, key, &out); err == nil && ok {
			return out, nil
		}
	}
	results, err := s.Client.Search(ctx, term)
	if err != nil {
		return nil, err
	}
	limit := s.Max
	if limit <= 0 {
		limit = 12
	}
	results = results[:min(limit, len(results))]
	imdbs := make([]string, len(results))
	var wg sync.WaitGroup
	for i, r := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			imdbs[i] = s.imdb(ctx, r.MediaType, r.ID)
		}()
	}
	wg.Wait()
	out = []found{}
	for i, r := range results {
		if imdbs[i] == "" {
			continue
		}
		kind := "movie"
		if r.MediaType == "tv" {
			kind = "series"
		}
		m := Meta{ID: imdbs[i], Type: kind, Name: r.DisplayTitle(), Description: r.Overview, MovieDBID: Text(strconv.Itoa(r.ID))}
		if y := r.Year(); y > 0 {
			m.ReleaseInfo = Text(strconv.Itoa(y))
		}
		if d := r.ReleaseDate + r.FirstAirDate; len(d) >= 10 {
			m.Released = d[:10]
		}
		if r.PosterPath != "" {
			m.Poster = tmdb.ImageBase + r.PosterPath
		}
		if r.BackdropPath != "" {
			m.Background = tmdb.ImageBase + r.BackdropPath
		}
		out = append(out, found{Type: kind, Meta: m})
	}
	if s.Cache != nil && ctx.Err() == nil {
		_ = s.Cache.SetJSON(ctx, key, out, cache.SearchTTL)
	}
	return out, nil
}

// imdb maps a TMDB movie/tv id to its IMDb id ("" when it has none or the
// lookup failed).
func (s *TMDBSearch) imdb(ctx context.Context, mediaType string, id int) string {
	key := cache.Key("tmdb:imdb:" + mediaType + ":" + strconv.Itoa(id))
	var imdb string
	if s.Cache != nil {
		if ok, err := s.Cache.GetJSON(ctx, key, &imdb); err == nil && ok {
			return imdb
		}
	}
	imdb, err := s.Client.IMDbID(ctx, mediaType, id)
	ttl := imdbTTL
	switch {
	case err != nil && strings.Contains(err.Error(), "no imdb id"):
		imdb, ttl = "", imdbMissTTL
	case err != nil:
		return "" // transient (rate limit, timeout): not cached
	}
	if s.Cache != nil {
		_ = s.Cache.SetJSON(ctx, key, imdb, ttl)
	}
	return imdb
}
