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
	months map[time.Time]int64
	err    error
}

func (f *fakeStore) AddProxiedBytes(_ context.Context, month time.Time, n int64) error {
	if f.err != nil {
		return f.err
	}
	f.months[month] += n
	return nil
}

func TestMonthlyBytes(t *testing.T) {
	st := &fakeStore{months: map[time.Time]int64{}}
	b := NewMonthlyBytes(st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	month := MonthStart(time.Now())

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
	if got := st.months[month]; got != 1500 {
		t.Errorf("stored %d, want 1500", got)
	}
	if got := b.Unflushed(); got != 0 {
		t.Errorf("Unflushed = %d after flush", got)
	}
}

func TestMonthStart(t *testing.T) {
	in := time.Date(2026, 10, 31, 23, 59, 0, 0, time.FixedZone("x", -5*3600)) // Nov 1 04:59 UTC
	if got, want := MonthStart(in), time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Errorf("MonthStart = %v, want %v", got, want)
	}
}
