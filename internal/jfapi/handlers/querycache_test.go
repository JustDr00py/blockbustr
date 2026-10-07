package handlers

import (
	"testing"

	"github.com/sysadmin/blockbustr/internal/events"
	"github.com/sysadmin/blockbustr/internal/testutil"
)

// A total is counted once per generation: an event retires it (TASKS P4.5).
func TestCachedCount(t *testing.T) {
	c := testutil.Cache(t)
	a := &api{Deps: Deps{Cache: c, Log: testutil.Discard()}}
	bus := &events.Bus{Cache: c, Log: testutil.Discard()}
	calls := 0
	count := func() (int, error) { calls++; return 40 + calls, nil }
	get := func(query string) int {
		t.Helper()
		n, err := a.cachedCount(t.Context(), query, count)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}

	if n := get("q1"); n != 41 || calls != 1 {
		t.Fatalf("first count = %d after %d calls, want 41 after 1", n, calls)
	}
	if n := get("q1"); n != 41 || calls != 1 {
		t.Fatalf("cached count = %d after %d calls, want 41 after 1", n, calls)
	}
	if n := get("q2"); n != 42 || calls != 2 {
		t.Fatalf("another query = %d after %d calls, want 42 after 2", n, calls)
	}
	bus.Publish(t.Context(), events.Event{Kind: events.UserDataChanged})
	if n := get("q1"); n != 43 || calls != 3 {
		t.Fatalf("after an event = %d after %d calls, want a recount (43 after 3)", n, calls)
	}
	bus.Publish(t.Context(), events.Event{Kind: events.LibraryChanged})
	if n := get("q1"); n != 44 {
		t.Fatalf("after a library change = %d, want a recount (44)", n)
	}
}
