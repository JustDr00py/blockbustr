package nfo

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

const movieNFO = `<?xml version="1.0" encoding="UTF-8" standalone="yes" ?>
<movie>
  <title>Luca</title>
  <originaltitle>Luca</originaltitle>
  <sorttitle>Luca</sorttitle>
  <ratings><rating name="imdb" max="10"><value>7.4</value></rating><rating name="themoviedb" max="10" default="true"><value>7.8</value></rating></ratings>
  <plot>On the Italian Riviera, an unlikely but strong friendship…</plot>
  <tagline>Hello summer.</tagline>
  <mpaa>US:PG</mpaa>
  <uniqueid type="tmdb" default="true">508943</uniqueid>
  <uniqueid type="imdb">tt12801262</uniqueid>
  <genre>Animation</genre><genre>Comedy / Family</genre><genre>comedy</genre>
  <studio>Pixar</studio>
  <director>Enrico Casarosa</director>
  <credits>Jesse Andrews</credits><credits>Mike Jones</credits>
  <premiered>2021-06-17</premiered>
  <thumb aspect="poster">https://image.tmdb.org/t/p/original/p.jpg</thumb>
  <thumb aspect="clearlogo">https://image.tmdb.org/t/p/original/logo.png</thumb>
  <fanart><thumb>https://image.tmdb.org/t/p/original/b.jpg</thumb></fanart>
  <actor><name>Jacob Tremblay</name><role>Luca Paguro (voice)</role><order>0</order><thumb>https://x/jt.jpg</thumb></actor>
  <actor><name>Jack Dylan Grazer</name><role>Alberto Scorfano (voice)</role><order>1</order></actor>
</movie>`

func TestParseMovie(t *testing.T) {
	i, err := Parse([]byte(movieNFO))
	if err != nil {
		t.Fatal(err)
	}
	want := Info{
		Kind: "movie", Title: "Luca", OriginalTitle: "Luca", SortTitle: "Luca", Year: 2021, Premiered: "2021-06-17",
		Plot: "On the Italian Riviera, an unlikely but strong friendship…", Tagline: "Hello summer.", Certification: "PG",
		Rating: 7.8, TMDBID: "508943", IMDbID: "tt12801262",
		Genres: []string{"Animation", "Comedy", "Family"}, Studios: []string{"Pixar"},
		Directors: []string{"Enrico Casarosa"}, Writers: []string{"Jesse Andrews", "Mike Jones"},
		Actors:  []Actor{{Name: "Jacob Tremblay", Role: "Luca Paguro (voice)", Thumb: "https://x/jt.jpg"}, {Name: "Jack Dylan Grazer", Role: "Alberto Scorfano (voice)", Order: 1}},
		Posters: []string{"https://image.tmdb.org/t/p/original/p.jpg"}, Fanart: []string{"https://image.tmdb.org/t/p/original/b.jpg"},
	}
	if !reflect.DeepEqual(i, want) {
		t.Errorf("got  %+v\nwant %+v", i, want)
	}
}

func TestParseShowAndEpisode(t *testing.T) {
	show, err := Parse([]byte(`<tvshow><title>MF Ghost</title><year>2023</year><id>425707</id><mpaa>TV-14</mpaa><rating>7.1</rating></tvshow>`))
	if err != nil || show.Kind != "tvshow" || show.TVDBID != "425707" || show.Year != 2023 || show.Certification != "TV-14" || show.Rating != 7.1 {
		t.Errorf("tvshow: %+v %v", show, err)
	}
	ep, err := Parse([]byte("\xef\xbb\xbf<episodedetails><title>Teamwork</title><season>1</season><episode>5</episode><aired>2023-10-30</aired><uniqueid type=\"tmdb\">4740001</uniqueid></episodedetails>"))
	if err != nil || ep.Kind != "episodedetails" || ep.Season != 1 || ep.Episode != 5 || ep.Premiered != "2023-10-30" || ep.Year != 2023 || ep.TMDBID != "4740001" {
		t.Errorf("episode: %+v %v", ep, err)
	}
}

func TestURLOnlyAndBadFiles(t *testing.T) {
	cases := map[string]Info{
		"https://www.imdb.com/title/tt12801262/\n":     {IMDbID: "tt12801262"},
		"https://www.themoviedb.org/movie/508943-luca": {TMDBID: "508943"},
	}
	for in, want := range cases {
		got, err := Parse([]byte(in))
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("Parse(%q) = %+v, %v", in, got, err)
		}
	}
	// XML plus a trailing link: keep the XML fields and take the id from the link.
	got, err := Parse([]byte("<movie><title>X</title></movie>\nhttps://imdb.com/title/tt0133093"))
	if err != nil || got.Kind != "movie" || got.Title != "X" || got.IMDbID != "tt0133093" {
		t.Errorf("xml + link: %+v %v", got, err)
	}
	// Broken XML still yields ids from links.
	got, err = Parse([]byte("<movie><title>X</titl\nhttps://www.themoviedb.org/movie/603"))
	if err != nil || got.TMDBID != "603" {
		t.Errorf("broken xml + link: %+v %v", got, err)
	}
	for _, bad := range []string{"", "just some notes", "<musicvideo><title>x</title></musicvideo>"} {
		if _, err := Parse([]byte(bad)); !errors.Is(err, ErrNotNFO) {
			t.Errorf("Parse(%q) error = %v", bad, err)
		}
	}
	if c := certification("Rated PG-13"); c != "PG-13" {
		t.Errorf("certification = %q", c)
	}
}

func TestReadFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "movie.nfo")
	if err := os.WriteFile(p, []byte(movieNFO), 0o600); err != nil {
		t.Fatal(err)
	}
	if i, err := ReadFile(p); err != nil || i.Title != "Luca" {
		t.Errorf("ReadFile: %+v %v", i.Title, err)
	}
	if _, err := ReadFile(p + ".x"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("missing: %v", err)
	}
}
