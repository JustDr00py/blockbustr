package metrics

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"
)

type fakeStore struct {
	hours map[time.Time]int64
	err   error
}

func (f *fakeStore) AddProxiedBytes(_ context.Context, hour time.Time, n int64) error {
	if f.err != nil {
		return f.err
	}
	f.hours[hour] += n
	return nil
}

func TestMonthlyBytes(t *testing.T) {
	st := &fakeStore{hours: map[time.Time]int64{}}
	b := NewMonthlyBytes(st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	hour := time.Now().UTC().Truncate(time.Hour)

	ProxiedBytes.Add(1000)
	if got := b.Unflushed(); got != 1000 {
		t.Fatalf("Unflushed = %d, want 1000", got)
	}
	st.err = errors.New("db down")
	b.Flush(t.Context())
	if got := b.Unflushed(); got != 1000 {
		t.Fatalf("a failed flush lost bytes: Unflushed = %d", got)
	}
	st.err = nil
	ProxiedBytes.Add(500)
	b.Flush(t.Context())
	b.Flush(t.Context()) // nothing new: no double count
	if got := st.hours[hour]; got != 1500 {
		t.Errorf("stored %d, want 1500", got)
	}
	if got := b.Unflushed(); got != 0 {
		t.Errorf("Unflushed = %d after flush", got)
	}
}
