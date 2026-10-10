package metadata

import (
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/sysadmin/blockbustr/internal/testutil"
)

// An episode TMDB numbers differently (IMDb's season 2, TMDB's S1E29…)
// gets its metadata from the TMDB episode that aired the same day, or a
// day off; an air date no TMDB episode has leaves it unmatched.
func TestRefreshEpisodeByAirDate(t *testing.T) {
	e := newEnv(t)
	for _, name := range []string{"MF Ghost S02E01.mkv", "MF Ghost S02E02.mkv"} {
		p := filepath.Join(e.root, "Series/MF Ghost/Season 02", name)
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	e.scan()
	// What Cinemeta would have stored: a day before TMDB's S1E2, and a day
	// nothing aired.
	for path, day := range map[string]string{"%S02E01.mkv": "2023-10-08", "%S02E02.mkv": "2024-05-01"} {
		if _, err := e.pool.Exec(t.Context(), `UPDATE items SET premiere_date = $1 WHERE path LIKE $2`, day, path); err != nil {
			t.Fatal(err)
		}
	}
	var calls atomic.Int32
	e.refresh(NewRefresher(e.pool, fakeTMDB(t, &calls), testutil.Discard()))
	eps := e.byType(e.tv, "Episode")
	if _, ok := eps["The Shocking New MFG Generation"]; !ok {
		t.Errorf("S02E01 not matched to TMDB's S1E2 by air date: %v", keys(eps))
	}
	for name, it := range eps {
		if it.IndexNumber != nil && *it.IndexNumber == 2 && it.ParentIndexNumber != nil && *it.ParentIndexNumber == 2 && deref(it.MetadataSource) != "none" {
			t.Errorf("S02E02 (no TMDB episode aired then) matched as %q", name)
		}
	}
}
