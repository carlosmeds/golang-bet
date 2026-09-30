package auth

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"wagering/internal/contract"
)

// TokenVerifier is what the middleware needs from a Verifier.
type TokenVerifier interface {
	Verify(ctx context.Context, raw string) (Identity, error)
}

// Error codes in the HTTP error envelope.
const (
	CodeUnauthenticated = "UNAUTHENTICATED"
	CodeForbidden       = "FORBIDDEN"
	CodeUnavailable     = "TEMPORARILY_UNAVAILABLE"
)

// Middleware authenticates and authorizes HTTP requests. Rejected requests
// never reach the wrapped handler, so they cannot have financial effects.
type Middleware struct {
	verifier TokenVerifier
	log      *slog.Logger
	// CorrelationID, when set, supplies the correlation ID for error bodies.
	CorrelationID func(*http.Request) string
}

func NewMiddleware(v TokenVerifier) *Middleware {
	return &Middleware{verifier: v, log: slog.Default()}
}

// Authenticate requires a valid bearer token and stores its Identity in the
// request context: 401 for absent, invalid or expired credentials, 503 when
// the IdP keys cannot be obtained.
func (m *Middleware) Authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := bearerToken(r)
		if err == nil {
			var id Identity
			id, err = m.verifier.Verify(r.Context(), raw)
			if err == nil {
				next.ServeHTTP(w, r.WithContext(WithIdentity(r.Context(), id)))
				return
			}
		}
		m.reject(w, r, err)
	})
}

// RequireProvider allows only provider principals (provider scope plus a
// provider claim). Place it after Authenticate.
func (m *Middleware) RequireProvider(next http.Handler) http.Handler {
	return m.require(next, func(id Identity) bool { return id.IsProvider() })
}

// RequireInternal allows only internal service principals. Place it after
// Authenticate.
func (m *Middleware) RequireInternal(next http.Handler) http.Handler {
	return m.require(next, func(id Identity) bool { return id.IsInternal() })
}

// Provider is Authenticate followed by RequireProvider.
func (m *Middleware) Provider(next http.Handler) http.Handler {
	return m.Authenticate(m.RequireProvider(next))
}

// Internal is Authenticate followed by RequireInternal.
func (m *Middleware) Internal(next http.Handler) http.Handler {
	return m.Authenticate(m.RequireInternal(next))
}

func (m *Middleware) require(next http.Handler, allowed func(Identity) bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := FromContext(r.Context())
		if !ok {
			m.reject(w, r, ErrMissingCredentials) // wiring error: fail closed
			return
		}
		if !allowed(id) {
			m.reject(w, r, ErrForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// WriteError renders err (as returned by Verify, AuthorizeProvider or
// ProviderScope) as the stable error envelope. Handlers use it for
// ErrProviderMismatch; it reports whether err was an auth error.
func (m *Middleware) WriteError(w http.ResponseWriter, r *http.Request, err error) bool {
	if !isAuthError(err) {
		return false
	}
	m.reject(w, r, err)
	return true
}

func isAuthError(err error) bool {
	for _, target := range []error{
		ErrMissingCredentials, ErrInvalidToken, ErrExpiredToken, ErrKeysUnavailable,
		ErrForbidden, ErrProviderMismatch,
	} {
		if errors.Is(err, target) {
			return true
		}
	}
	return false
}

func (m *Middleware) reject(w http.ResponseWriter, r *http.Request, err error) {
	var (
		status int
		body   contract.ErrorBody
	)
	switch {
	case errors.Is(err, ErrForbidden), errors.Is(err, ErrProviderMismatch):
		status = http.StatusForbidden
		body = contract.ErrorBody{Code: CodeForbidden, Message: "not allowed to perform this operation"}
	case errors.Is(err, ErrKeysUnavailable), isContextError(err):
		status = http.StatusServiceUnavailable
		body = contract.ErrorBody{Code: CodeUnavailable, Message: "temporarily unavailable"}
		w.Header().Set("Retry-After", "5")
	default:
		status = http.StatusUnauthorized
		body = contract.ErrorBody{Code: CodeUnauthenticated, Message: "authentication required"}
		challenge := `Bearer realm="wagering"`
		switch {
		case errors.Is(err, ErrExpiredToken):
			challenge += `, error="invalid_token", error_description="token expired"`
		case errors.Is(err, ErrInvalidToken):
			challenge += `, error="invalid_token"`
		}
		w.Header().Set("WWW-Authenticate", challenge)
	}
	if m.CorrelationID != nil {
		body.CorrelationID = m.CorrelationID(r)
	}
	// The reason is logged, never returned: clients learn only the class.
	m.log.WarnContext(r.Context(), "request rejected", "status", status, "reason", err.Error(), "path", r.URL.Path)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func isContextError(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// bearerToken extracts the token from exactly one Authorization header using
// the Bearer scheme (case-insensitive per RFC 7235).
func bearerToken(r *http.Request) (string, error) {
	values := r.Header.Values("Authorization")
	if len(values) == 0 {
		return "", ErrMissingCredentials
	}
	if len(values) > 1 {
		return "", invalid("multiple authorization headers")
	}
	scheme, token, found := strings.Cut(values[0], " ")
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return "", invalid("unsupported authorization scheme")
	}
	token = strings.TrimSpace(token)
	if token == "" || strings.ContainsAny(token, " \t") {
		return "", invalid("malformed bearer token")
	}
	return token, nil
}
