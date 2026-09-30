package auth

import "errors"

// Authentication failures. All of them map to HTTP 401 except
// ErrKeysUnavailable, which fails closed with 503.
var (
	ErrMissingCredentials = errors.New("auth: missing credentials")
	ErrInvalidToken       = errors.New("auth: invalid token")
	ErrExpiredToken       = errors.New("auth: token expired")
	ErrKeysUnavailable    = errors.New("auth: signing keys unavailable")
)

// Authorization failures. They map to HTTP 403.
var (
	ErrForbidden        = errors.New("auth: forbidden")
	ErrProviderMismatch = errors.New("auth: provider mismatch")
)
