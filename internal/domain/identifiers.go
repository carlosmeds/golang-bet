package domain

import (
	"unicode"
	"unicode/utf8"
)

// MaxIdentifierLength bounds opaque provider-supplied identifiers.
const MaxIdentifierLength = 255

// validateText checks an opaque identifier: non-empty, no leading/trailing
// whitespace (never silently trimmed), no control characters, valid UTF-8 and
// at most MaxIdentifierLength bytes.
func validateText(field, v string) error {
	if v == "" {
		return newInvalid(CodeInvalidIdentifier, "%s is required", field)
	}
	if len(v) > MaxIdentifierLength {
		return newInvalid(CodeInvalidIdentifier, "%s exceeds %d bytes", field, MaxIdentifierLength)
	}
	if !utf8.ValidString(v) {
		return newInvalid(CodeInvalidIdentifier, "%s is not valid UTF-8", field)
	}
	first, _ := utf8.DecodeRuneInString(v)
	last, _ := utf8.DecodeLastRuneInString(v)
	if unicode.IsSpace(first) || unicode.IsSpace(last) {
		return newInvalid(CodeInvalidIdentifier, "%s has leading or trailing whitespace", field)
	}
	for _, r := range v {
		if unicode.IsControl(r) {
			return newInvalid(CodeInvalidIdentifier, "%s contains control characters", field)
		}
	}
	return nil
}

func validateUUID(field string, u UUID) error {
	if u.IsNil() {
		return newInvalid(CodeInvalidIdentifier, "%s is required", field)
	}
	return nil
}

// validateHash accepts a lowercase hex SHA-256 digest.
func validateHash(field, v string) error {
	if len(v) != 64 {
		return newInvalid(CodeInvalidIdentifier, "%s must be a 64-character lowercase hex SHA-256", field)
	}
	for i := 0; i < len(v); i++ {
		c := v[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return newInvalid(CodeInvalidIdentifier, "%s must be a 64-character lowercase hex SHA-256", field)
		}
	}
	return nil
}
