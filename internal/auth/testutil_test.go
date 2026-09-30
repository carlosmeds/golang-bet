package auth

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	testAudience = "wagering-api"
	testKid      = "key-1"
)

var testNow = time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)

// idp is a fake OIDC provider serving discovery and a JWKS.
type idp struct {
	t       *testing.T
	server  *httptest.Server
	mu      sync.Mutex
	keys    []jwk
	down    atomic.Bool
	jwksHit atomic.Int32
	discHit atomic.Int32
}

func newIDP(t *testing.T) *idp {
	t.Helper()
	p := &idp{t: t}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		p.discHit.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]string{"issuer": p.server.URL, "jwks_uri": p.server.URL + "/jwks"})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		p.jwksHit.Add(1)
		if p.down.Load() {
			http.Error(w, "down", http.StatusInternalServerError)
			return
		}
		p.mu.Lock()
		defer p.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": p.keys})
	})
	p.server = httptest.NewServer(mux)
	t.Cleanup(p.server.Close)
	return p
}

func (p *idp) issuer() string { return p.server.URL }

func (p *idp) publish(keys ...jwk) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.keys = keys
}

func (p *idp) verifier(t *testing.T, mutate ...func(*Options)) *Verifier {
	t.Helper()
	o := Options{Issuer: p.issuer(), Audience: testAudience, Now: func() time.Time { return testNow }}
	for _, m := range mutate {
		m(&o)
	}
	v, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

type rsaKey struct {
	kid string
	key *rsa.PrivateKey
}

func newRSAKey(t *testing.T, kid string, bits int) rsaKey {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, bits)
	if err != nil {
		t.Fatal(err)
	}
	return rsaKey{kid: kid, key: k}
}

func (k rsaKey) jwk() jwk {
	return jwk{Kty: "RSA", Kid: k.kid, Use: "sig", Alg: "RS256",
		N: b64(k.key.N.Bytes()), E: b64(big.NewInt(int64(k.key.E)).Bytes())}
}

func (k rsaKey) sign(t *testing.T, header, claims map[string]any) string {
	t.Helper()
	signed := encode(t, header) + "." + encode(t, claims)
	d := sha256.Sum256([]byte(signed))
	sig, err := rsa.SignPKCS1v15(rand.Reader, k.key, crypto.SHA256, d[:])
	if err != nil {
		t.Fatal(err)
	}
	return signed + "." + b64(sig)
}

type ecKey struct {
	kid string
	key *ecdsa.PrivateKey
}

func newECKey(t *testing.T, kid string) ecKey {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return ecKey{kid: kid, key: k}
}

func (k ecKey) jwk() jwk {
	return jwk{Kty: "EC", Kid: k.kid, Use: "sig", Alg: "ES256", Crv: "P-256",
		X: b64(k.key.X.FillBytes(make([]byte, 32))), Y: b64(k.key.Y.FillBytes(make([]byte, 32)))}
}

func (k ecKey) sign(t *testing.T, header, claims map[string]any) string {
	t.Helper()
	signed := encode(t, header) + "." + encode(t, claims)
	d := sha256.Sum256([]byte(signed))
	r, s, err := ecdsa.Sign(rand.Reader, k.key, d[:])
	if err != nil {
		t.Fatal(err)
	}
	sig := append(r.FillBytes(make([]byte, 32)), s.FillBytes(make([]byte, 32))...)
	return signed + "." + b64(sig)
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func encode(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b64(b)
}

func hdr(alg, kid string) map[string]any { return map[string]any{"alg": alg, "kid": kid, "typ": "JWT"} }

// claimsFor returns valid provider claims for the IdP; tests override fields.
func claimsFor(p *idp, overrides map[string]any) map[string]any {
	c := map[string]any{
		"iss":         p.issuer(),
		"aud":         testAudience,
		"sub":         "svc-provider-a",
		"azp":         "provider-a-client",
		"exp":         testNow.Add(5 * time.Minute).Unix(),
		"iat":         testNow.Unix(),
		"scope":       "openid " + ScopeProvider,
		"provider_id": "provider-a",
	}
	for k, v := range overrides {
		if v == nil {
			delete(c, k)
		} else {
			c[k] = v
		}
	}
	return c
}
