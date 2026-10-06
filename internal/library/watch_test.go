package library

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sysadmin/blockbustr/internal/store/pg/db"
	"github.com/sysadmin/blockbustr/internal/testutil"
)

func TestIgnoredName(t *testing.T) {
	for name, want := range map[string]bool{
		"/m/Luca (2021).mkv": false, "/m/Show/Season 1": false, "/m/movie.strm": false, "/m/movie.nfo": false,
		"/m/.DS_Store": true, "/m/.hidden/x.mkv": false, "/m/.hidden": true,
		"/m/Luca.mkv.part": true, "/m/a.TMP": true, "/m/a.mkv.!qB": true, "/m/a.crdownload": true,
	} {
		if got := ignoredName(name); got != want {
			t.Errorf("%s: %v", name, got)
		}
	}
}

// watchFor runs the watcher over libs and returns the channel of libraries
// it asks to rescan.
func watchFor(t *testing.T, libs []db.Library, delay time.Duration) <-chan uuid.UUID {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	changed := make(chan uuid.UUID, 16)
	done := make(chan error, 1)
	go func() { done <- watch(ctx, libs, delay, testutil.Discard(), func(id uuid.UUID) { changed <- id }) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	time.Sleep(100 * time.Millisecond) // let the initial walk add its watches
	return changed
}

// expect waits for exactly the given rescans (in any order) and then for
// quiet.
func expect(t *testing.T, changed <-chan uuid.UUID, want ...uuid.UUID) {
	t.Helper()
	got := map[uuid.UUID]int{}
	for range want {
		select {
		case id := <-changed:
			got[id]++
		case <-time.After(3 * time.Second):
			t.Fatalf("waited for %v, got %v", want, got)
		}
	}
	select {
	case id := <-changed:
		t.Fatalf("unexpected extra rescan of %v (after %v)", id, got)
	case <-time.After(400 * time.Millisecond):
	}
	for _, id := range want {
		if got[id] != 1 {
			t.Errorf("rescans: %v, want each of %v once", got, want)
		}
	}
}

func write(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestWatchRescansTheChangedLibrary(t *testing.T) {
	movies, shows := t.TempDir(), t.TempDir()
	if err := os.MkdirAll(filepath.Join(shows, "Show", "Season 1"), 0o755); err != nil {
		t.Fatal(err)
	}
	m, s := db.Library{ID: uuid.New(), Paths: []string{movies}}, db.Library{ID: uuid.New(), Paths: []string{shows}}
	changed := watchFor(t, []db.Library{m, s}, 150*time.Millisecond)

	// A burst of writes is one rescan, of that library only, even in a
	// folder that existed before (watched by the initial walk).
	for i := range 5 {
		write(t, filepath.Join(shows, "Show", "Season 1", "e"+string(rune('1'+i))+".mkv"))
		time.Sleep(30 * time.Millisecond)
	}
	expect(t, changed, s.ID)

	// A new folder is watched too: creating it is one change, a file put in
	// it later another.
	if err := os.Mkdir(filepath.Join(movies, "Luca (2021)"), 0o755); err != nil {
		t.Fatal(err)
	}
	expect(t, changed, m.ID)
	write(t, filepath.Join(movies, "Luca (2021)", "Luca.mkv"))
	expect(t, changed, m.ID)

	// Deletes and renames count; hidden files and partial downloads don't.
	write(t, filepath.Join(movies, ".DS_Store"))
	write(t, filepath.Join(movies, "Big.mkv.part"))
	expect(t, changed)
	if err := os.Rename(filepath.Join(movies, "Big.mkv.part"), filepath.Join(movies, "Big.mkv")); err != nil {
		t.Fatal(err)
	}
	expect(t, changed, m.ID)
	if err := os.RemoveAll(filepath.Join(shows, "Show")); err != nil {
		t.Fatal(err)
	}
	expect(t, changed, s.ID)
}

// A missing library folder (an unmounted share) doesn't stop the others.
func TestWatchMissingRoot(t *testing.T) {
	dir := t.TempDir()
	ok, gone := db.Library{ID: uuid.New(), Paths: []string{dir}}, db.Library{ID: uuid.New(), Paths: []string{filepath.Join(dir, "..", "nope-"+uuid.NewString())}}
	changed := watchFor(t, []db.Library{gone, ok}, 50*time.Millisecond)
	write(t, filepath.Join(dir, "a.mkv"))
	expect(t, changed, ok.ID)
}

func TestTakePending(t *testing.T) {
	a, b, c := db.Library{ID: uuid.New()}, db.Library{ID: uuid.New()}, db.Library{ID: uuid.New()}
	libs := []db.Library{a, b, c}
	s := &Scanner{trigger: make(chan struct{}, 1)}
	if got := s.takePending(libs); len(got) != 0 {
		t.Errorf("nothing triggered: %v", got)
	}
	s.TriggerLibrary(c.ID)
	s.TriggerLibrary(a.ID)
	s.TriggerLibrary(a.ID)
	if got := s.takePending(libs); len(got) != 2 || got[0].ID != a.ID || got[1].ID != c.ID {
		t.Errorf("per library, in library order, merged: %v", got)
	}
	s.TriggerLibrary(b.ID)
	s.Trigger()
	if got := s.takePending(libs); len(got) != 3 {
		t.Errorf("Trigger scans everything: %v", got)
	}
	if got := s.takePending(libs); len(got) != 0 {
		t.Errorf("requests are cleared: %v", got)
	}
	if len(s.trigger) != 1 {
		t.Errorf("signals merge into one pending wake-up: %d", len(s.trigger))
	}
}
