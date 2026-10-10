// Package metadata fills library items with descriptive metadata (TASKS
// P1.16, DESIGN §6 step 6): a local Kodi .nfo first, then TMDB.
package metadata

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sysadmin/blockbustr/internal/images"
	"github.com/sysadmin/blockbustr/internal/library"
	"github.com/sysadmin/blockbustr/internal/metadata/nfo"
	"github.com/sysadmin/blockbustr/internal/metadata/tmdb"
	"github.com/sysadmin/blockbustr/internal/store/pg/db"
)

const (
	// MatchedTTL is how long applied metadata is kept before a refresh.
	MatchedTTL = 30 * 24 * time.Hour
	// UnmatchedRetry is how long an item without a match waits for a retry.
	UnmatchedRetry = 7 * 24 * time.Hour
	// maxCast bounds the actors stored per item.
	maxCast = 25
)

// Refresher applies metadata to a library's items.
type Refresher struct {
	q    *db.Queries
	pool *pgxpool.Pool
	tmdb *tmdb.Client // nil or without a key: nfo files only
	log  *slog.Logger
}

// NewRefresher returns a Refresher; client may be nil when no TMDB key is set.
func NewRefresher(pool *pgxpool.Pool, client *tmdb.Client, log *slog.Logger) *Refresher {
	return &Refresher{q: db.New(pool), pool: pool, tmdb: client, log: log}
}

// Meta is the merged metadata for one item.
type Meta struct {
	Name, OriginalTitle, Overview, Tagline, Rating string
	CommunityRating                                float64
	Premiere, End                                  string // YYYY-MM-DD
	Year, RuntimeMinutes                           int
	OriginalLanguage                               string // ISO 639-1, movies and series only
	ProviderIDs                                    map[string]string
	Genres, Studios                                []string
	People                                         []Person
	Images                                         []Image
	Source                                         string // "tmdb", "nfo", "nfo+tmdb"
}

// Person is one credit.
type Person struct {
	Name, Role, Kind, ImageURL string
	TMDBID, Order              int
}

// Image is one artwork URL of a Jellyfin ImageType ("Primary", "Backdrop", "Logo").
type Image struct{ Type, URL string }

func (m Meta) empty() bool { return m.Source == "" }

// Stats summarises one refresh.
type Stats struct{ Matched, Unmatched, Failed int }

// Refresh applies metadata to every item of lib that needs it.
func (r *Refresher) Refresh(ctx context.Context, lib db.Library) error {
	now := time.Now()
	rows, err := r.q.ItemsNeedingMetadata(ctx, db.ItemsNeedingMetadataParams{
		LibraryID: lib.ID, MatchedBefore: ptr(now.Add(-MatchedTTL)), UnmatchedBefore: ptr(now.Add(-UnmatchedRetry)),
	})
	if err != nil || len(rows) == 0 {
		return err
	}
	run := &run{r: r, items: map[uuid.UUID]db.Item{}, seasons: map[[2]int]*tmdb.SeasonDetails{}}
	var st Stats
	for _, row := range rows {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		m, err := run.item(ctx, row)
		switch {
		case err != nil:
			// Transient (network, TMDB outage): leave the item for the next run.
			st.Failed++
			r.log.Warn("metadata lookup failed", "item", row.Name, "type", row.Type, "err", err)
		case m.empty():
			st.Unmatched++
			if err := r.q.MarkMetadataUnmatched(ctx, row.ID); err != nil {
				return err
			}
		default:
			st.Matched++
			if err := r.apply(ctx, row, m); err != nil {
				return fmt.Errorf("metadata %s: %w", row.Name, err)
			}
			delete(run.items, row.ID) // re-read with fresh provider ids if a child needs it
		}
	}
	r.log.Info("metadata refreshed", "library", lib.Name, "matched", st.Matched, "unmatched", st.Unmatched,
		"failed", st.Failed, "tmdb", r.tmdb.Enabled(), "took", time.Since(now).Round(time.Millisecond))
	return nil
}

type run struct {
	r       *Refresher
	items   map[uuid.UUID]db.Item          // parent lookups
	seasons map[[2]int]*tmdb.SeasonDetails // (show, season) → details, fetched once per run
}

func (u *run) item(ctx context.Context, row db.ItemsNeedingMetadataRow) (Meta, error) {
	switch row.Type {
	case "Movie":
		return u.movie(ctx, row)
	case "Series":
		return u.series(ctx, row)
	case "Season":
		return u.season(ctx, row)
	case "Episode":
		return u.episode(ctx, row)
	}
	return Meta{}, nil
}

func (u *run) movie(ctx context.Context, row db.ItemsNeedingMetadataRow) (Meta, error) {
	path := deref(row.Path)
	local := readNFO(stemPath(path)+".nfo", filepath.Join(filepath.Dir(path), "movie.nfo"))
	var m Meta
	if c := u.r.tmdb; c.Enabled() {
		id := atoi(providerID(row.ProviderIds, "Tmdb"))
		if local != nil && id == 0 {
			id = atoi(local.TMDBID)
			if id == 0 && local.IMDbID != "" {
				mid, _, err := c.FindByIMDb(ctx, local.IMDbID)
				if err != nil {
					return Meta{}, err
				}
				id = mid
			}
		}
		if imdb := providerID(row.ProviderIds, "Imdb"); id == 0 && imdb != "" { // Stremio catalog items
			mid, _, err := c.FindByIMDb(ctx, imdb)
			if err != nil {
				return Meta{}, err
			}
			id = mid
		}
		if id == 0 {
			res, err := c.SearchMovie(ctx, row.Name, int(deref(row.ProductionYear)))
			if err != nil {
				return Meta{}, err
			}
			id = bestMatch(res, row.Name, int(deref(row.ProductionYear)))
		}
		if id != 0 {
			det, err := c.MovieDetails(ctx, id)
			if err != nil && !errors.Is(err, tmdb.ErrNotFound) {
				return Meta{}, err
			}
			if err == nil {
				m = fromMovie(det, c)
			}
		}
	}
	return merge(m, local), nil
}

func (u *run) series(ctx context.Context, row db.ItemsNeedingMetadataRow) (Meta, error) {
	local := readNFO(filepath.Join(deref(row.Path), "tvshow.nfo"))
	var m Meta
	if c := u.r.tmdb; c.Enabled() {
		id := atoi(providerID(row.ProviderIds, "Tmdb"))
		if local != nil && id == 0 {
			id = atoi(local.TMDBID)
			if id == 0 && local.IMDbID != "" {
				_, sid, err := c.FindByIMDb(ctx, local.IMDbID)
				if err != nil {
					return Meta{}, err
				}
				id = sid
			}
		}
		if imdb := providerID(row.ProviderIds, "Imdb"); id == 0 && imdb != "" { // Stremio catalog items
			_, sid, err := c.FindByIMDb(ctx, imdb)
			if err != nil {
				return Meta{}, err
			}
			id = sid
		}
		if id == 0 {
			res, err := c.SearchTV(ctx, row.Name, int(deref(row.ProductionYear)))
			if err != nil {
				return Meta{}, err
			}
			id = bestMatch(res, row.Name, int(deref(row.ProductionYear)))
		}
		if id != 0 {
			det, err := c.ShowDetails(ctx, id)
			if err != nil && !errors.Is(err, tmdb.ErrNotFound) {
				return Meta{}, err
			}
			if err == nil {
				m = fromShow(det, c)
			}
		}
	}
	return merge(m, local), nil
}

func (u *run) season(ctx context.Context, row db.ItemsNeedingMetadataRow) (Meta, error) {
	showID, err := u.showID(ctx, row.ParentID)
	if err != nil || showID == 0 || row.IndexNumber == nil {
		return Meta{}, err
	}
	s, err := u.seasonDetails(ctx, showID, int(*row.IndexNumber))
	if err != nil || s == nil {
		return Meta{}, err
	}
	m := Meta{Overview: s.Overview, Premiere: s.AirDate, Year: yearOf(s.AirDate), Source: "tmdb"}
	// TMDB often names a season after the show ("MF GHOST" for season 1),
	// which reads badly in a season list; keep "Season N" then.
	if series, err := u.parent(ctx, *row.ParentID); err == nil && normTitle(s.Name) != normTitle(series.Name) {
		m.Name = s.Name
	}
	if s.PosterPath != "" {
		m.Images = []Image{{"Primary", tmdb.ImageURL(s.PosterPath)}}
	}
	return m, nil
}

func (u *run) episode(ctx context.Context, row db.ItemsNeedingMetadataRow) (Meta, error) {
	local := readNFO(stemPath(deref(row.Path)) + ".nfo")
	var m Meta
	if row.IndexNumber != nil && row.ParentID != nil {
		seasonItem, err := u.parent(ctx, *row.ParentID)
		if err != nil {
			return Meta{}, err
		}
		showID, err := u.showID(ctx, seasonItem.ParentID)
		if err != nil {
			return Meta{}, err
		}
		if showID != 0 && seasonItem.IndexNumber != nil {
			s, err := u.seasonDetails(ctx, showID, int(*seasonItem.IndexNumber))
			if err != nil {
				return Meta{}, err
			}
			if s != nil {
				m = fromEpisode(s, int(*row.IndexNumber))
			}
		}
	}
	return merge(m, local), nil
}

// showID is the TMDB id of the series item seriesID (0 if unmatched).
func (u *run) showID(ctx context.Context, seriesID *uuid.UUID) (int, error) {
	if seriesID == nil || !u.r.tmdb.Enabled() {
		return 0, nil
	}
	s, err := u.parent(ctx, *seriesID)
	if err != nil {
		return 0, err
	}
	return atoi(providerID(s.ProviderIds, "Tmdb")), nil
}

func (u *run) parent(ctx context.Context, id uuid.UUID) (db.Item, error) {
	if it, ok := u.items[id]; ok {
		return it, nil
	}
	it, err := u.r.q.GetItem(ctx, id)
	if err != nil {
		return db.Item{}, err
	}
	u.items[id] = it
	return it, nil
}

func (u *run) seasonDetails(ctx context.Context, showID, n int) (*tmdb.SeasonDetails, error) {
	key := [2]int{showID, n}
	if s, ok := u.seasons[key]; ok {
		return s, nil
	}
	s, err := u.r.tmdb.Season(ctx, showID, n)
	if errors.Is(err, tmdb.ErrNotFound) {
		u.seasons[key] = nil
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	u.seasons[key] = &s
	return &s, nil
}

func fromMovie(d tmdb.Movie, c *tmdb.Client) Meta {
	lang, _, _ := strings.Cut(c.Language(), "-")
	m := Meta{
		Name: d.Title, Overview: d.Overview, Tagline: d.Tagline, Rating: d.Certification(c.Country()),
		CommunityRating: d.VoteAverage, Premiere: d.ReleaseDate, Year: yearOf(d.ReleaseDate), RuntimeMinutes: d.Runtime,
		OriginalLanguage: d.OriginalLanguage,
		ProviderIDs:      ids("Tmdb", strconv.Itoa(d.ID), "Imdb", d.IMDbID), Genres: names(d.Genres), Studios: names(d.ProductionCompanies),
		People: credits(d.Credits.Cast, d.Credits.Crew), Images: artwork(d.Images, d.PosterPath, d.BackdropPath, lang), Source: "tmdb",
	}
	if d.OriginalTitle != d.Title {
		m.OriginalTitle = d.OriginalTitle
	}
	return m
}

func fromShow(d tmdb.Show, c *tmdb.Client) Meta {
	lang, _, _ := strings.Cut(c.Language(), "-")
	tvdb := ""
	if d.ExternalIDs.TVDBID != 0 {
		tvdb = strconv.Itoa(d.ExternalIDs.TVDBID)
	}
	m := Meta{
		Name: d.Name, Overview: d.Overview, Tagline: d.Tagline, Rating: d.Rating(c.Country()),
		CommunityRating: d.VoteAverage, Premiere: d.FirstAirDate, Year: yearOf(d.FirstAirDate),
		OriginalLanguage: d.OriginalLanguage,
		ProviderIDs:      ids("Tmdb", strconv.Itoa(d.ID), "Imdb", d.ExternalIDs.IMDbID, "Tvdb", tvdb),
		// Jellyfin lists a show's networks as its studios.
		Genres: names(d.Genres), Studios: names(d.Networks),
		People: credits(d.Credits.Cast, nil), Images: artwork(d.Images, d.PosterPath, d.BackdropPath, lang), Source: "tmdb",
	}
	if d.OriginalName != d.Name {
		m.OriginalTitle = d.OriginalName
	}
	if d.Status == "Ended" || d.Status == "Canceled" {
		m.End = d.LastAirDate
	}
	return m
}

func fromEpisode(s *tmdb.SeasonDetails, n int) Meta {
	for _, e := range s.Episodes {
		if e.EpisodeNumber != n {
			continue
		}
		m := Meta{
			Name: e.Name, Overview: e.Overview, Premiere: e.AirDate, Year: yearOf(e.AirDate),
			CommunityRating: e.VoteAverage, RuntimeMinutes: e.Runtime, Source: "tmdb",
			ProviderIDs: ids("Tmdb", strconv.Itoa(e.ID)),
		}
		for i, g := range e.GuestStars {
			m.People = append(m.People, Person{Name: g.Name, Role: g.Character, Kind: "GuestStar", TMDBID: g.ID, ImageURL: tmdb.ImageURL(g.ProfilePath), Order: i})
		}
		m.People = append(m.People, credits(nil, e.Crew)...)
		if e.StillPath != "" {
			m.Images = []Image{{"Primary", tmdb.ImageURL(e.StillPath)}}
		}
		return m
	}
	return Meta{}
}

var writerJobs = map[string]bool{"Screenplay": true, "Writer": true, "Story": true, "Novel": true, "Teleplay": true, "Author": true}

func credits(cast, crew []tmdb.Credit) []Person {
	var out []Person
	for i, c := range cast {
		if i >= maxCast {
			break
		}
		out = append(out, Person{Name: c.Name, Role: c.Character, Kind: "Actor", TMDBID: c.ID, ImageURL: tmdb.ImageURL(c.ProfilePath), Order: c.Order})
	}
	seen := map[string]bool{}
	for i, c := range crew {
		kind := ""
		switch {
		case c.Job == "Director":
			kind = "Director"
		case writerJobs[c.Job]:
			kind = "Writer"
		default:
			continue
		}
		key := kind + strconv.Itoa(c.ID)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, Person{Name: c.Name, Role: c.Job, Kind: kind, TMDBID: c.ID, ImageURL: tmdb.ImageURL(c.ProfilePath), Order: i})
	}
	return out
}

func artwork(im tmdb.Images, poster, backdrop, lang string) []Image {
	var out []Image
	if p, ok := tmdb.BestImage(im.Posters, lang); ok {
		out = append(out, Image{"Primary", tmdb.ImageURL(p.FilePath)})
	} else if poster != "" {
		out = append(out, Image{"Primary", tmdb.ImageURL(poster)})
	}
	// Backdrops look best without text: prefer textless (language "") first.
	if b, ok := tmdb.BestImage(im.Backdrops, ""); ok {
		out = append(out, Image{"Backdrop", tmdb.ImageURL(b.FilePath)})
	} else if backdrop != "" {
		out = append(out, Image{"Backdrop", tmdb.ImageURL(backdrop)})
	}
	// TMDB has some logos only as SVG, which the image store can't decode.
	var logos []tmdb.Image
	for _, l := range im.Logos {
		if !strings.HasSuffix(strings.ToLower(l.FilePath), ".svg") {
			logos = append(logos, l)
		}
	}
	if l, ok := tmdb.BestImage(logos, lang); ok {
		out = append(out, Image{"Logo", tmdb.ImageURL(l.FilePath)})
	}
	return out
}

// merge lays a local .nfo over TMDB metadata: the nfo wins wherever it has a value.
func merge(m Meta, n *nfo.Info) Meta {
	if n == nil {
		return m
	}
	had := !m.empty()
	set := func(dst *string, v string) {
		if v != "" {
			*dst = v
		}
	}
	set(&m.Name, n.Title)
	set(&m.OriginalTitle, n.OriginalTitle)
	set(&m.Overview, n.Plot)
	set(&m.Tagline, n.Tagline)
	set(&m.Rating, n.Certification)
	set(&m.Premiere, n.Premiered)
	if n.Year != 0 {
		m.Year = n.Year
	}
	if n.Rating > 0 {
		m.CommunityRating = n.Rating
	}
	if m.ProviderIDs == nil {
		m.ProviderIDs = map[string]string{}
	}
	for k, v := range map[string]string{"Tmdb": n.TMDBID, "Imdb": n.IMDbID, "Tvdb": n.TVDBID} {
		if v != "" {
			m.ProviderIDs[k] = v
		}
	}
	if len(n.Genres) > 0 {
		m.Genres = n.Genres
	}
	if len(n.Studios) > 0 {
		m.Studios = n.Studios
	}
	if len(n.Actors) > 0 || len(n.Directors) > 0 || len(n.Writers) > 0 {
		var people []Person
		for _, a := range n.Actors {
			people = append(people, Person{Name: a.Name, Role: a.Role, Kind: "Actor", ImageURL: a.Thumb, Order: a.Order})
		}
		for i, d := range n.Directors {
			people = append(people, Person{Name: d, Kind: "Director", Order: i})
		}
		for i, w := range n.Writers {
			people = append(people, Person{Name: w, Kind: "Writer", Order: i})
		}
		m.People = withTMDBIDs(people, m.People)
	}
	if len(n.Posters) > 0 {
		m.Images = replaceImage(m.Images, Image{"Primary", n.Posters[0]})
	}
	if len(n.Fanart) > 0 {
		m.Images = replaceImage(m.Images, Image{"Backdrop", n.Fanart[0]})
	}
	if m.Name == "" && n.Kind == "" && !had {
		return Meta{} // a link-only nfo that TMDB couldn't resolve: nothing to show
	}
	m.Source = "nfo"
	if had {
		m.Source = "nfo+tmdb"
	}
	return m
}

// withTMDBIDs keeps nfo people but borrows TMDB ids/images for the same
// names, so the person rows are shared with TMDB-matched titles.
func withTMDBIDs(local, fromTMDB []Person) []Person {
	byName := map[string]Person{}
	for _, p := range fromTMDB {
		byName[strings.ToLower(p.Name)] = p
	}
	for i, p := range local {
		if t, ok := byName[strings.ToLower(p.Name)]; ok {
			local[i].TMDBID = t.TMDBID
			if local[i].ImageURL == "" {
				local[i].ImageURL = t.ImageURL
			}
		}
	}
	return local
}

func replaceImage(images []Image, im Image) []Image {
	for i := range images {
		if images[i].Type == im.Type {
			images[i] = im
			return images
		}
	}
	return append(images, im)
}

// apply stores m on the item in one transaction.
func (r *Refresher) apply(ctx context.Context, row db.ItemsNeedingMetadataRow, m Meta) error {
	ids, err := json.Marshal(m.ProviderIDs)
	if err != nil {
		return err
	}
	if m.ProviderIDs == nil {
		ids = []byte("{}")
	}
	p := db.ApplyItemMetadataParams{
		ID: row.ID, OriginalTitle: opt(m.OriginalTitle), Overview: opt(m.Overview), Tagline: opt(m.Tagline),
		OfficialRating: opt(m.Rating), PremiereDate: parseDate(m.Premiere), EndDate: parseDate(m.End),
		OriginalLanguage: opt(m.OriginalLanguage),
		ProviderIds:      ids, MetadataSource: &m.Source,
	}
	if m.Name != "" {
		p.Name = &m.Name
		if row.Type == "Movie" || row.Type == "Series" { // seasons/episodes keep number-based sort names
			p.SortName = ptr(library.SortName(m.Name))
		}
	}
	if m.CommunityRating > 0 {
		p.CommunityRating = ptr(float32(m.CommunityRating))
	}
	if m.Year > 0 {
		p.ProductionYear = ptr(int32(m.Year))
	}
	if m.RuntimeMinutes > 0 {
		p.RuntimeTicks = ptr(int64(m.RuntimeMinutes) * 60 * 10_000_000)
	}
	return pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		q := r.q.WithTx(tx)
		if err := q.ApplyItemMetadata(ctx, p); err != nil {
			return err
		}
		if err := q.DeleteItemGenres(ctx, row.ID); err != nil {
			return err
		}
		for i, g := range m.Genres {
			gid, err := q.UpsertGenre(ctx, g)
			if err != nil {
				return err
			}
			if err := q.InsertItemGenre(ctx, db.InsertItemGenreParams{ItemID: row.ID, GenreID: gid, SortOrder: int32(i)}); err != nil {
				return err
			}
		}
		if err := q.DeleteItemStudios(ctx, row.ID); err != nil {
			return err
		}
		for i, s := range m.Studios {
			sid, err := q.UpsertStudio(ctx, s)
			if err != nil {
				return err
			}
			if err := q.InsertItemStudio(ctx, db.InsertItemStudioParams{ItemID: row.ID, StudioID: sid, SortOrder: int32(i)}); err != nil {
				return err
			}
		}
		if err := q.DeleteItemPeople(ctx, row.ID); err != nil {
			return err
		}
		for _, person := range m.People {
			pid, pids := personID(person)
			if err := q.UpsertPerson(ctx, db.UpsertPersonParams{ID: pid, Name: person.Name, ProviderIds: pids, ImageUrl: opt(person.ImageURL)}); err != nil {
				return err
			}
			if err := q.InsertItemPerson(ctx, db.InsertItemPersonParams{
				ItemID: row.ID, PersonID: pid, Kind: person.Kind, Role: opt(person.Role), SortOrder: int32(person.Order),
			}); err != nil {
				return err
			}
		}
		if err := q.DeleteRemoteImages(ctx, row.ID); err != nil {
			return err
		}
		idx := map[string]int32{}
		for _, im := range m.Images {
			if err := q.UpsertImage(ctx, db.UpsertImageParams{ItemID: row.ID, Type: im.Type, Idx: idx[im.Type], SourceUrl: &im.URL, Tag: images.Tag(im.URL)}); err != nil {
				return err
			}
			idx[im.Type]++
		}
		return nil
	})
}

var personNS = uuid.MustParse("3e0c1b7a-1f7e-4a2c-9b8e-5d1f0c6a7e21")

// personID is stable: from the TMDB id when known, otherwise from the name.
func personID(p Person) (uuid.UUID, json.RawMessage) {
	if p.TMDBID != 0 {
		id := strconv.Itoa(p.TMDBID)
		return uuid.NewSHA1(personNS, []byte("tmdb:"+id)), json.RawMessage(`{"Tmdb":"` + id + `"}`)
	}
	return uuid.NewSHA1(personNS, []byte("name:"+strings.ToLower(strings.TrimSpace(p.Name)))), json.RawMessage(`{}`)
}

// bestMatch picks the search result whose title matches exactly (ignoring
// case, accents and punctuation) and whose year is within one of ours. A
// single result is accepted when its year fits. Otherwise there is no match:
// a wrong match is worse than none.
func bestMatch(results []tmdb.Result, name string, year int) int {
	want := normTitle(name)
	yearOK := func(r tmdb.Result) bool {
		y := r.Year()
		return year == 0 || y == 0 || abs(y-year) <= 1
	}
	for _, r := range results {
		titles := []string{r.Title, r.Name, r.OriginalTitle, r.OriginalName}
		for _, t := range titles {
			if t != "" && normTitle(t) == want && yearOK(r) {
				return r.ID
			}
		}
	}
	if len(results) == 1 && year != 0 && results[0].Year() != 0 && yearOK(results[0]) {
		return results[0].ID
	}
	return 0
}

func normTitle(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r == '&':
			b.WriteString("and")
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(foldAccent(r))
		}
	}
	return b.String()
}

// foldAccent maps common Latin accented letters to ASCII ("é" → "e").
func foldAccent(r rune) rune {
	const from, to = "àáâãäåèéêëìíîïòóôõöùúûüýÿñç", "aaaaaaeeeeiiiiooooouuuuyync"
	if i := strings.IndexRune(from, r); i >= 0 {
		return []rune(to)[len([]rune(from[:i]))]
	}
	return r
}

func readNFO(paths ...string) *nfo.Info {
	for _, p := range paths {
		if p == "" || strings.HasPrefix(p, "stremio:") { // catalog items have no files
			continue
		}
		if _, err := os.Stat(p); err != nil {
			continue
		}
		if info, err := nfo.ReadFile(p); err == nil {
			return &info
		}
	}
	return nil
}

func ids(kv ...string) map[string]string {
	m := map[string]string{}
	for i := 0; i+1 < len(kv); i += 2 {
		if kv[i+1] != "" && kv[i+1] != "0" {
			m[kv[i]] = kv[i+1]
		}
	}
	return m
}

func names(ns []tmdb.Named) []string {
	var out []string
	for _, n := range ns {
		if n.Name != "" {
			out = append(out, n.Name)
		}
	}
	return out
}

func providerID(raw json.RawMessage, key string) string {
	var m map[string]string
	_ = json.Unmarshal(raw, &m)
	return m[key]
}

func parseDate(s string) *time.Time {
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return nil
	}
	return &t
}

func yearOf(date string) int {
	if len(date) < 4 {
		return 0
	}
	return atoi(date[:4])
}

func stemPath(p string) string { return strings.TrimSuffix(p, filepath.Ext(p)) }

func atoi(s string) int {
	n, _ := strconv.Atoi(strings.TrimSpace(s))
	return n
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

func opt(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func ptr[T any](v T) *T { return &v }

func deref[T any](p *T) T {
	var zero T
	if p == nil {
		return zero
	}
	return *p
}
