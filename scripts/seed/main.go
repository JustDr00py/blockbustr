// Command seed fills a database with a synthetic library for load tests
// (TASKS P4.5, DESIGN §9.6): movies and shows with seasons and episodes,
// genres, people, media sources with streams, image rows, and a user with a
// watch history (played movies, resume points, favourites, shows part
// watched for Next Up).
//
//	go run ./scripts/seed -database-url postgres://…/blockbustr_perf
//
// The defaults make about 51k items. It migrates the database, then refuses
// to touch one holding any library other than its own two ("Perf Movies",
// "Perf Shows"); -reset deletes those and the seed user first. The data is
// deterministic (-rand-seed). The server must list the two libraries in its
// config, or it disables them at start (scripts/loadtest/config.yaml does).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sysadmin/blockbustr/internal/auth"
	"github.com/sysadmin/blockbustr/internal/library"
	"github.com/sysadmin/blockbustr/internal/store/pg"
)

const (
	moviesLib = "Perf Movies"
	showsLib  = "Perf Shows"
	tick      = 10_000_000 // ticks per second
)

type options struct {
	url                          string
	movies, series, seasons, eps int
	people                       int
	user, password               string
	reset                        bool
	seed                         uint64
}

func main() {
	var o options
	flag.StringVar(&o.url, "database-url", os.Getenv("BLOCKBUSTR_DATABASE_URL"), "PostgreSQL URL (env BLOCKBUSTR_DATABASE_URL)")
	flag.IntVar(&o.movies, "movies", 30000, "movies")
	flag.IntVar(&o.series, "series", 300, "shows")
	flag.IntVar(&o.seasons, "seasons", 5, "seasons per show")
	flag.IntVar(&o.eps, "episodes", 13, "episodes per season")
	flag.IntVar(&o.people, "people", 4000, "distinct cast and crew")
	flag.StringVar(&o.user, "user", "perf", "user to create, with the watch history")
	flag.StringVar(&o.password, "password", "perf", "that user's password")
	flag.BoolVar(&o.reset, "reset", false, "delete a previous seed first")
	flag.Uint64Var(&o.seed, "rand-seed", 1, "random seed")
	flag.Parse()
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if o.url == "" {
		log.Error("-database-url is required")
		os.Exit(2)
	}
	if err := run(context.Background(), o, log); err != nil {
		log.Error("seed failed", "err", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, o options, log *slog.Logger) error {
	sqlDB, err := pg.Open(ctx, o.url, 10*time.Second, log)
	if err != nil {
		return err
	}
	err = pg.Migrate(ctx, sqlDB, "up", log)
	_ = sqlDB.Close()
	if err != nil {
		return err
	}
	pool, err := pg.NewPool(ctx, o.url)
	if err != nil {
		return err
	}
	defer pool.Close()

	var others int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM libraries WHERE name NOT IN ($1, $2)`, moviesLib, showsLib).Scan(&others); err != nil {
		return err
	}
	if others > 0 {
		return fmt.Errorf("the database has %d other libraries; seed only an empty database made for load tests", others)
	}
	if o.reset {
		// Nothing else uses this database, so genres and people go too.
		for _, q := range []string{`DELETE FROM libraries`, `DELETE FROM genres`, `DELETE FROM studios`, `DELETE FROM people`} {
			if _, err := pool.Exec(ctx, q); err != nil {
				return err
			}
		}
		if _, err := pool.Exec(ctx, `DELETE FROM users WHERE name = $1`, o.user); err != nil {
			return err
		}
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM libraries`).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return errors.New("already seeded (use -reset to seed again)")
	}

	start := time.Now()
	s := newSeeder(o)
	if err := s.build(); err != nil {
		return err
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.write(ctx, tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	if _, err := pool.Exec(ctx, `VACUUM ANALYZE`); err != nil {
		return err
	}
	log.Info("seeded", "items", len(s.items), "movies", o.movies, "series", o.series,
		"episodes", o.series*o.seasons*o.eps, "media_sources", len(s.sources), "user_data", len(s.userData),
		"user", o.user, "took", time.Since(start).Round(time.Millisecond))
	return nil
}

// seeder builds every row in memory, then copies them in.
type seeder struct {
	o     options
	rnd   *rand.Rand
	now   time.Time
	ns    uuid.UUID
	count int

	libraries, items, sources, streams, imageRows [][]any
	genres, itemGenres, people, itemPeople        [][]any
	userData                                      [][]any
	userID                                        uuid.UUID
	userHash                                      string
	genreIDs, personIDs                           []uuid.UUID
}

func newSeeder(o options) *seeder {
	return &seeder{
		o:   o,
		rnd: rand.New(rand.NewPCG(o.seed, o.seed)), //nolint:gosec // synthetic data, not security
		now: time.Now().UTC().Truncate(time.Second),
		ns:  uuid.MustParse("6d1f4b8e-2c55-4f0e-9a3b-5e0c1d7a9b42"),
	}
}

// id is deterministic, so a reseed gives the same ids.
func (s *seeder) id() uuid.UUID {
	s.count++
	return uuid.NewSHA1(s.ns, fmt.Appendf(nil, "%d:%d", s.o.seed, s.count))
}

var (
	words = strings.Fields(`amber atlas beacon broken canyon cipher cobalt crimson dawn delta drift echo
		ember empire falcon frontier galaxy ghost glass harbor haven hollow horizon hunter iron island
		jade kingdom lantern legacy liberty lunar meridian midnight mirror monarch nebula night north
		oasis obsidian orbit paper phantom pioneer prism quarry quiet raven rebel river saga shadow
		signal silver solstice spark static stone storm summit tempest thunder tide timber titan
		twilight umbra valley velvet vertex violet voyage wander whisper wild winter wolf zenith`)
	genreNames = []string{"Action", "Adventure", "Animation", "Comedy", "Crime", "Documentary", "Drama",
		"Family", "Fantasy", "History", "Horror", "Music", "Mystery", "Romance", "Science Fiction",
		"Thriller", "War", "Western", "Reality", "Sport"}
	ratings = []string{"G", "PG", "PG-13", "R", "TV-14", "TV-MA", "NR"}
)

func (s *seeder) title() string {
	n := 1 + s.rnd.IntN(3)
	parts := make([]string, 0, n+1)
	if s.rnd.IntN(4) == 0 {
		parts = append(parts, "The")
	}
	for range n {
		w := words[s.rnd.IntN(len(words))]
		parts = append(parts, strings.ToUpper(w[:1])+w[1:])
	}
	return strings.Join(parts, " ")
}

func (s *seeder) build() error {
	s.genreIDs = make([]uuid.UUID, len(genreNames))
	for i, g := range genreNames {
		s.genreIDs[i] = s.id()
		s.genres = append(s.genres, []any{s.genreIDs[i], g})
	}
	s.personIDs = make([]uuid.UUID, s.o.people)
	for i := range s.personIDs {
		s.personIDs[i] = s.id()
		s.people = append(s.people, []any{s.personIDs[i], s.title() + " " + s.title(), jsonb(map[string]string{"Tmdb": fmt.Sprint(100000 + i)})})
	}
	s.userID = s.id()
	h, err := auth.HashPassword(s.o.password)
	if err != nil {
		return err
	}
	s.userHash = h

	movies := s.library(moviesLib, "movies")
	for i := range s.o.movies {
		s.movie(movies, i)
	}
	shows := s.library(showsLib, "tvshows")
	for i := range s.o.series {
		s.show(shows, i)
	}
	return nil
}

type lib struct{ id, folder uuid.UUID }

func (s *seeder) library(name, kind string) lib {
	l := lib{id: s.id(), folder: s.id()}
	path := "/perf/" + strings.ToLower(strings.TrimPrefix(name, "Perf "))
	s.libraries = append(s.libraries, []any{l.id, name, kind, []string{path}})
	s.items = append(s.items, s.item(itemRow{id: l.folder, lib: l.id, typ: "CollectionFolder", name: name, created: s.now}))
	return l
}

type itemRow struct {
	id, lib            uuid.UUID
	parent, top        *uuid.UUID
	typ, name, path    string
	index, parentIndex *int
	year               int
	premiere           *time.Time
	runtime            int64
	created            time.Time
	rating             string
	community          float32
	providers          map[string]string
}

// item is one items row, in write's column order. NULLs are nil.
func (s *seeder) item(r itemRow) []any {
	var path, etag, overview, rating, metaSource, year, runtime, community any
	source := "virtual"
	if r.path != "" {
		path, etag, source = r.path, fmt.Sprintf("%x", r.id[:6]), "file"
	}
	if r.typ != "CollectionFolder" {
		overview = "A synthetic " + strings.ToLower(r.typ) + " for load tests: " + r.name + "."
		metaSource = "tmdb"
	}
	if r.rating != "" {
		rating = r.rating
	}
	if r.year != 0 {
		year = r.year
	}
	if r.runtime != 0 {
		runtime = r.runtime
	}
	if r.community != 0 {
		community = r.community
	}
	providers := r.providers
	if providers == nil {
		providers = map[string]string{}
	}
	return []any{r.id, r.lib, r.parent, r.top, r.typ, r.name, library.SortName(r.name), r.index, r.parentIndex,
		year, r.premiere, overview, rating, community, runtime, jsonb(providers), source, path, etag,
		r.created, r.created, r.created, metaSource, r.created}
}

func (s *seeder) movie(l lib, i int) {
	id := s.id()
	name := s.title()
	year := 1960 + s.rnd.IntN(66)
	premiere := time.Date(year, time.Month(1+s.rnd.IntN(12)), 1+s.rnd.IntN(28), 0, 0, 0, 0, time.UTC)
	created := s.now.Add(-time.Duration(s.rnd.IntN(3*365*24)) * time.Hour)
	runtime := int64(80+s.rnd.IntN(80)) * 60 * tick
	s.items = append(s.items, s.item(itemRow{
		id: id, lib: l.id, parent: &l.folder, top: &l.folder, typ: "Movie", name: name,
		path: fmt.Sprintf("/perf/movies/%s (%d) [%d]/movie.mkv", name, year, i), year: year, premiere: &premiere,
		runtime: runtime, created: created, rating: ratings[s.rnd.IntN(len(ratings))],
		community: float32(40+s.rnd.IntN(55)) / 10,
		providers: map[string]string{"Imdb": fmt.Sprintf("tt%07d", 9000000+i), "Tmdb": fmt.Sprint(900000 + i)},
	}))
	s.source(id, runtime, true)
	s.images(id, true)
	s.tags(id)
	// Watch history: 1 in 15 played (some favourites), 1 in 600 part
	// watched, 1 in 100 a favourite.
	switch {
	case i%15 == 0:
		s.watched(id, true, 0, created.Add(time.Duration(i)*time.Minute), i%100 == 0)
	case i%600 == 7:
		s.watched(id, false, runtime/3, s.now.Add(-time.Duration(i)*time.Minute), false)
	case i%100 == 1:
		s.watched(id, false, 0, time.Time{}, true)
	}
}

func (s *seeder) show(l lib, i int) {
	id := s.id()
	name := s.title()
	year := 1990 + s.rnd.IntN(36)
	premiere := time.Date(year, time.Month(1+s.rnd.IntN(12)), 1+s.rnd.IntN(28), 0, 0, 0, 0, time.UTC)
	created := s.now.Add(-time.Duration(s.rnd.IntN(3*365*24)) * time.Hour)
	dir := fmt.Sprintf("/perf/shows/%s (%d) [%d]", name, year, i)
	s.items = append(s.items, s.item(itemRow{
		id: id, lib: l.id, parent: &l.folder, top: &l.folder, typ: "Series", name: name, path: dir,
		year: year, premiere: &premiere, created: created, rating: ratings[s.rnd.IntN(len(ratings))],
		community: float32(50+s.rnd.IntN(45)) / 10,
		providers: map[string]string{"Imdb": fmt.Sprintf("tt%07d", 8000000+i), "Tvdb": fmt.Sprint(700000 + i)},
	}))
	s.images(id, true)
	s.tags(id)
	if i%30 == 2 {
		s.watched(id, false, 0, time.Time{}, true)
	}
	// Half the shows are watched up to an episode that differs per show;
	// every sixth stops part way into it.
	watchedUpTo := -1
	if i%2 == 0 {
		watchedUpTo = (i * 7) % (s.o.seasons * s.o.eps)
	}
	n := 0
	for season := 1; season <= s.o.seasons; season++ {
		sid := s.id()
		sn := season
		s.items = append(s.items, s.item(itemRow{
			id: sid, lib: l.id, parent: &id, top: &l.folder, typ: "Season", name: fmt.Sprintf("Season %d", season),
			index: &sn, created: created,
		}))
		s.images(sid, false)
		for ep := 1; ep <= s.o.eps; ep++ {
			eid := s.id()
			en := ep
			runtime := int64(22+s.rnd.IntN(40)) * 60 * tick
			aired := premiere.AddDate(season-1, 0, 7*(ep-1))
			s.items = append(s.items, s.item(itemRow{
				id: eid, lib: l.id, parent: &sid, top: &l.folder, typ: "Episode", name: s.title(),
				path:  fmt.Sprintf("%s/Season %02d/S%02dE%02d.mkv", dir, season, season, ep),
				index: &en, parentIndex: &sn, year: aired.Year(), premiere: &aired, runtime: runtime,
				created: created.Add(time.Duration(n) * time.Hour),
			}))
			s.source(eid, runtime, false)
			switch {
			case n < watchedUpTo:
				s.watched(eid, true, 0, s.now.Add(-time.Duration(i*100+watchedUpTo-n)*time.Hour), false)
			case n == watchedUpTo && i%6 == 0:
				s.watched(eid, false, runtime/2, s.now.Add(-time.Duration(i)*time.Hour), false)
			}
			n++
		}
	}
}

// source adds a probed media source with video, audio and subtitle streams.
func (s *seeder) source(item uuid.UUID, runtime int64, movie bool) {
	src := s.id()
	w, h, codec, rng := 1920, 1080, "h264", "SDR"
	switch r := s.rnd.IntN(10); {
	case r < 2:
		w, h, codec, rng = 3840, 2160, "hevc", "HDR10"
	case r < 4:
		w, h = 1280, 720
	}
	bitrate := 2_000_000 + s.rnd.IntN(18_000_000)
	name := "episode"
	if movie {
		name = "movie"
	}
	s.sources = append(s.sources, []any{src, item, name, "mkv", int64(bitrate) / 8 * (runtime / tick), bitrate,
		"/perf/" + item.String() + ".mkv", "File", false, s.now, runtime, fmt.Sprintf("%x", item[:6])})
	s.streams = append(s.streams,
		[]any{src, 0, "Video", codec, nil, w, h, bitrate - 640_000, nil, nil, rng, rng, true, "Main", float32(23.976), float32(23.976), nil, "16:9", "yuv420p", 8},
		[]any{src, 1, "Audio", "eac3", "eng", nil, nil, 640_000, 6, "5.1", nil, nil, true, nil, nil, nil, 48000, nil, nil, nil},
		[]any{src, 2, "Subtitle", "subrip", "eng", nil, nil, nil, nil, nil, nil, nil, false, nil, nil, nil, nil, nil, nil, nil},
	)
	if movie && s.rnd.IntN(3) == 0 {
		s.streams = append(s.streams, []any{src, 3, "Audio", "aac", "spa", nil, nil, 192_000, 2, "stereo", nil, nil, false, nil, nil, nil, 48000, nil, nil, nil})
	}
}

// images adds remote artwork rows (never fetched by the load test).
func (s *seeder) images(item uuid.UUID, backdrop bool) {
	tag := fmt.Sprintf("%x", item[:8])
	s.imageRows = append(s.imageRows, []any{item, "Primary", 0, "https://image.invalid/" + tag + ".jpg", tag, 600, 900})
	if backdrop {
		s.imageRows = append(s.imageRows, []any{item, "Backdrop", 0, "https://image.invalid/" + tag + "b.jpg", tag + "b", 1920, 1080})
	}
}

// tags gives an item one to three genres and up to five people.
func (s *seeder) tags(item uuid.UUID) {
	for i, gi := range s.rnd.Perm(len(s.genreIDs))[:1+s.rnd.IntN(3)] {
		s.itemGenres = append(s.itemGenres, []any{item, s.genreIDs[gi], i})
	}
	seen := map[int]bool{}
	for i := range 5 {
		p := s.rnd.IntN(len(s.personIDs))
		if seen[p] {
			continue
		}
		seen[p] = true
		kind, role := "Actor", any(s.title())
		if i == 0 {
			kind, role = "Director", nil
		}
		s.itemPeople = append(s.itemPeople, []any{item, s.personIDs[p], kind, role, i})
	}
}

func (s *seeder) watched(item uuid.UUID, played bool, pos int64, at time.Time, fav bool) {
	count := 0
	if played {
		count = 1
	}
	var last any
	if !at.IsZero() {
		last = at
	}
	s.userData = append(s.userData, []any{s.userID, item, played, count, pos, fav, last})
}

func (s *seeder) write(ctx context.Context, tx pgx.Tx) error {
	if _, err := tx.Exec(ctx, `INSERT INTO users (id, name, password_hash, is_admin) VALUES ($1, $2, $3, true)`,
		s.userID, s.o.user, s.userHash); err != nil {
		return err
	}
	tables := []struct {
		name string
		cols []string
		rows [][]any
	}{
		{"libraries", []string{"id", "name", "kind", "paths"}, s.libraries},
		{"items", []string{"id", "library_id", "parent_id", "top_parent_id", "type", "name", "sort_name",
			"index_number", "parent_index_number", "production_year", "premiere_date", "overview",
			"official_rating", "community_rating", "runtime_ticks", "provider_ids", "source_kind", "path", "etag",
			"date_created", "date_modified", "date_last_refreshed", "metadata_source", "metadata_refreshed_at"}, s.items},
		{"media_sources", []string{"id", "item_id", "name", "container", "size", "bitrate", "path_or_url",
			"protocol", "is_remote", "probed_at", "runtime_ticks", "etag"}, s.sources},
		{"media_streams", []string{"media_source_id", "idx", "type", "codec", "language", "width", "height",
			"bitrate", "channels", "channel_layout", "video_range", "video_range_type", "is_default", "profile",
			"average_frame_rate", "real_frame_rate", "sample_rate", "aspect_ratio", "pixel_format", "bit_depth"}, s.streams},
		{"images", []string{"item_id", "type", "idx", "source_url", "tag", "width", "height"}, s.imageRows},
		{"genres", []string{"id", "name"}, s.genres},
		{"item_genres", []string{"item_id", "genre_id", "sort_order"}, s.itemGenres},
		{"people", []string{"id", "name", "provider_ids"}, s.people},
		{"item_people", []string{"item_id", "person_id", "kind", "role", "sort_order"}, s.itemPeople},
		{"user_data", []string{"user_id", "item_id", "played", "play_count", "playback_position_ticks",
			"is_favorite", "last_played_at"}, s.userData},
	}
	for _, t := range tables {
		if _, err := tx.CopyFrom(ctx, pgx.Identifier{t.name}, t.cols, pgx.CopyFromRows(t.rows)); err != nil {
			return fmt.Errorf("copy %s: %w", t.name, err)
		}
	}
	return nil
}

func jsonb(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
