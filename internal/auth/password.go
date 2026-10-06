package auth

import (
	"errors"

	"golang.org/x/crypto/bcrypt"
)

// ErrPasswordTooLong: bcrypt only uses the first 72 bytes, so longer
// passwords are rejected instead of being silently truncated.
var ErrPasswordTooLong = errors.New("auth: password longer than 72 bytes")

// HashPassword returns a bcrypt hash of pw.
func HashPassword(pw string) (string, error) {
	h, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
	if errors.Is(err, bcrypt.ErrPasswordTooLong) {
		return "", ErrPasswordTooLong
	}
	return string(h), err
}

// CheckPassword reports whether pw matches hash. A user without a password
// (nil hash, which Jellyfin allows) signs in with an empty password.
func CheckPassword(hash *string, pw string) bool {
	if hash == nil {
		return pw == ""
	}
	return bcrypt.CompareHashAndPassword([]byte(*hash), []byte(pw)) == nil
}
