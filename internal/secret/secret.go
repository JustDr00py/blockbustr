// Package secret seals short secrets (debrid API keys) with AES-256-GCM
// before they are stored in Postgres (DESIGN §4, debrid_accounts).
package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
)

// KeyLen is the AES-256 key length in bytes.
const KeyLen = 32

// ErrBadKey reports a secret_key that is not 32 bytes of hex or base64.
var ErrBadKey = errors.New("secret: key must be 32 bytes, hex or base64 encoded")

// ParseKey decodes a key from hex (64 chars) or base64 (std/URL, padded or
// raw). Anything that is not exactly KeyLen bytes fails with ErrBadKey.
func ParseKey(s string) ([]byte, error) {
	if b, err := hex.DecodeString(s); err == nil {
		if len(b) == KeyLen {
			return b, nil
		}
		return nil, ErrBadKey
	}
	encodings := []*base64.Encoding{
		base64.StdEncoding, base64.RawStdEncoding,
		base64.URLEncoding, base64.RawURLEncoding,
	}
	for _, enc := range encodings {
		if b, err := enc.DecodeString(s); err == nil {
			if len(b) == KeyLen {
				return b, nil
			}
			return nil, ErrBadKey
		}
	}
	return nil, ErrBadKey
}

// Seal encrypts plaintext under key, returning nonce ‖ ciphertext+tag. Every
// call produces a different blob (random nonce).
func Seal(key, plaintext []byte) ([]byte, error) {
	aead, err := newAEAD(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("secret: nonce: %w", err)
	}
	return aead.Seal(nonce, nonce, plaintext, nil), nil
}

// Open decrypts a blob produced by Seal. It fails if the blob is truncated,
// was sealed with another key, or was modified.
func Open(key, blob []byte) ([]byte, error) {
	aead, err := newAEAD(key)
	if err != nil {
		return nil, err
	}
	if len(blob) < aead.NonceSize()+aead.Overhead() {
		return nil, errors.New("secret: blob too short")
	}
	plain, err := aead.Open(nil, blob[:aead.NonceSize()], blob[aead.NonceSize():], nil)
	if err != nil {
		return nil, fmt.Errorf("secret: open: %w", err)
	}
	return plain, nil
}

func newAEAD(key []byte) (cipher.AEAD, error) {
	if len(key) != KeyLen {
		return nil, ErrBadKey
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("secret: cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("secret: gcm: %w", err)
	}
	return aead, nil
}
