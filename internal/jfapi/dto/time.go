package dto

import (
	"bytes"
	"fmt"
	"time"
)

// Time is a timestamp as Jellyfin's System.Text.Json writes it (verified
// against every 12.1.0 capture): UTC with "Z", 100 ns precision, and
//   - a non-zero fraction trimmed of trailing zeros: "2026-10-05T22:25:34.529331Z"
//   - a zero fraction written as seven zeros:        "2010-12-21T00:00:00.0000000Z"
type Time struct{ time.Time }

const (
	// trimmedLayout uses 9s so trailing fractional zeros are dropped, and
	// stops at 7 digits like .NET ticks.
	trimmedLayout  = "2006-01-02T15:04:05.9999999Z07:00"
	wholeSecLayout = "2006-01-02T15:04:05.0000000Z07:00"
)

// Accepted input layouts, most specific first. Clients sometimes send
// timestamps without a zone (.NET "unspecified" kind); those are taken as UTC.
var inputLayouts = []string{
	time.RFC3339Nano,
	"2006-01-02T15:04:05.999999999",
	"2006-01-02",
}

// NewTime wraps t.
func NewTime(t time.Time) Time { return Time{t} }

// String returns the wire form.
func (t Time) String() string {
	u := t.UTC().Truncate(100 * time.Nanosecond)
	if u.Nanosecond() == 0 {
		return u.Format(wholeSecLayout)
	}
	return u.Format(trimmedLayout)
}

// MarshalText implements encoding.TextMarshaler.
func (t Time) MarshalText() ([]byte, error) { return []byte(t.String()), nil }

// UnmarshalText implements encoding.TextUnmarshaler.
func (t *Time) UnmarshalText(b []byte) error {
	s := string(b)
	for _, layout := range inputLayouts {
		if v, err := time.Parse(layout, s); err == nil {
			t.Time = v.UTC()
			return nil
		}
	}
	return fmt.Errorf("invalid timestamp %q", s)
}

// MarshalJSON and UnmarshalJSON are needed explicitly: the embedded
// time.Time's own JSON methods would otherwise be promoted and win over the
// text methods.

// MarshalJSON implements json.Marshaler.
func (t Time) MarshalJSON() ([]byte, error) { return []byte(`"` + t.String() + `"`), nil }

// UnmarshalJSON implements json.Unmarshaler. JSON null leaves t unchanged.
func (t *Time) UnmarshalJSON(b []byte) error {
	if bytes.Equal(b, []byte("null")) {
		return nil
	}
	if len(b) < 2 || b[0] != '"' || b[len(b)-1] != '"' {
		return fmt.Errorf("invalid timestamp %s", b)
	}
	return t.UnmarshalText(b[1 : len(b)-1])
}
