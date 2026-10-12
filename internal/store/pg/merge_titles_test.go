package pg

import (
	"context"
	"database/sql"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Migration 00023 runs in seconds at production's size. v0.2.13's version
// updated seasons and episodes with an OR'd, correlated subquery: a
// subplan per (item, title) pair, 69 s on a copy of the dev database
// (108k items, 1.7k titles) and over 10 minutes on production (99k), while
// the server, migrating before it listens, refused every login. Rewritten
// as joins it took 5.8 s for the whole migration on the same copy.
//
// Here: 1,200 series and 1,200 movies, each in two catalogs (both series
// copies with 2 seasons of 12 episodes: 67k items), without statistics (as
// after a restore). The fixed migration took 2.1 s for this; the old one
// was still running at the 15 s bound.
func TestMergeDuplicateTitlesAtScale(t *testing.T) {
	db := newTestDB(t)
	p, err := newProvider(db)
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	if _, err := p.UpTo(ctx, 22); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO libraries (name, kind) VALUES ('A', 'stremio'), ('B', 'stremio');
		INSERT INTO items (library_id, type, name, sort_name, source_kind, path, stremio_ref, provider_ids)
		SELECT l.id, k.type, k.type || n, k.type || n, 'stremio', 'stremio:' || lower(k.type) || ':tt' || n,
		       jsonb_build_object('id', 'tt' || n), jsonb_build_object('Imdb', 'tt' || n)
		FROM libraries l, generate_series(1, 1200) n,
		     (VALUES ('Series'), ('Movie')) k(type);
		INSERT INTO items (library_id, parent_id, type, name, sort_name, index_number)
		SELECT s.library_id, s.id, 'Season', 'Season ' || n, n::text, n
		FROM items s, generate_series(1, 2) n WHERE s.type = 'Series';
		INSERT INTO items (library_id, parent_id, type, name, sort_name, source_kind, path, stremio_ref, index_number, parent_index_number)
		SELECT s.library_id, s.id, 'Episode', 'E' || n, n::text, 'stremio', 'stremio:series:' || s.id || ':' || n,
		       '{"id":"x"}', n, s.index_number
		FROM items s, generate_series(1, 12) n WHERE s.type = 'Season';`); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	uctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if _, err := p.UpTo(uctx, 23); err != nil {
		t.Fatalf("migration 23 after %s: %v", time.Since(start).Round(time.Millisecond), err)
	}
	t.Logf("migration 23: %s", time.Since(start).Round(time.Millisecond))
	var titles, unlinked int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM items WHERE canonical_key IS NOT NULL`).Scan(&titles); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM items WHERE type = 'Episode' AND cardinality(library_ids) <> 2`).Scan(&unlinked); err != nil {
		t.Fatal(err)
	}
	if titles != 2400 || unlinked != 0 {
		t.Errorf("%d titles (want 2400), %d episodes not in both libraries", titles, unlinked)
	}
}

// Migration 00023 merges the copies of a title that catalog libraries each
// had into one item: in every library, with the users' data merged (played
// and favourite if any copy was; the resume point of the copy played
// last), episodes' too, and plays moved to it.
func TestMergeDuplicateTitles(t *testing.T) {
	db := newTestDB(t)
	p, err := newProvider(db)
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	if _, err := p.UpTo(ctx, 22); err != nil {
		t.Fatal(err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	libs := []uuid.UUID{uuid.New(), uuid.New(), uuid.New()} // Trending, Apple TV+, Popular
	for i, l := range libs {
		exec(`INSERT INTO libraries (id, name, kind) VALUES ($1, $2, 'stremio')`, l, []string{"Trending", "Apple TV+", "Popular"}[i])
	}
	day := func(d int) time.Time { return time.Date(2026, 10, d, 20, 0, 0, 0, time.UTC) }
	item := func(lib uuid.UUID, typ, path, ids string, parent *uuid.UUID, idx *int, metadata *string, created time.Time) uuid.UUID {
		t.Helper()
		id := uuid.New()
		exec(`INSERT INTO items (id, library_id, parent_id, type, name, sort_name, source_kind, path, stremio_ref,
		                         provider_ids, index_number, metadata_source, date_created, date_last_refreshed)
		      VALUES ($1, $2, $3, $4, $5, $5, 'stremio', $6, '{"id":"x"}', $7, $8, $9, $10, $10)`,
			id, lib, parent, typ, path, path, ids, idx, metadata, created)
		return id
	}
	tmdb := "tmdb"
	// Matchbox: the Trending copy has metadata, so it's kept; the Apple TV+
	// catalog listed it by its TMDB id.
	kept := item(libs[0], "Movie", "stremio:movie:tt28498219", `{"Imdb":"tt28498219","Tmdb":"1148365"}`, nil, nil, &tmdb, day(1))
	byTMDB := item(libs[1], "Movie", "stremio:movie:tmdb:1148365", `{"Imdb":"tt28498219","Tmdb":"1148365"}`, nil, nil, nil, day(2))
	third := item(libs[2], "Movie", "stremio:movie:tt28498219", `{"Imdb":"tt28498219"}`, nil, nil, nil, day(3))
	other := item(libs[2], "Movie", "stremio:movie:tt0133093", `{"Imdb":"tt0133093"}`, nil, nil, nil, day(3))

	one, two := 1, 2
	series := func(lib uuid.UUID, created time.Time) (show, ep1, ep2 uuid.UUID) {
		show = item(lib, "Series", "stremio:series:tt0944947", `{"Imdb":"tt0944947"}`, nil, nil, nil, created)
		season := item(lib, "Season", "", `{}`, &show, &one, nil, created)
		exec(`UPDATE items SET path = NULL WHERE id = $1`, season)
		ep1 = item(lib, "Episode", "stremio:series:tt0944947:1:1"+lib.String(), `{}`, &season, &one, nil, created)
		ep2 = item(lib, "Episode", "stremio:series:tt0944947:1:2"+lib.String(), `{}`, &season, &two, nil, created)
		return show, ep1, ep2
	}
	show, ep1, _ := series(libs[0], day(1))
	showCopy, ep1Copy, ep2Copy := series(libs[1], day(2))

	pd, other2 := uuid.New(), uuid.New()
	exec(`INSERT INTO users (id, name) VALUES ($1, 'pd'), ($2, 'guest')`, pd, other2)
	ud := func(user, item uuid.UUID, played bool, count int, pos int64, fav bool, last *time.Time, audio *int) {
		t.Helper()
		exec(`INSERT INTO user_data (user_id, item_id, played, play_count, playback_position_ticks, is_favorite, last_played_at, audio_stream_idx)
		      VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`, user, item, played, count, pos, fav, last, audio)
	}
	d1, d5, d9 := day(1), day(5), day(9)
	// pd watched Matchbox to the end on one copy, then started it again on
	// another (the latest), and favourited a third.
	ud(pd, third, true, 1, 0, false, &d1, nil)
	ud(pd, byTMDB, false, 0, 5_000, false, &d9, &two)
	ud(pd, kept, false, 0, 9_000, true, &d5, &one)
	ud(other2, byTMDB, false, 0, 7_000, false, &d5, nil)
	ud(pd, ep1Copy, false, 0, 300, false, &d9, nil)
	ud(pd, ep2Copy, true, 1, 0, false, &d5, nil)
	exec(`INSERT INTO playback_log (user_name, item_id, item_name) VALUES ('pd', $1, 'Matchbox'), ('pd', $2, 'Ep 1')`, byTMDB, ep1Copy)

	if _, err := p.UpTo(ctx, 23); err != nil {
		t.Fatal(err)
	}

	count := func(q string, args ...any) int {
		t.Helper()
		var n int
		if err := db.QueryRowContext(ctx, q, args...).Scan(&n); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return n
	}
	if n := count(`SELECT count(*) FROM items WHERE canonical_key = 'movie:tt28498219'`); n != 1 {
		t.Errorf("Matchbox items: %d", n)
	}
	if n := count(`SELECT count(*) FROM items WHERE id = ANY($1)`, []uuid.UUID{byTMDB, third, showCopy, ep1Copy, ep2Copy}); n != 0 {
		t.Errorf("%d merged copies left", n)
	}
	if n := count(`SELECT count(*) FROM items WHERE id = $1 AND canonical_key = 'movie:tt0133093'`, other); n != 1 {
		t.Error("a title with one copy changed")
	}
	libraryIDs := func(id uuid.UUID) []uuid.UUID {
		t.Helper()
		var ids []uuid.UUID
		rows, err := db.QueryContext(ctx, `SELECT unnest(library_ids) FROM items WHERE id = $1`, id)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var l uuid.UUID
			_ = rows.Scan(&l)
			ids = append(ids, l)
		}
		slices.SortFunc(ids, func(a, b uuid.UUID) int { return slices.Compare(a[:], b[:]) })
		return ids
	}
	all := slices.Clone(libs)
	slices.SortFunc(all, func(a, b uuid.UUID) int { return slices.Compare(a[:], b[:]) })
	if got := libraryIDs(kept); !slices.Equal(got, all) {
		t.Errorf("Matchbox in %v, want all three", got)
	}
	if n := count(`SELECT count(*) FROM item_libraries WHERE item_id = $1`, kept); n != 3 {
		t.Errorf("Matchbox memberships: %d", n)
	}
	if got := libraryIDs(ep1); len(got) != 2 {
		t.Errorf("episode in %v, want its series' two libraries", got)
	}

	type row struct {
		played, fav bool
		count       int
		pos         int64
		last        sql.NullTime
		audio       sql.NullInt64
	}
	get := func(user, item uuid.UUID) row {
		t.Helper()
		var r row
		if err := db.QueryRowContext(ctx, `SELECT played, is_favorite, play_count, playback_position_ticks, last_played_at, audio_stream_idx
		                                   FROM user_data WHERE user_id = $1 AND item_id = $2`, user, item).
			Scan(&r.played, &r.fav, &r.count, &r.pos, &r.last, &r.audio); err != nil {
			t.Fatalf("user data of %s: %v", item, err)
		}
		return r
	}
	if r := get(pd, kept); !r.played || !r.fav || r.count != 1 || r.pos != 5_000 || !r.last.Time.Equal(d9) || r.audio.Int64 != 2 {
		t.Errorf("pd's Matchbox: %+v; want played, favourite, the latest play's position (5000) and track (2)", r)
	}
	if r := get(other2, kept); r.pos != 7_000 {
		t.Errorf("guest's Matchbox: %+v", r)
	}
	if r := get(pd, ep1); r.pos != 300 {
		t.Errorf("pd's episode 1 (on the merged copy): %+v", r)
	}
	if n := count(`SELECT count(*) FROM user_data WHERE user_id = $1 AND played AND item_id IN
	                 (SELECT e.id FROM items e JOIN items s ON s.id = e.parent_id WHERE s.parent_id = $2 AND e.index_number = 2)`, pd, show); n != 1 {
		t.Error("pd's played episode 2 (on the merged copy) wasn't kept")
	}
	if n := count(`SELECT count(*) FROM playback_log WHERE item_id = ANY($1)`, []uuid.UUID{kept, ep1}); n != 2 {
		t.Errorf("plays moved: %d of 2", n)
	}
}
