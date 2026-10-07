package urlsign

import (
	"errors"
	"testing"
	"time"
)

func TestSignVerify(t *testing.T) {
	s := &Signer{Key: []byte("0123456789abcdef0123456789abcdef"), TTL: time.Hour}
	now := time.Unix(1_700_000_000, 0)
	exp, sig := s.Sign("item", "ms", now)
	if exp != "1700003600" {
		t.Errorf("expires = %s", exp)
	}
	if err := s.Verify("item", "ms", exp, sig, now.Add(59*time.Minute)); err != nil {
		t.Errorf("valid: %v", err)
	}
	cases := map[string]struct {
		item, ms, exp, sig string
		at                 time.Time
		want               error
	}{
		"expired":      {"item", "ms", exp, sig, now.Add(61 * time.Minute), ErrExpired},
		"other source": {"item", "ms2", exp, sig, now, ErrInvalid},
		"other item":   {"item2", "ms", exp, sig, now, ErrInvalid},
		"later expiry": {"item", "ms", "1800000000", sig, now, ErrInvalid},
		"bad expiry":   {"item", "ms", "soon", sig, now, ErrInvalid},
		"tampered sig": {"item", "ms", exp, sig[:len(sig)-1] + "A", now, ErrInvalid},
		"empty sig":    {"item", "ms", exp, "", now, ErrInvalid},
	}
	for name, c := range cases {
		if err := s.Verify(c.item, c.ms, c.exp, c.sig, c.at); !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", name, err, c.want)
		}
	}
	other := &Signer{Key: []byte("another key, also thirty-two by!"), TTL: time.Hour}
	if err := other.Verify("item", "ms", exp, sig, now); !errors.Is(err, ErrInvalid) {
		t.Errorf("another key: %v", err)
	}
}
