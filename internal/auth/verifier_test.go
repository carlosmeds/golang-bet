package auth

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"strings"
	"testing"
	"time"
)

func setup(t *testing.T) (*idp, rsaKey, *Verifier) {
	t.Helper()
	p := newIDP(t)
	k := newRSAKey(t, testKid, 2048)
	p.publish(k.jwk())
	return p, k, p.verifier(t)
}

func TestVerifyValidProviderToken(t *testing.T) {
	p, k, v := setup(t)
	id, err := v.Verify(context.Background(), k.sign(t, hdr("RS256", testKid), claimsFor(p, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if id.Subject != "svc-provider-a" || id.ProviderID != "provider-a" || id.ClientID != "provider-a-client" {
		t.Fatalf("identity = %+v", id)
	}
	if !id.IsProvider() || id.IsInternal() {
		t.Fatalf("expected provider-only identity: %+v", id)
	}
	if got := id.Scopes(); len(got) != 2 || got[0] != "openid" || got[1] != ScopeProvider {
		t.Fatalf("scopes = %v", got)
	}
}

func TestVerifyES256(t *testing.T) {
	p := newIDP(t)
	k := newECKey(t, "ec-1")
	p.publish(k.jwk())
	v := p.verifier(t)
	if _, err := v.Verify(context.Background(), k.sign(t, hdr("ES256", "ec-1"), claimsFor(p, nil))); err != nil {
		t.Fatal(err)
	}
	// A valid ES256 header over an RSA-shaped signature length must fail.
	bad := k.sign(t, hdr("ES256", "ec-1"), claimsFor(p, nil))
	bad = bad[:strings.LastIndex(bad, ".")+1] + b64(make([]byte, 64))
	if _, err := v.Verify(context.Background(), bad); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("err = %v", err)
	}
}

func TestVerifyInternalScopeVariants(t *testing.T) {
	p, k, v := setup(t)
	cases := map[string]map[string]any{
		"scope string": {"scope": ScopeInternal, "provider_id": nil},
		"scp array":    {"scope": nil, "scp": []string{ScopeInternal}, "provider_id": nil},
		"roles":        {"scope": nil, "roles": []string{ScopeInternal}, "provider_id": nil},
		"realm role": {"scope": nil, "provider_id": nil,
			"realm_access": map[string]any{"roles": []string{ScopeInternal}}},
	}
	for name, o := range cases {
		t.Run(name, func(t *testing.T) {
			id, err := v.Verify(context.Background(), k.sign(t, hdr("RS256", testKid), claimsFor(p, o)))
			if err != nil {
				t.Fatal(err)
			}
			if !id.IsInternal() || id.IsProvider() {
				t.Fatalf("identity = %+v", id)
			}
		})
	}
}

func TestVerifyAudienceArray(t *testing.T) {
	p, k, v := setup(t)
	tok := k.sign(t, hdr("RS256", testKid), claimsFor(p, map[string]any{"aud": []string{"other", testAudience}}))
	if _, err := v.Verify(context.Background(), tok); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyRejects(t *testing.T) {
	p, k, v := setup(t)
	other := newRSAKey(t, testKid, 2048) // same kid, different key material
	ec := newECKey(t, testKid)
	good := k.sign(t, hdr("RS256", testKid), claimsFor(p, nil))
	parts := strings.Split(good, ".")
	tamperedClaims := claimsFor(p, map[string]any{"provider_id": "provider-b"})

	// HS256 token signed with the JWKS public modulus as the secret (alg confusion).
	hsSigned := encode(t, hdr("HS256", testKid)) + "." + encode(t, claimsFor(p, nil))
	mac := hmac.New(sha256.New, k.key.N.Bytes())
	mac.Write([]byte(hsSigned))
	hs256 := hsSigned + "." + b64(mac.Sum(nil))

	noneSigned := encode(t, hdr("none", testKid)) + "." + encode(t, claimsFor(p, nil)) + "."

	cases := []struct {
		name  string
		token string
		want  error
	}{
		{"empty", "", ErrMissingCredentials},
		{"garbage", "not-a-jwt", ErrInvalidToken},
		{"two parts", parts[0] + "." + parts[1], ErrInvalidToken},
		{"four parts", good + ".x", ErrInvalidToken},
		{"alg none", noneSigned, ErrInvalidToken},
		{"alg HS256 confusion", hs256, ErrInvalidToken},
		{"alg RS256 header on EC key", ecKeyToken(t, ec, p), ErrInvalidToken},
		{"wrong signing key", other.sign(t, hdr("RS256", testKid), claimsFor(p, nil)), ErrInvalidToken},
		{"tampered payload", parts[0] + "." + encode(t, tamperedClaims) + "." + parts[2], ErrInvalidToken},
		{"empty signature", parts[0] + "." + parts[1] + ".", ErrInvalidToken},
		{"unknown kid", k.sign(t, hdr("RS256", "nope"), claimsFor(p, nil)), ErrInvalidToken},
		{"missing kid", k.sign(t, map[string]any{"alg": "RS256"}, claimsFor(p, nil)), ErrInvalidToken},
		{"crit header", k.sign(t, map[string]any{"alg": "RS256", "kid": testKid, "crit": []string{"x"}, "x": 1}, claimsFor(p, nil)), ErrInvalidToken},
		{"wrong issuer", k.sign(t, hdr("RS256", testKid), claimsFor(p, map[string]any{"iss": "https://evil.example"})), ErrInvalidToken},
		{"missing issuer", k.sign(t, hdr("RS256", testKid), claimsFor(p, map[string]any{"iss": nil})), ErrInvalidToken},
		{"wrong audience", k.sign(t, hdr("RS256", testKid), claimsFor(p, map[string]any{"aud": "other"})), ErrInvalidToken},
		{"missing audience", k.sign(t, hdr("RS256", testKid), claimsFor(p, map[string]any{"aud": nil})), ErrInvalidToken},
		{"expired", k.sign(t, hdr("RS256", testKid), claimsFor(p, map[string]any{"exp": testNow.Add(-time.Second).Unix()})), ErrExpiredToken},
		{"expiring now", k.sign(t, hdr("RS256", testKid), claimsFor(p, map[string]any{"exp": testNow.Unix()})), ErrExpiredToken},
		{"missing exp", k.sign(t, hdr("RS256", testKid), claimsFor(p, map[string]any{"exp": nil})), ErrInvalidToken},
		{"string exp", k.sign(t, hdr("RS256", testKid), claimsFor(p, map[string]any{"exp": "9999999999"})), ErrInvalidToken},
		{"not yet valid", k.sign(t, hdr("RS256", testKid), claimsFor(p, map[string]any{"nbf": testNow.Add(time.Minute).Unix()})), ErrInvalidToken},
		{"missing sub", k.sign(t, hdr("RS256", testKid), claimsFor(p, map[string]any{"sub": nil})), ErrInvalidToken},
		{"non-string provider", k.sign(t, hdr("RS256", testKid), claimsFor(p, map[string]any{"provider_id": 7})), ErrInvalidToken},
		{"blank provider", k.sign(t, hdr("RS256", testKid), claimsFor(p, map[string]any{"provider_id": " "})), ErrInvalidToken},
		{"oversized provider", k.sign(t, hdr("RS256", testKid), claimsFor(p, map[string]any{"provider_id": strings.Repeat("a", 256)})), ErrInvalidToken},
		{"malformed scope", k.sign(t, hdr("RS256", testKid), claimsFor(p, map[string]any{"scope": 5})), ErrInvalidToken},
		{"oversized token", strings.Repeat("a", maxTokenBytes+1), ErrInvalidToken},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id, err := v.Verify(context.Background(), tc.token)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if id.Subject != "" || id.ProviderID != "" || len(id.Scopes()) != 0 {
				t.Fatalf("rejected token leaked identity: %+v", id)
			}
		})
	}
}

func ecKeyToken(t *testing.T, k ecKey, p *idp) string {
	t.Helper()
	return k.sign(t, hdr("RS256", k.kid), claimsFor(p, nil))
}

func TestClockSkewAndBoundaries(t *testing.T) {
	p := newIDP(t)
	k := newRSAKey(t, testKid, 2048)
	p.publish(k.jwk())
	v := p.verifier(t, func(o *Options) { o.ClockSkew = 10 * time.Second })
	within := k.sign(t, hdr("RS256", testKid), claimsFor(p, map[string]any{"exp": testNow.Add(-5 * time.Second).Unix()}))
	if _, err := v.Verify(context.Background(), within); err != nil {
		t.Fatalf("within skew: %v", err)
	}
	beyond := k.sign(t, hdr("RS256", testKid), claimsFor(p, map[string]any{"exp": testNow.Add(-11 * time.Second).Unix()}))
	if _, err := v.Verify(context.Background(), beyond); !errors.Is(err, ErrExpiredToken) {
		t.Fatalf("beyond skew: %v", err)
	}
	nbf := k.sign(t, hdr("RS256", testKid), claimsFor(p, map[string]any{"nbf": testNow.Add(5 * time.Second).Unix()}))
	if _, err := v.Verify(context.Background(), nbf); err != nil {
		t.Fatalf("nbf within skew: %v", err)
	}
}

func TestCustomProviderClaim(t *testing.T) {
	p, k, _ := setup(t)
	v := p.verifier(t, func(o *Options) { o.ProviderClaim = "tenant" })
	tok := k.sign(t, hdr("RS256", testKid), claimsFor(p, map[string]any{"tenant": "provider-z"}))
	id, err := v.Verify(context.Background(), tok)
	if err != nil || id.ProviderID != "provider-z" {
		t.Fatalf("id=%+v err=%v", id, err)
	}
	// With a custom claim, the default claim name is ignored.
	plain := k.sign(t, hdr("RS256", testKid), claimsFor(p, nil))
	if id, err := v.Verify(context.Background(), plain); err != nil || id.ProviderID != "" {
		t.Fatalf("id=%+v err=%v", id, err)
	}
}

func TestNewValidatesOptions(t *testing.T) {
	for name, o := range map[string]Options{
		"no issuer":   {Audience: "a"},
		"no audience": {Issuer: "https://i"},
		"neg skew":    {Issuer: "https://i", Audience: "a", ClockSkew: -1},
	} {
		if _, err := New(o); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}
