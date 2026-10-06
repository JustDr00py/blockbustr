package tmdb

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// ImageBase is TMDB's image CDN root; sizes are picked when serving (P1.17).
const ImageBase = "https://image.tmdb.org/t/p/original"

// ImageURL turns a TMDB file path ("/abc.jpg") into a full URL ("" stays "").
func ImageURL(path string) string {
	if path == "" {
		return ""
	}
	return ImageBase + path
}

// SearchMovie searches movies by title, optionally narrowed to a release year.
func (c *Client) SearchMovie(ctx context.Context, query string, year int) ([]Result, error) {
	q := url.Values{"query": {query}, "include_adult": {"false"}, "language": {c.language}}
	if year > 0 {
		q.Set("year", strconv.Itoa(year))
	}
	return c.search(ctx, "/search/movie", "movie", q)
}

// SearchTV searches shows by name, optionally narrowed to a first-air year.
func (c *Client) SearchTV(ctx context.Context, query string, year int) ([]Result, error) {
	q := url.Values{"query": {query}, "include_adult": {"false"}, "language": {c.language}}
	if year > 0 {
		q.Set("first_air_date_year", strconv.Itoa(year))
	}
	return c.search(ctx, "/search/tv", "tv", q)
}

func (c *Client) search(ctx context.Context, path, mediaType string, q url.Values) ([]Result, error) {
	var out struct {
		Results []Result `json:"results"`
	}
	if err := c.get(ctx, path, q, &out); err != nil {
		return nil, err
	}
	for i := range out.Results {
		out.Results[i].MediaType = mediaType
	}
	return out.Results, nil
}

// Credit is a cast or crew entry.
type Credit struct {
	ID          int    `json:"id"`
	Name        string `json:"name"`
	Character   string `json:"character"` // cast
	Job         string `json:"job"`       // crew: "Director", "Screenplay", "Writer", …
	Department  string `json:"department"`
	Order       int    `json:"order"`
	ProfilePath string `json:"profile_path"`
}

// Image is one poster, backdrop, logo or still.
type Image struct {
	FilePath    string  `json:"file_path"`
	Language    string  `json:"iso_639_1"` // "" (null) for textless art
	VoteAverage float64 `json:"vote_average"`
	Width       int     `json:"width"`
	Height      int     `json:"height"`
}

// Images groups a title's artwork.
type Images struct {
	Posters   []Image `json:"posters"`
	Backdrops []Image `json:"backdrops"`
	Logos     []Image `json:"logos"`
	Stills    []Image `json:"stills"`
}

// Named is a {"name": …} entry (genres, companies, networks).
type Named struct {
	Name string `json:"name"`
}

type credits struct {
	Cast []Credit `json:"cast"`
	Crew []Credit `json:"crew"`
}

// Movie is a movie's details with credits, images, ids and certifications.
type Movie struct {
	ID                  int     `json:"id"`
	IMDbID              string  `json:"imdb_id"`
	Title               string  `json:"title"`
	OriginalTitle       string  `json:"original_title"`
	OriginalLanguage    string  `json:"original_language"` // ISO 639-1, e.g. "en"
	Overview            string  `json:"overview"`
	Tagline             string  `json:"tagline"`
	ReleaseDate         string  `json:"release_date"`
	Runtime             int     `json:"runtime"` // minutes
	VoteAverage         float64 `json:"vote_average"`
	PosterPath          string  `json:"poster_path"`
	BackdropPath        string  `json:"backdrop_path"`
	Genres              []Named `json:"genres"`
	ProductionCompanies []Named `json:"production_companies"`
	Credits             credits `json:"credits"`
	Images              Images  `json:"images"`
	ReleaseDates        struct {
		Results []struct {
			Country string `json:"iso_3166_1"`
			Dates   []struct {
				Certification string `json:"certification"`
				Type          int    `json:"type"`
			} `json:"release_dates"`
		} `json:"results"`
	} `json:"release_dates"`
}

// Show is a TV show's details with credits, images, ids and content ratings.
type Show struct {
	ID                  int     `json:"id"`
	Name                string  `json:"name"`
	OriginalName        string  `json:"original_name"`
	OriginalLanguage    string  `json:"original_language"` // ISO 639-1, e.g. "ja"
	Overview            string  `json:"overview"`
	Tagline             string  `json:"tagline"`
	FirstAirDate        string  `json:"first_air_date"`
	LastAirDate         string  `json:"last_air_date"`
	Status              string  `json:"status"` // "Returning Series", "Ended", "Canceled"
	VoteAverage         float64 `json:"vote_average"`
	PosterPath          string  `json:"poster_path"`
	BackdropPath        string  `json:"backdrop_path"`
	Genres              []Named `json:"genres"`
	Networks            []Named `json:"networks"`
	ProductionCompanies []Named `json:"production_companies"`
	Credits             credits `json:"credits"`
	Images              Images  `json:"images"`
	ExternalIDs         struct {
		IMDbID string `json:"imdb_id"`
		TVDBID int    `json:"tvdb_id"`
	} `json:"external_ids"`
	ContentRatings struct {
		Results []struct {
			Country string `json:"iso_3166_1"`
			Rating  string `json:"rating"`
		} `json:"results"`
	} `json:"content_ratings"`
}

// SeasonDetails is one season with its episodes.
type SeasonDetails struct {
	SeasonNumber int    `json:"season_number"`
	Name         string `json:"name"`
	Overview     string `json:"overview"`
	AirDate      string `json:"air_date"`
	PosterPath   string `json:"poster_path"`
	Episodes     []struct {
		ID            int      `json:"id"`
		EpisodeNumber int      `json:"episode_number"`
		Name          string   `json:"name"`
		Overview      string   `json:"overview"`
		AirDate       string   `json:"air_date"`
		StillPath     string   `json:"still_path"`
		VoteAverage   float64  `json:"vote_average"`
		Runtime       int      `json:"runtime"`
		GuestStars    []Credit `json:"guest_stars"`
		Crew          []Credit `json:"crew"`
	} `json:"episodes"`
}

// imageLanguages asks for artwork in the metadata language plus textless art.
func (c *Client) imageLanguages() string {
	lang, _, _ := strings.Cut(c.language, "-")
	return lang + ",null"
}

// Country is the region of the metadata language ("en-US" → "US"), used to
// pick certifications.
func (c *Client) Country() string {
	if _, region, ok := strings.Cut(c.language, "-"); ok {
		return strings.ToUpper(region)
	}
	return "US"
}

// MovieDetails fetches a movie with credits, images, external ids and
// release dates in one request.
func (c *Client) MovieDetails(ctx context.Context, id int) (Movie, error) {
	var m Movie
	err := c.get(ctx, fmt.Sprintf("/movie/%d", id), url.Values{
		"language": {c.language}, "append_to_response": {"credits,images,release_dates"},
		"include_image_language": {c.imageLanguages()},
	}, &m)
	return m, err
}

// ShowDetails fetches a show with credits, images, external ids and content ratings.
func (c *Client) ShowDetails(ctx context.Context, id int) (Show, error) {
	var s Show
	err := c.get(ctx, fmt.Sprintf("/tv/%d", id), url.Values{
		"language": {c.language}, "append_to_response": {"credits,images,external_ids,content_ratings"},
		"include_image_language": {c.imageLanguages()},
	}, &s)
	return s, err
}

// Season fetches one season with its episodes.
func (c *Client) Season(ctx context.Context, showID, season int) (SeasonDetails, error) {
	var s SeasonDetails
	err := c.get(ctx, fmt.Sprintf("/tv/%d/season/%d", showID, season), url.Values{"language": {c.language}}, &s)
	return s, err
}

// Certification returns the movie's rating for country (e.g. "PG-13"),
// preferring the theatrical release (type 3).
func (m Movie) Certification(country string) string {
	for _, r := range m.ReleaseDates.Results {
		if r.Country != country {
			continue
		}
		best := ""
		for _, d := range r.Dates {
			if d.Certification == "" {
				continue
			}
			if d.Type == 3 {
				return d.Certification
			}
			if best == "" {
				best = d.Certification
			}
		}
		return best
	}
	return ""
}

// Rating returns the show's content rating for country (e.g. "TV-14").
func (s Show) Rating(country string) string {
	for _, r := range s.ContentRatings.Results {
		if r.Country == country {
			return r.Rating
		}
	}
	return ""
}

// BestImage picks the highest-voted image, preferring lang, then textless,
// then anything. ok is false if there are none.
func BestImage(images []Image, lang string) (Image, bool) {
	pick := func(match func(Image) bool) (Image, bool) {
		var best Image
		found := false
		for _, im := range images {
			if match(im) && (!found || im.VoteAverage > best.VoteAverage) {
				best, found = im, true
			}
		}
		return best, found
	}
	if im, ok := pick(func(i Image) bool { return i.Language == lang }); ok {
		return im, true
	}
	if im, ok := pick(func(i Image) bool { return i.Language == "" }); ok {
		return im, true
	}
	return pick(func(Image) bool { return true })
}

// FindByIMDb resolves an IMDb id (tt…) to TMDB movie and show ids (0 if none).
func (c *Client) FindByIMDb(ctx context.Context, imdbID string) (movieID, showID int, err error) {
	var out struct {
		MovieResults []Result `json:"movie_results"`
		TVResults    []Result `json:"tv_results"`
	}
	if err := c.get(ctx, "/find/"+url.PathEscape(imdbID), url.Values{"external_source": {"imdb_id"}}, &out); err != nil {
		return 0, 0, err
	}
	if len(out.MovieResults) > 0 {
		movieID = out.MovieResults[0].ID
	}
	if len(out.TVResults) > 0 {
		showID = out.TVResults[0].ID
	}
	return movieID, showID, nil
}
