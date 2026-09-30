package auth

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	maxResponseBytes = 1 << 20
	minRSABits       = 2048
)

// signingKey is a verified-usable public key. alg is the only JWS algorithm
// the key may be used with, so a token cannot choose a weaker one.
type signingKey struct {
	alg string
	pub crypto.PublicKey
}

type keySetConfig struct {
	issuer   string
	jwksURL  string
	client   *http.Client
	now      func() time.Time
	ttl      time.Duration
	cooldown time.Duration
	timeout  time.Duration
}

type flight struct {
	done chan struct{}
	err  error
}

// keySet caches the provider's JWKS. Refreshes are shared between callers,
// rate-limited (so random kids cannot turn the service into a fetch
// amplifier) and never tied to a single caller's cancellation.
type keySet struct {
	cfg keySetConfig

	mu        sync.Mutex
	keys      map[string]signingKey
	jwksURL   string
	fetchedAt time.Time // last successful fetch
	attemptAt time.Time // last attempt, successful or not
	inflight  *flight
}

func newKeySet(cfg keySetConfig) *keySet {
	return &keySet{cfg: cfg, jwksURL: cfg.jwksURL}
}

// key returns the key for kid, refreshing the set when kid is unknown or the
// cache is stale.
func (k *keySet) key(ctx context.Context, kid string) (signingKey, error) {
	k.mu.Lock()
	key, ok := k.keys[kid]
	now := k.cfg.now()
	stale := k.keys == nil || now.Sub(k.fetchedAt) >= k.cfg.ttl
	canTry := k.keys == nil || k.inflight != nil || now.Sub(k.attemptAt) >= k.cfg.cooldown
	k.mu.Unlock()

	if ok && !stale {
		return key, nil
	}
	if !canTry {
		if ok {
			return key, nil
		}
		return signingKey{}, fmt.Errorf("%w: unknown kid", ErrInvalidToken)
	}

	err := k.refresh(ctx)
	k.mu.Lock()
	key, ok = k.keys[kid]
	k.mu.Unlock()
	if ok {
		// A stale-but-present key is still usable when the refresh failed.
		return key, nil
	}
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return signingKey{}, ctxErr
		}
		return signingKey{}, fmt.Errorf("%w: %v", ErrKeysUnavailable, err)
	}
	return signingKey{}, fmt.Errorf("%w: unknown kid", ErrInvalidToken)
}

func (k *keySet) refresh(ctx context.Context) error {
	k.mu.Lock()
	f := k.inflight
	if f == nil {
		f = &flight{done: make(chan struct{})}
		k.inflight = f
		k.attemptAt = k.cfg.now()
		// The fetch outlives the caller that started it: other callers wait on
		// the same flight.
		fetchCtx := context.WithoutCancel(ctx)
		go func() {
			err := k.fetch(fetchCtx)
			k.mu.Lock()
			f.err = err
			k.inflight = nil
			k.mu.Unlock()
			close(f.done)
		}()
	}
	k.mu.Unlock()

	select {
	case <-f.done:
		return f.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (k *keySet) fetch(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, k.cfg.timeout)
	defer cancel()

	k.mu.Lock()
	url := k.jwksURL
	k.mu.Unlock()
	if url == "" {
		discovered, err := k.discover(ctx)
		if err != nil {
			return err
		}
		url = discovered
		k.mu.Lock()
		k.jwksURL = url
		k.mu.Unlock()
	}

	var doc struct {
		Keys []jwk `json:"keys"`
	}
	if err := k.getJSON(ctx, url, &doc); err != nil {
		return fmt.Errorf("fetch jwks: %w", err)
	}
	keys := make(map[string]signingKey, len(doc.Keys))
	for _, j := range doc.Keys {
		key, err := j.signingKey()
		if err != nil {
			continue // unusable keys are ignored; others stay valid
		}
		keys[j.Kid] = key
	}
	if len(keys) == 0 {
		return errors.New("fetch jwks: no usable signing keys")
	}
	k.mu.Lock()
	k.keys = keys
	k.fetchedAt = k.cfg.now()
	k.mu.Unlock()
	return nil
}

func (k *keySet) discover(ctx context.Context) (string, error) {
	var doc struct {
		Issuer  string `json:"issuer"`
		JWKSURI string `json:"jwks_uri"`
	}
	url := strings.TrimRight(k.cfg.issuer, "/") + "/.well-known/openid-configuration"
	if err := k.getJSON(ctx, url, &doc); err != nil {
		return "", fmt.Errorf("oidc discovery: %w", err)
	}
	if doc.Issuer != k.cfg.issuer {
		return "", errors.New("oidc discovery: issuer mismatch")
	}
	if doc.JWKSURI == "" {
		return "", errors.New("oidc discovery: missing jwks_uri")
	}
	return doc.JWKSURI, nil
}

func (k *keySet) getJSON(ctx context.Context, url string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := k.cfg.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(out)
}

type jwk struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Use string `json:"use"`
	Alg string `json:"alg"`
	N   string `json:"n"`
	E   string `json:"e"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

func (j jwk) signingKey() (signingKey, error) {
	if j.Kid == "" {
		return signingKey{}, errors.New("missing kid")
	}
	if j.Use != "" && j.Use != "sig" {
		return signingKey{}, errors.New("not a signing key")
	}
	switch j.Kty {
	case "RSA":
		if j.Alg != "" && j.Alg != AlgRS256 {
			return signingKey{}, errors.New("unsupported alg")
		}
		n, err := b64Int(j.N)
		if err != nil {
			return signingKey{}, err
		}
		e, err := b64Int(j.E)
		if err != nil || !e.IsInt64() || e.Int64() < 3 || e.Int64() > 1<<31-1 {
			return signingKey{}, errors.New("bad exponent")
		}
		if n.BitLen() < minRSABits {
			return signingKey{}, errors.New("rsa key too small")
		}
		return signingKey{alg: AlgRS256, pub: &rsa.PublicKey{N: n, E: int(e.Int64())}}, nil
	case "EC":
		if j.Crv != "P-256" || (j.Alg != "" && j.Alg != AlgES256) {
			return signingKey{}, errors.New("unsupported curve")
		}
		x, err := b64Int(j.X)
		if err != nil {
			return signingKey{}, err
		}
		y, err := b64Int(j.Y)
		if err != nil {
			return signingKey{}, err
		}
		pub := &ecdsa.PublicKey{Curve: elliptic.P256(), X: x, Y: y}
		if !pub.Curve.IsOnCurve(x, y) {
			return signingKey{}, errors.New("point not on curve")
		}
		return signingKey{alg: AlgES256, pub: pub}, nil
	}
	return signingKey{}, errors.New("unsupported key type")
}

func b64Int(s string) (*big.Int, error) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil || len(b) == 0 {
		return nil, errors.New("bad base64url integer")
	}
	return new(big.Int).SetBytes(b), nil
}
