// Package urlsign signs blockbustr's stream URLs (TASKS P3.10, DESIGN §6):
// an HMAC over the item, the media source and an expiry, so a URL handed to
// a client stops working when it expires and can't be altered to point at
// another source.
package urlsign

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strconv"
	"time"
)

// Errors from Verify.
var (
	ErrExpired = errors.New("urlsign: expired")
	ErrInvalid = errors.New("urlsign: invalid signature")
)

// Signer signs with Key; TTL is how long a signature lasts.
type Signer struct {
	Key []byte
	TTL time.Duration
}

// Sign returns the expiry (unix seconds) and signature for a stream of
// item's media source ms, valid TTL from now.
func (s *Signer) Sign(item, ms string, now time.Time) (expires, sig string) {
	expires = strconv.FormatInt(now.Add(s.TTL).Unix(), 10)
	return expires, s.mac(item, ms, expires)
}

// Verify checks a signature made by Sign.
func (s *Signer) Verify(item, ms, expires, sig string, now time.Time) error {
	exp, err := strconv.ParseInt(expires, 10, 64)
	if err != nil {
		return ErrInvalid
	}
	if !hmac.Equal([]byte(sig), []byte(s.mac(item, ms, expires))) {
		return ErrInvalid
	}
	if now.Unix() > exp {
		return ErrExpired
	}
	return nil
}

func (s *Signer) mac(item, ms, expires string) string {
	m := hmac.New(sha256.New, s.Key)
	m.Write([]byte("stream\x00" + item + "\x00" + ms + "\x00" + expires))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}
