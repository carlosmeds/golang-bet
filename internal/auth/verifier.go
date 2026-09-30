package auth

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Supported JWS algorithms. Anything else, including "none" and HMAC, is
// rejected before any key lookup.
const (
	AlgRS256 = "RS256"
	AlgES256 = "ES256"
)

const maxTokenBytes = 16 << 10

// Options configures a Verifier. Issuer and Audience are required.
type Options struct {
	Issuer   string
	Audience string
	// JWKSURL overrides OIDC discovery. It is needed when the service reaches
	// the IdP under a different host than the one in the token's issuer.
	JWKSURL string
	// ProviderClaim names the claim carrying the provider ID (default
	// "provider_id").
	ProviderClaim string
	// ClockSkew is leeway for exp/nbf (default 0).
	ClockSkew time.Duration
	// HTTPClient fetches discovery and JWKS (default: 5s timeout client).
	HTTPClient *http.Client
	// KeyTTL is how long a fetched key set is fresh (default 10m).
	KeyTTL time.Duration
	// KeyRefreshCooldown is the minimum gap between JWKS fetches (default 10s).
	KeyRefreshCooldown time.Duration
	// Now is the clock (default time.Now).
	Now func() time.Time
}

// Verifier validates bearer tokens against an OIDC provider's JWKS.
type Verifier struct {
	issuer   string
	audience string
	provider string
	skew     time.Duration
	now      func() time.Time
	keys     *keySet
}

// New builds a Verifier. It performs no network I/O; keys are fetched lazily.
func New(opts Options) (*Verifier, error) {
	if strings.TrimSpace(opts.Issuer) == "" {
		return nil, errors.New("auth: issuer is required")
	}
	if strings.TrimSpace(opts.Audience) == "" {
		return nil, errors.New("auth: audience is required")
	}
	if opts.ClockSkew < 0 {
		return nil, errors.New("auth: clock skew must not be negative")
	}
	v := &Verifier{
		issuer:   opts.Issuer,
		audience: opts.Audience,
		provider: opts.ProviderClaim,
		skew:     opts.ClockSkew,
		now:      opts.Now,
	}
	if v.provider == "" {
		v.provider = "provider_id"
	}
	if v.now == nil {
		v.now = time.Now
	}
	client := opts.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	ttl, cooldown := opts.KeyTTL, opts.KeyRefreshCooldown
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}
	if cooldown <= 0 {
		cooldown = 10 * time.Second
	}
	v.keys = newKeySet(keySetConfig{
		issuer: opts.Issuer, jwksURL: opts.JWKSURL, client: client, now: v.now,
		ttl: ttl, cooldown: cooldown, timeout: 5 * time.Second,
	})
	return v, nil
}

type header struct {
	Alg  string           `json:"alg"`
	Kid  string           `json:"kid"`
	Crit *json.RawMessage `json:"crit"`
}

// Verify validates a raw compact JWT and returns the identity it carries.
// Errors wrap ErrInvalidToken, ErrExpiredToken or ErrKeysUnavailable; a
// context error is returned as is.
func (v *Verifier) Verify(ctx context.Context, raw string) (Identity, error) {
	if err := ctx.Err(); err != nil {
		return Identity{}, err
	}
	if raw == "" {
		return Identity{}, ErrMissingCredentials
	}
	if len(raw) > maxTokenBytes {
		return Identity{}, invalid("token too large")
	}
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return Identity{}, invalid("not a compact JWT")
	}
	var h header
	if err := decodeSegment(parts[0], &h); err != nil {
		return Identity{}, invalid("bad header")
	}
	if h.Alg != AlgRS256 && h.Alg != AlgES256 {
		return Identity{}, invalid("unsupported algorithm")
	}
	if h.Crit != nil {
		return Identity{}, invalid("unsupported critical header")
	}
	if h.Kid == "" {
		return Identity{}, invalid("missing kid")
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(sig) == 0 {
		return Identity{}, invalid("bad signature encoding")
	}

	key, err := v.keys.key(ctx, h.Kid)
	if err != nil {
		return Identity{}, err
	}
	if key.alg != h.Alg {
		return Identity{}, invalid("algorithm does not match key")
	}
	if !verifySignature(key, []byte(parts[0]+"."+parts[1]), sig) {
		return Identity{}, invalid("bad signature")
	}

	var claims map[string]json.RawMessage
	if err := decodeSegment(parts[1], &claims); err != nil {
		return Identity{}, invalid("bad claims")
	}
	return v.identity(claims)
}

func verifySignature(key signingKey, signed, sig []byte) bool {
	digest := sha256.Sum256(signed)
	switch pub := key.pub.(type) {
	case *rsa.PublicKey:
		return rsa.VerifyPKCS1v15(pub, crypto.SHA256, digest[:], sig) == nil
	case *ecdsa.PublicKey:
		if len(sig) != 64 {
			return false
		}
		r := new(big.Int).SetBytes(sig[:32])
		s := new(big.Int).SetBytes(sig[32:])
		return ecdsa.Verify(pub, digest[:], r, s)
	}
	return false
}

func (v *Verifier) identity(c map[string]json.RawMessage) (Identity, error) {
	if iss, ok := stringClaim(c, "iss"); !ok || iss != v.issuer {
		return Identity{}, invalid("issuer mismatch")
	}
	if !audienceContains(c["aud"], v.audience) {
		return Identity{}, invalid("audience mismatch")
	}
	now := v.now()
	exp, ok := timeClaim(c, "exp")
	if !ok {
		return Identity{}, invalid("missing exp")
	}
	if !now.Before(exp.Add(v.skew)) {
		return Identity{}, fmt.Errorf("%w", ErrExpiredToken)
	}
	if raw, present := c["nbf"]; present {
		nbf, ok := numericTime(raw)
		if !ok {
			return Identity{}, invalid("bad nbf")
		}
		if now.Add(v.skew).Before(nbf) {
			return Identity{}, invalid("token not yet valid")
		}
	}
	sub, ok := stringClaim(c, "sub")
	if !ok || sub == "" {
		return Identity{}, invalid("missing sub")
	}

	id := Identity{Subject: sub, scopes: map[string]struct{}{}}
	if azp, ok := stringClaim(c, "azp"); ok {
		id.ClientID = azp
	} else if cid, ok := stringClaim(c, "client_id"); ok {
		id.ClientID = cid
	}
	if raw, present := c[v.provider]; present {
		var p string
		if err := json.Unmarshal(raw, &p); err != nil || !validProviderID(p) {
			return Identity{}, invalid("bad provider claim")
		}
		id.ProviderID = p
	}
	if err := addScopes(id.scopes, c["scope"]); err != nil {
		return Identity{}, err
	}
	if err := addScopes(id.scopes, c["scp"]); err != nil {
		return Identity{}, err
	}
	// Keycloak-style role grants ("roles" or "realm_access.roles") carry the
	// same names as scopes.
	if err := addScopes(id.scopes, c["roles"]); err != nil {
		return Identity{}, err
	}
	if raw, ok := c["realm_access"]; ok {
		var ra struct {
			Roles json.RawMessage `json:"roles"`
		}
		if err := json.Unmarshal(raw, &ra); err != nil {
			return Identity{}, invalid("bad realm_access claim")
		}
		if err := addScopes(id.scopes, ra.Roles); err != nil {
			return Identity{}, err
		}
	}
	return id, nil
}

func invalid(reason string) error { return fmt.Errorf("%w: %s", ErrInvalidToken, reason) }

func decodeSegment(seg string, out any) error {
	b, err := base64.RawURLEncoding.DecodeString(seg)
	if err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	if err := dec.Decode(out); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("trailing data")
	}
	return nil
}

func stringClaim(c map[string]json.RawMessage, name string) (string, bool) {
	raw, ok := c[name]
	if !ok {
		return "", false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", false
	}
	return s, true
}

func timeClaim(c map[string]json.RawMessage, name string) (time.Time, bool) {
	raw, ok := c[name]
	if !ok {
		return time.Time{}, false
	}
	return numericTime(raw)
}

func numericTime(raw json.RawMessage) (time.Time, bool) {
	if len(raw) == 0 || raw[0] == '"' {
		return time.Time{}, false // json.Number would accept a quoted string
	}
	var n json.Number
	if err := json.Unmarshal(raw, &n); err != nil {
		return time.Time{}, false
	}
	f, err := n.Float64()
	if err != nil || f < 0 || f > 1<<53 {
		return time.Time{}, false
	}
	sec := int64(f)
	return time.Unix(sec, int64((f-float64(sec))*1e9)), true
}

func audienceContains(raw json.RawMessage, want string) bool {
	if len(raw) == 0 {
		return false
	}
	var one string
	if err := json.Unmarshal(raw, &one); err == nil {
		return one == want
	}
	var many []string
	if err := json.Unmarshal(raw, &many); err != nil {
		return false
	}
	for _, a := range many {
		if a == want {
			return true
		}
	}
	return false
}

// addScopes accepts the OAuth "scope" string (space separated) or an array, as
// used by the "scp" claim. A present-but-malformed value fails closed.
func addScopes(dst map[string]struct{}, raw json.RawMessage) error {
	if len(raw) == 0 {
		return nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		for _, f := range strings.Fields(s) {
			dst[f] = struct{}{}
		}
		return nil
	}
	var list []string
	if err := json.Unmarshal(raw, &list); err != nil {
		return invalid("bad scope claim")
	}
	for _, f := range list {
		if f != "" {
			dst[f] = struct{}{}
		}
	}
	return nil
}

// validProviderID mirrors the contract for opaque identifiers: non-empty, at
// most 255 bytes, valid UTF-8, no control characters or surrounding space.
func validProviderID(s string) bool {
	if s == "" || len(s) > 255 || !utf8.ValidString(s) || strings.TrimSpace(s) != s {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}
