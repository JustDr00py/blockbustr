package dto

import (
	"encoding/json"
	"testing"
	"time"
)

func TestTimeWireFormat(t *testing.T) {
	cases := []struct {
		in   time.Time
		want string
	}{
		// Trailing zeros trimmed, as in the 12.1.0 captures.
		{time.Date(2026, 10, 5, 22, 25, 34, 529331000, time.UTC), "2026-10-05T22:25:34.529331Z"},
		{time.Date(2026, 10, 5, 22, 39, 53, 123456700, time.UTC), "2026-10-05T22:39:53.1234567Z"},
		// Below 100 ns is cut, like .NET ticks.
		{time.Date(2026, 10, 5, 22, 39, 53, 123456789, time.UTC), "2026-10-05T22:39:53.1234567Z"},
		// A zero fraction is written as seven zeros (dates, DateTime.MinValue).
		{time.Date(2010, 12, 21, 0, 0, 0, 0, time.UTC), "2010-12-21T00:00:00.0000000Z"},
		{time.Time{}, "0001-01-01T00:00:00.0000000Z"},
		// Only sub-100 ns: truncates to a zero fraction.
		{time.Date(2026, 10, 5, 22, 0, 0, 50, time.UTC), "2026-10-05T22:00:00.0000000Z"},
		// Converted to UTC.
		{time.Date(2026, 10, 5, 15, 0, 0, 0, time.FixedZone("PDT", -7*3600)), "2026-10-05T22:00:00.0000000Z"},
	}
	for _, c := range cases {
		if got := NewTime(c.in).String(); got != c.want {
			t.Errorf("%v → %s, want %s", c.in, got, c.want)
		}
	}
}

func TestTimeParse(t *testing.T) {
	want := time.Date(2026, 10, 5, 22, 0, 0, 500000000, time.UTC)
	for _, in := range []string{
		"2026-10-05T22:00:00.5Z",
		"2026-10-05T22:00:00.5000000Z",
		"2026-10-05T15:00:00.5-07:00",
		"2026-10-05T22:00:00.5", // no zone → UTC
	} {
		var tm Time
		if err := tm.UnmarshalText([]byte(in)); err != nil || !tm.Equal(want) || tm.Location() != time.UTC {
			t.Errorf("%q → %v, %v", in, tm.Time, err)
		}
	}
	var d Time
	if err := d.UnmarshalText([]byte("2026-10-05")); err != nil || d.Day() != 5 {
		t.Errorf("date only: %v %v", d, err)
	}
	if err := d.UnmarshalText([]byte("yesterday")); err == nil {
		t.Error("expected error")
	}
}

func TestTimeJSON(t *testing.T) {
	type doc struct {
		At  Time
		Opt *Time `json:",omitempty"`
	}
	b, err := json.Marshal(doc{At: NewTime(time.Date(2026, 1, 2, 3, 4, 5, 600000000, time.UTC))})
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"At":"2026-01-02T03:04:05.6Z"}` {
		t.Errorf("marshal = %s (embedded time.Time's MarshalJSON must not win)", b)
	}
	var d doc
	if err := json.Unmarshal([]byte(`{"At":null,"Opt":"2026-01-02T03:04:05Z"}`), &d); err != nil || !d.At.IsZero() || d.Opt == nil {
		t.Errorf("unmarshal = %+v, %v", d, err)
	}
	if err := json.Unmarshal([]byte(`{"At":12}`), &d); err == nil {
		t.Error("expected error for non-string")
	}
}

func TestTicks(t *testing.T) {
	if got := TicksFromDuration(90 * time.Minute); got != 54_000_000_000 {
		t.Errorf("90m = %d ticks", got)
	}
	if got := DurationFromTicks(57152000000); got != 95*time.Minute+15*time.Second+200*time.Millisecond {
		t.Errorf("Luca runtime = %v", got)
	}
	if TicksFromDuration(DurationFromTicks(123456789)) != 123456789 {
		t.Error("round trip")
	}
	if TicksPerSecond != int64(time.Second/100) {
		t.Error("TicksPerSecond")
	}
}
