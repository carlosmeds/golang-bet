package domain

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

// UUID is a 128-bit identifier. Its canonical text form is lowercase
// 8-4-4-4-12 hexadecimal. The zero value is the nil UUID and is never a valid
// identifier for a domain entity.
type UUID [16]byte

// NewUUID returns a random (version 4) UUID. It panics only if the operating
// system entropy source fails, which is unrecoverable.
func NewUUID() UUID {
	var u UUID
	if _, err := rand.Read(u[:]); err != nil {
		panic(fmt.Sprintf("domain: entropy source failure: %v", err))
	}
	u[6] = (u[6] & 0x0f) | 0x40
	u[8] = (u[8] & 0x3f) | 0x80
	return u
}

// ParseUUID parses the canonical 8-4-4-4-12 form in either letter case and
// normalizes it. Braces, URN prefixes and unhyphenated forms are rejected.
func ParseUUID(s string) (UUID, error) {
	var u UUID
	if len(s) != 36 || s[8] != '-' || s[13] != '-' || s[18] != '-' || s[23] != '-' {
		return u, newInvalid(CodeInvalidIdentifier, "uuid must use the canonical 8-4-4-4-12 form")
	}
	raw := make([]byte, 0, 32)
	for i := 0; i < len(s); i++ {
		switch i {
		case 8, 13, 18, 23:
			continue
		}
		raw = append(raw, s[i])
	}
	if _, err := hex.Decode(u[:], raw); err != nil {
		return UUID{}, newInvalid(CodeInvalidIdentifier, "uuid contains non-hexadecimal characters")
	}
	return u, nil
}

// IsNil reports whether u is the nil UUID.
func (u UUID) IsNil() bool { return u == UUID{} }

// String returns the canonical lowercase form.
func (u UUID) String() string {
	var buf [36]byte
	hex.Encode(buf[0:8], u[0:4])
	buf[8] = '-'
	hex.Encode(buf[9:13], u[4:6])
	buf[13] = '-'
	hex.Encode(buf[14:18], u[6:8])
	buf[18] = '-'
	hex.Encode(buf[19:23], u[8:10])
	buf[23] = '-'
	hex.Encode(buf[24:], u[10:])
	return string(buf[:])
}

// MarshalText implements encoding.TextMarshaler.
func (u UUID) MarshalText() ([]byte, error) { return []byte(u.String()), nil }

// UnmarshalText implements encoding.TextUnmarshaler.
func (u *UUID) UnmarshalText(b []byte) error {
	parsed, err := ParseUUID(string(b))
	if err != nil {
		return err
	}
	*u = parsed
	return nil
}

// IDGenerator produces identifiers for new entities and events. Callers inject
// deterministic generators in tests; nil means NewUUID.
type IDGenerator func() UUID

func (g IDGenerator) next() UUID {
	if g == nil {
		return NewUUID()
	}
	return g()
}
