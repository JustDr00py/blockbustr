package tmdb

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

const lucaJSON = `{"id":508943,"imdb_id":"tt12801262","title":"Luca","original_title":"Luca","overview":"Two sea monsters…",
"tagline":"Hello summer.","release_date":"2021-06-17","runtime":95,"vote_average":7.8,"poster_path":"/p.jpg","backdrop_path":"/b.jpg",
"genres":[{"id":16,"name":"Animation"},{"id":35,"name":"Comedy"}],"production_companies":[{"name":"Pixar"}],
"credits":{"cast":[{"id":1,"name":"Jacob Tremblay","character":"Luca Paguro (voice)","order":0,"profile_path":"/jt.jpg"}],
"crew":[{"id":2,"name":"Enrico Casarosa","job":"Director","department":"Directing"}]},
"images":{"posters":[{"file_path":"/it.jpg","iso_639_1":"it","vote_average":9},{"file_path":"/en.jpg","iso_639_1":"en","vote_average":5.2},{"file_path":"/none.jpg","iso_639_1":null,"vote_average":6}],
"backdrops":[{"file_path":"/bd.jpg","iso_639_1":null,"vote_average":5}],"logos":[{"file_path":"/logo.png","iso_639_1":"en","vote_average":5}]},
"release_dates":{"results":[{"iso_3166_1":"IT","release_dates":[{"certification":"T","type":4}]},
{"iso_3166_1":"US","release_dates":[{"certification":"","type":1},{"certification":"PG","type":4},{"certification":"PG","type":3}]}]}}`

func fake(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c := New("secret-key", "en-US")
	c.SetBaseURL(srv.URL)
	return c
}

func TestMovieDetails(t *testing.T) {
	var q map[string][]string
	c := fake(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/movie/508943" {
			t.Errorf("path %s", r.URL.Path)
		}
		q = r.URL.Query()
		_, _ = w.Write([]byte(lucaJSON))
	})
	m, err := c.MovieDetails(t.Context(), 508943)
	if err != nil {
		t.Fatal(err)
	}
	if q["append_to_response"][0] != "credits,images,release_dates" || q["include_image_language"][0] != "en,null" || q["api_key"][0] != "secret-key" {
		t.Errorf("query %v", q)
	}
	if m.Title != "Luca" || m.IMDbID != "tt12801262" || m.Runtime != 95 || len(m.Genres) != 2 || m.ProductionCompanies[0].Name != "Pixar" ||
		m.Credits.Cast[0].Character != "Luca Paguro (voice)" || m.Credits.Crew[0].Job != "Director" {
		t.Errorf("movie: %+v", m)
	}
	if got := m.Certification("US"); got != "PG" {
		t.Errorf("US certification = %q", got)
	}
	if got := m.Certification("IT"); got != "T" {
		t.Errorf("IT certification = %q", got)
	}
	if m.Certification("DE") != "" {
		t.Error("unknown country should be empty")
	}
	if im, _ := BestImage(m.Images.Posters, "en"); im.FilePath != "/en.jpg" {
		t.Errorf("poster for en = %s (language beats votes)", im.FilePath)
	}
	if im, _ := BestImage(m.Images.Posters, "fr"); im.FilePath != "/none.jpg" {
		t.Errorf("poster for fr = %s (textless next)", im.FilePath)
	}
	if _, ok := BestImage(nil, "en"); ok {
		t.Error("no images")
	}
	if ImageURL("/p.jpg") != "https://image.tmdb.org/t/p/original/p.jpg" || ImageURL("") != "" {
		t.Error("ImageURL")
	}
}

func TestShowAndSeason(t *testing.T) {
	c := fake(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/tv/235493":
			if r.URL.Query().Get("append_to_response") != "credits,images,external_ids,content_ratings" {
				t.Errorf("show query %v", r.URL.Query())
			}
			_, _ = w.Write([]byte(`{"id":235493,"name":"MF Ghost","first_air_date":"2023-10-02","status":"Returning Series",
				"networks":[{"name":"Tokyo MX"}],"external_ids":{"imdb_id":"tt27526575","tvdb_id":425707},
				"content_ratings":{"results":[{"iso_3166_1":"US","rating":"TV-14"}]}}`))
		case "/tv/235493/season/1":
			_, _ = w.Write([]byte(`{"season_number":1,"name":"Season 1","air_date":"2023-10-02","poster_path":"/s1.jpg",
				"episodes":[{"episode_number":1,"name":"The Challenger from England","air_date":"2023-10-02","still_path":"/e1.jpg","vote_average":7.5}]}`))
		default:
			http.NotFound(w, r)
		}
	})
	s, err := c.ShowDetails(t.Context(), 235493)
	if err != nil || s.Name != "MF Ghost" || s.ExternalIDs.TVDBID != 425707 || s.Rating("US") != "TV-14" || s.Networks[0].Name != "Tokyo MX" {
		t.Fatalf("show: %+v %v", s, err)
	}
	se, err := c.Season(t.Context(), 235493, 1)
	if err != nil || len(se.Episodes) != 1 || se.Episodes[0].Name != "The Challenger from England" || se.PosterPath != "/s1.jpg" {
		t.Fatalf("season: %+v %v", se, err)
	}
	if _, err := c.Season(t.Context(), 235493, 9); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing season: %v", err)
	}
}

func TestSearchByYear(t *testing.T) {
	var path string
	var q map[string][]string
	c := fake(t, func(w http.ResponseWriter, r *http.Request) {
		path, q = r.URL.Path, r.URL.Query()
		_, _ = w.Write([]byte(`{"results":[{"id":1,"title":"Luca","release_date":"2021-06-17"}]}`))
	})
	res, err := c.SearchMovie(t.Context(), "Luca", 2021)
	if err != nil || path != "/search/movie" || q["year"][0] != "2021" || res[0].MediaType != "movie" || res[0].Year() != 2021 {
		t.Errorf("movie search: %s %v %+v %v", path, q, res, err)
	}
	if _, err := c.SearchTV(t.Context(), "MF Ghost", 0); err != nil || path != "/search/tv" || q["first_air_date_year"] != nil {
		t.Errorf("tv search without year: %s %v %v", path, q, err)
	}
	if _, _ = c.SearchTV(t.Context(), "MF Ghost", 2023); q["first_air_date_year"][0] != "2023" {
		t.Errorf("tv search with year: %v", q)
	}
}

func TestRetriesAndErrorsDontLeakKey(t *testing.T) {
	var calls atomic.Int32
	c := fake(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/tv/1": // rate limited once, then fine
			if calls.Add(1) == 1 {
				w.Header().Set("Retry-After", "0")
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}
			_, _ = w.Write([]byte(`{"id":1,"name":"ok"}`))
		default:
			w.WriteHeader(http.StatusUnauthorized)
		}
	})
	if s, err := c.ShowDetails(t.Context(), 1); err != nil || s.Name != "ok" || calls.Load() != 2 {
		t.Errorf("429 retry: %+v %v (calls %d)", s, err, calls.Load())
	}
	if _, err := c.ShowDetails(t.Context(), 2); err == nil || strings.Contains(err.Error(), "secret-key") || !strings.Contains(err.Error(), "401") {
		t.Errorf("401 error: %v", err)
	}
	dead := New("secret-key", "en-US")
	dead.SetBaseURL("http://127.0.0.1:1")
	if _, err := dead.MovieDetails(t.Context(), 1); err == nil || strings.Contains(err.Error(), "secret-key") {
		t.Errorf("connection error leaks the key: %v", err)
	}
	if New("k", "de-DE").Country() != "DE" || New("k", "fr").Country() != "US" || New("k", "").Language() != "en-US" {
		t.Error("Country/Language")
	}
}
