package dto

import (
	"encoding/hex"
	"fmt"

	"github.com/google/uuid"
)

// ID is a Jellyfin GUID. On the wire it is .NET's "N" format: 32 lower-case
// hex digits, no dashes. Input accepts N, dashed ("D"), braced and upper-case
// forms, since clients echo IDs back in whatever form they have.
type ID uuid.UUID

// IDFromUUID converts a database UUID to a wire ID.
func IDFromUUID(u uuid.UUID) ID { return ID(u) }

// ParseID parses any GUID spelling. The empty string is the zero ID.
func ParseID(s string) (ID, error) {
	if s == "" {
		return ID{}, nil
	}
	u, err := uuid.Parse(s)
	if err != nil {
		return ID{}, fmt.Errorf("invalid id %q: %w", s, err)
	}
	return ID(u), nil
}

// UUID returns the database form.
func (id ID) UUID() uuid.UUID { return uuid.UUID(id) }

// IsZero reports whether id is the all-zero GUID (Jellyfin's Guid.Empty).
func (id ID) IsZero() bool { return id == ID{} }

// String returns the N format.
func (id ID) String() string { return hex.EncodeToString(id[:]) }

// MarshalText implements encoding.TextMarshaler (used for JSON values and map keys).
func (id ID) MarshalText() ([]byte, error) { return []byte(id.String()), nil }

// UnmarshalText implements encoding.TextUnmarshaler.
func (id *ID) UnmarshalText(b []byte) error {
	v, err := ParseID(string(b))
	if err != nil {
		return err
	}
	*id = v
	return nil
}
