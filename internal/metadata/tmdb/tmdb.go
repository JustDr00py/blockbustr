// Package tmdb is a client for The Movie Database (v3 API key).
//
// Search, Browse, Genres, TVSeasons, TVSeasonEpisodes, IMDbID and Details are
// ported unchanged from jellybird's internal/metadata/tmdb. blockbustr adds
// title+year searches and full detail calls (details.go) for library
// metadata (TASKS P1.16), plus rate limiting with 429 backoff in get.
package tmdb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/sysadmin/blockbustr/internal/ratelimit"
)

const baseURL = "https://api.themoviedb.org/3"

// Client talks to TMDB. Safe for concurrent use.
type Client struct {
	apiKey   string
	language string
	baseURL  string
	http     *http.Client
	limiter  *ratelimit.Limiter
}

// RequestsPerMinute keeps well under TMDB's documented ~50 requests/second.
const RequestsPerMinute = 1200

// maxRetries bounds retries after HTTP 429 or 5xx.
const maxRetries = 4

// New builds a TMDB client (v3 API key).
func New(apiKey, language string) *Client {
	if language == "" {
		language = "en-US"
	}
	return &Client{
		apiKey:   apiKey,
		language: language,
		baseURL:  baseURL,
		http:     &http.Client{Timeout: 20 * time.Second},
		limiter:  ratelimit.New(RequestsPerMinute),
	}
}

// Language is the metadata language, e.g. "en-US".
func (c *Client) Language() string { return c.language }

// SetBaseURL overrides the API root (self-hosted mirrors, tests).
func (c *Client) SetBaseURL(u string) { c.baseURL = strings.TrimSuffix(u, "/") }

// Result is one media search hit.
type Result struct {
	ID            int     `json:"id"`
	Title         string  `json:"title"` // movies
	Name          string  `json:"name"`  // tv
	OriginalTitle string  `json:"original_title"`
	OriginalName  string  `json:"original_name"`
	ReleaseDate   string  `json:"release_date"`   // movies
	FirstAirDate  string  `json:"first_air_date"` // tv
	MediaType     string  `json:"media_type"`
	Overview      string  `json:"overview"`
	PosterPath    string  `json:"poster_path"`
	VoteAverage   float64 `json:"vote_average"`
}

// Year extracts the release year.
func (r Result) Year() int {
	d := r.ReleaseDate
	if d == "" {
		d = r.FirstAirDate
	}
	if len(d) >= 4 {
		var y int
		if _, err := fmt.Sscanf(d[:4], "%d", &y); err == nil {
			return y
		}
	}
	return 0
}

// DisplayTitle picks the best title field.
func (r Result) DisplayTitle() string {
	if r.Title != "" {
		return r.Title
	}
	return r.Name
}

// Search finds movies and shows matching query (multi-search).
func (c *Client) Search(ctx context.Context, query string) ([]Result, error) {
	var out struct {
		Results []Result `json:"results"`
	}
	err := c.get(ctx, "/search/multi", url.Values{
		"query":         {query},
		"include_adult": {"false"},
		"language":      {c.language},
	}, &out)
	if err != nil {
		return nil, err
	}
	// Keep only movies and shows; filter no-person results.
	var filtered []Result
	for _, r := range out.Results {
		if r.MediaType == "movie" || r.MediaType == "tv" {
			filtered = append(filtered, r)
		}
	}
	return filtered, nil
}

// Page is one page of a browsable TMDB list.
type Page struct {
	Results    []Result `json:"results"`
	Page       int      `json:"page"`
	TotalPages int      `json:"total_pages"`
}

// Lists maps each media type to the named lists Browse accepts, in display
// order, with their TMDB paths. "trending" is weekly trending.
var Lists = map[string][]struct{ Name, Label, Path string }{
	"movie": {
		{"trending", "Trending", "/trending/movie/week"},
		{"popular", "Popular", "/movie/popular"},
		{"now_playing", "Now playing", "/movie/now_playing"},
		{"upcoming", "Upcoming", "/movie/upcoming"},
		{"top_rated", "Top rated", "/movie/top_rated"},
	},
	"tv": {
		{"trending", "Trending", "/trending/tv/week"},
		{"popular", "Popular", "/tv/popular"},
		{"on_the_air", "On the air", "/tv/on_the_air"},
		{"airing_today", "Airing today", "/tv/airing_today"},
		{"top_rated", "Top rated", "/tv/top_rated"},
	},
}

// Browse fetches one page of a named list (see Lists) for mediaType "movie"
// or "tv". A non-zero genreID ignores list and uses TMDB's discover endpoint
// instead, sorted by popularity.
func (c *Client) Browse(ctx context.Context, mediaType, list string, genreID, page int) (Page, error) {
	if mediaType != "movie" && mediaType != "tv" {
		return Page{}, fmt.Errorf("tmdb: unknown media type %q", mediaType)
	}
	if page < 1 {
		page = 1
	}
	q := url.Values{"language": {c.language}, "page": {fmt.Sprint(page)}}
	var path string
	if genreID > 0 {
		path = "/discover/" + mediaType
		q.Set("with_genres", fmt.Sprint(genreID))
		q.Set("sort_by", "popularity.desc")
		q.Set("include_adult", "false")
	} else {
		for _, l := range Lists[mediaType] {
			if l.Name == list {
				path = l.Path
			}
		}
		if path == "" {
			return Page{}, fmt.Errorf("tmdb: unknown %s list %q", mediaType, list)
		}
	}
	var out Page
	if err := c.get(ctx, path, q, &out); err != nil {
		return Page{}, err
	}
	// Only /trending sets media_type; fill it in so callers can treat
	// every list alike.
	for i := range out.Results {
		out.Results[i].MediaType = mediaType
	}
	return out, nil
}

// Genre is one TMDB genre.
type Genre struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

// Genres lists the genres TMDB uses for mediaType ("movie" or "tv").
func (c *Client) Genres(ctx context.Context, mediaType string) ([]Genre, error) {
	if mediaType != "movie" && mediaType != "tv" {
		return nil, fmt.Errorf("tmdb: unknown media type %q", mediaType)
	}
	var out struct {
		Genres []Genre `json:"genres"`
	}
	if err := c.get(ctx, "/genre/"+mediaType+"/list", url.Values{"language": {c.language}}, &out); err != nil {
		return nil, err
	}
	return out.Genres, nil
}

// Season is one entry from a TV show's season list.
type Season struct {
	SeasonNumber int    `json:"season_number"`
	Name         string `json:"name"`
	EpisodeCount int    `json:"episode_count"`
	PosterPath   string `json:"poster_path"`
	AirDate      string `json:"air_date"`
}

// Episode is one entry from a season's episode list.
type Episode struct {
	EpisodeNumber int    `json:"episode_number"`
	Name          string `json:"name"`
	Overview      string `json:"overview"`
	StillPath     string `json:"still_path"`
	AirDate       string `json:"air_date"`
}

// TVSeasons lists a show's seasons (from its details endpoint), dropping
// season 0 ("Specials") since that's not what a browsing user expects.
func (c *Client) TVSeasons(ctx context.Context, tvID int) ([]Season, error) {
	var out struct {
		Seasons []Season `json:"seasons"`
	}
	if err := c.get(ctx, fmt.Sprintf("/tv/%d", tvID), url.Values{"language": {c.language}}, &out); err != nil {
		return nil, err
	}
	var filtered []Season
	for _, s := range out.Seasons {
		if s.SeasonNumber > 0 {
			filtered = append(filtered, s)
		}
	}
	return filtered, nil
}

// TVSeasonEpisodes lists episodes for one season of a show.
func (c *Client) TVSeasonEpisodes(ctx context.Context, tvID, season int) ([]Episode, error) {
	var out struct {
		Episodes []Episode `json:"episodes"`
	}
	path := fmt.Sprintf("/tv/%d/season/%d", tvID, season)
	if err := c.get(ctx, path, url.Values{"language": {c.language}}, &out); err != nil {
		return nil, err
	}
	return out.Episodes, nil
}

// IMDbID resolves the IMDb identifier for a TMDB movie/show id.
func (c *Client) IMDbID(ctx context.Context, mediaType string, tmdbID int) (string, error) {
	var out struct {
		IMDbID string `json:"imdb_id"`
	}
	var path string
	switch mediaType {
	case "tv":
		path = fmt.Sprintf("/tv/%d/external_ids", tmdbID)
	default:
		path = fmt.Sprintf("/movie/%d/external_ids", tmdbID)
	}
	if err := c.get(ctx, path, nil, &out); err != nil {
		return "", err
	}
	if out.IMDbID == "" {
		return "", fmt.Errorf("tmdb: no imdb id for %s %d", mediaType, tmdbID)
	}
	return out.IMDbID, nil
}

// Details fetches the canonical title/year for a known TMDB id, used to
// build a naming hint (e.g. for watchlist-driven adds) without a text search.
func (c *Client) Details(ctx context.Context, mediaType string, tmdbID int) (Result, error) {
	var path string
	if mediaType == "tv" {
		path = fmt.Sprintf("/tv/%d", tmdbID)
	} else {
		path = fmt.Sprintf("/movie/%d", tmdbID)
	}
	var r Result
	if err := c.get(ctx, path, url.Values{"language": {c.language}}, &r); err != nil {
		return Result{}, err
	}
	r.MediaType = mediaType
	return r, nil
}

func (c *Client) get(ctx context.Context, path string, q url.Values, out any) error {
	if q == nil {
		q = url.Values{}
	}
	q.Set("api_key", c.apiKey)
	for attempt := 0; ; attempt++ {
		if err := c.limiter.Wait(ctx); err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path+"?"+q.Encode(), nil)
		if err != nil {
			return err
		}
		resp, err := c.http.Do(req)
		if err != nil {
			// *url.Error embeds the URL, which carries the api_key: report the path only.
			return fmt.Errorf("tmdb: GET %s: %w", path, unwrapURLError(err))
		}
		retryable := resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500
		if retryable && attempt < maxRetries {
			retryAfter, _ := strconv.Atoi(resp.Header.Get("Retry-After"))
			_ = resp.Body.Close()
			if err := ratelimit.Backoff(ctx, attempt, time.Duration(retryAfter)*time.Second); err != nil {
				return err
			}
			continue
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode == http.StatusNotFound {
			return fmt.Errorf("tmdb: %s: %w", path, ErrNotFound)
		}
		if resp.StatusCode >= 400 {
			return fmt.Errorf("tmdb: HTTP %d for %s", resp.StatusCode, path)
		}
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return fmt.Errorf("tmdb: decode %s: %w", path, err)
		}
		return nil
	}
}

// ErrNotFound is TMDB's 404 (unknown id).
var ErrNotFound = errors.New("not found")

func unwrapURLError(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}
