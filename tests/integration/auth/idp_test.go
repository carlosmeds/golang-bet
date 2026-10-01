// Package auth_test verifies the service's token verifier against the real
// Keycloak container (REQ-059, REQ-060, REQ-078): tokens issued by the IdP are
// accepted with the right identity, and every kind of missing, invalid,
// forged, foreign or expired credential is refused.
package auth_test

import (
	"context"
	"crypto"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"wagering/internal/auth"
	"wagering/tests/integration/auth/idptest"
)

func newVerifier(t *testing.T, base string, mutate ...func(*auth.Options)) *auth.Verifier {
	t.Helper()
	opts := auth.Options{Issuer: idptest.Issuer(base), Audience: idptest.Audience}
	for _, m := range mutate {
		m(&opts)
	}
	v, err := auth.New(opts)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func verify(v *auth.Verifier, token string) (auth.Identity, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return v.Verify(ctx, token)
}

func TestIdPIssuedTokensAreAcceptedWithTheirIdentity(t *testing.T) {
	base := idptest.KeycloakURL(t)
	v := newVerifier(t, base) // discovery through the issuer

	a, err := verify(v, idptest.Token(t, base, idptest.ProviderA))
	if err != nil {
		t.Fatalf("provider-a token: %v", err)
	}
	if !a.IsProvider() || a.IsInternal() || a.ProviderID != "provider-a" {
		t.Fatalf("provider-a identity = %+v scopes %v", a, a.Scopes())
	}
	b, err := verify(v, idptest.Token(t, base, idptest.ProviderB))
	if err != nil {
		t.Fatalf("provider-b token: %v", err)
	}
	if !b.IsProvider() || b.IsInternal() || b.ProviderID != "provider-b" {
		t.Fatalf("provider-b identity = %+v scopes %v", b, b.Scopes())
	}
	if a.Subject == b.Subject || a.Subject == "" {
		t.Fatalf("providers must have distinct subjects: %q %q", a.Subject, b.Subject)
	}
	in, err := verify(v, idptest.Token(t, base, idptest.Internal))
	if err != nil {
		t.Fatalf("internal token: %v", err)
	}
	if !in.IsInternal() || in.IsProvider() || in.ProviderID != "" {
		t.Fatalf("internal identity = %+v scopes %v", in, in.Scopes())
	}
}

func TestExplicitJWKSURLAcceptsTheSameTokens(t *testing.T) {
	base := idptest.KeycloakURL(t)
	v := newVerifier(t, base, func(o *auth.Options) {
		o.JWKSURL = base + "/realms/" + idptest.Realm + "/protocol/openid-connect/certs"
	})
	if _, err := verify(v, idptest.Token(t, base, idptest.ProviderA)); err != nil {
		t.Fatal(err)
	}
}

func TestIdPTokenEndpointIssuesNothingWithoutValidClientCredentials(t *testing.T) {
	base := idptest.KeycloakURL(t)
	for name, form := range map[string]url.Values{
		"wrong secret":   {"grant_type": {"client_credentials"}, "client_id": {"provider-a"}, "client_secret": {"not-the-secret"}},
		"unknown client": {"grant_type": {"client_credentials"}, "client_id": {"nobody"}, "client_secret": {"x"}},
		"no secret":      {"grant_type": {"client_credentials"}, "client_id": {"provider-a"}},
		"password grant": {"grant_type": {"password"}, "client_id": {"provider-a"}, "client_secret": {"provider-a-secret"}, "username": {"u"}, "password": {"p"}},
	} {
		t.Run(name, func(t *testing.T) {
			r, err := idptest.RequestToken(base, idptest.Realm, form)
			if err != nil {
				t.Fatal(err)
			}
			if r.Status < 400 || r.AccessToken != "" {
				t.Fatalf("IdP issued a token: status %d", r.Status)
			}
		})
	}
}

func TestProvisionedClaimsAreIsolatedPerClient(t *testing.T) {
	base := idptest.KeycloakURL(t)
	iss := idptest.Issuer(base)
	for _, tc := range []struct {
		client   idptest.Client
		provider string
		roles    string
	}{
		{idptest.ProviderA, "provider-a", "wagering:provider"},
		{idptest.ProviderB, "provider-b", "wagering:provider"},
		{idptest.Internal, "", "wagering:internal"},
	} {
		c := idptest.Claims(t, idptest.Token(t, base, tc.client))
		if c["iss"] != iss || c["aud"] != idptest.Audience || c["roles"] != tc.roles {
			t.Errorf("%s: iss=%v aud=%v roles=%v", tc.client.ID, c["iss"], c["aud"], c["roles"])
		}
		if got, _ := c["provider_id"].(string); got != tc.provider {
			t.Errorf("%s: provider_id=%q want %q", tc.client.ID, got, tc.provider)
		}
	}
}

// forge builds a compact JWT from header and claims with the given signer.
func forge(t *testing.T, header, claims map[string]any, sign func(signingInput string) []byte) string {
	t.Helper()
	enc := func(v any) string {
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return base64.RawURLEncoding.EncodeToString(raw)
	}
	in := enc(header) + "." + enc(claims)
	return in + "." + base64.RawURLEncoding.EncodeToString(sign(in))
}

func TestInvalidCredentialsAreRefused(t *testing.T) {
	base := idptest.KeycloakURL(t)
	v := newVerifier(t, base)
	good := idptest.Token(t, base, idptest.ProviderA)
	other := idptest.Token(t, base, idptest.Internal)
	if _, err := verify(v, good); err != nil { // warm keys; proves the baseline is accepted
		t.Fatal(err)
	}
	parts := strings.Split(good, ".")
	otherParts := strings.Split(other, ".")
	realHeader := idptest.Header(t, good)
	realClaims := idptest.Claims(t, good)

	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	signRSA := func(in string) []byte {
		sum := sha256.Sum256([]byte(in))
		sig, err := rsa.SignPKCS1v15(rand.Reader, rsaKey, crypto.SHA256, sum[:])
		if err != nil {
			t.Fatal(err)
		}
		return sig
	}
	withClaims := func(mut func(map[string]any)) map[string]any {
		c := map[string]any{}
		for k, v := range realClaims {
			c[k] = v
		}
		mut(c)
		return c
	}
	hmacSign := func(in string) []byte {
		m := hmac.New(sha256.New, []byte("guess"))
		m.Write([]byte(in))
		return m.Sum(nil)
	}
	noneHeader := map[string]any{"alg": "none", "typ": "JWT"}

	cases := map[string]string{
		"empty":                  "",
		"garbage":                "not-a-jwt",
		"two segments":           parts[0] + "." + parts[1],
		"signature removed":      parts[0] + "." + parts[1] + ".",
		"signature flipped":      parts[0] + "." + parts[1] + "." + flip(parts[2]),
		"payload of other token": parts[0] + "." + otherParts[1] + "." + parts[2],
		"privilege escalation: internal roles on provider signature": forgePayloadSwap(t, parts, map[string]any{"roles": "wagering:internal"}, realClaims),
		"alg none":                      forge(t, noneHeader, realClaims, func(string) []byte { return nil }),
		"alg HS256":                     forge(t, map[string]any{"alg": "HS256", "typ": "JWT", "kid": realHeader["kid"]}, realClaims, hmacSign),
		"attacker RSA key, real kid":    forge(t, map[string]any{"alg": "RS256", "typ": "JWT", "kid": realHeader["kid"]}, realClaims, signRSA),
		"attacker RSA key, unknown kid": forge(t, map[string]any{"alg": "RS256", "typ": "JWT", "kid": "attacker"}, realClaims, signRSA),
		"attacker key, other provider": forge(t, map[string]any{"alg": "RS256", "typ": "JWT", "kid": realHeader["kid"]},
			withClaims(func(c map[string]any) { c["provider_id"] = "provider-b" }), signRSA),
		"real token of the master realm":        idptest.MasterToken(t, base),
		"real token without audience or claims": idptest.Token(t, base, idptest.NoClaims),
	}
	for name, token := range cases {
		t.Run(name, func(t *testing.T) {
			id, err := verify(v, token)
			if err == nil {
				t.Fatalf("accepted as %+v", id)
			}
			if !errors.Is(err, auth.ErrInvalidToken) && !errors.Is(err, auth.ErrMissingCredentials) {
				t.Fatalf("error = %v, want invalid/missing credentials", err)
			}
		})
	}
}

// forgePayloadSwap keeps the IdP's header and signature but swaps claims, so
// only the signature check can catch it.
func forgePayloadSwap(t *testing.T, parts []string, override, claims map[string]any) string {
	t.Helper()
	c := map[string]any{}
	for k, v := range claims {
		c[k] = v
	}
	for k, v := range override {
		c[k] = v
	}
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	return parts[0] + "." + base64.RawURLEncoding.EncodeToString(raw) + "." + parts[2]
}

func flip(sig string) string {
	b := []byte(sig)
	if b[0] == 'A' {
		b[0] = 'B'
	} else {
		b[0] = 'A'
	}
	return string(b)
}

func TestRealTokenIsRefusedForAnotherAudienceOrIssuer(t *testing.T) {
	base := idptest.KeycloakURL(t)
	token := idptest.Token(t, base, idptest.ProviderA)
	for name, mutate := range map[string]func(*auth.Options){
		"audience": func(o *auth.Options) { o.Audience = "another-service" },
		// Keycloak's built-in client must not be accepted as the API audience (F-9).
		"built-in account audience": func(o *auth.Options) { o.Audience = "account" },
		// The realm's keys are reachable, but the expected issuer differs.
		"issuer": func(o *auth.Options) {
			o.Issuer = idptest.Issuer(base) + "-other"
			o.JWKSURL = base + "/realms/" + idptest.Realm + "/protocol/openid-connect/certs"
		},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := verify(newVerifier(t, base, mutate), token); !errors.Is(err, auth.ErrInvalidToken) {
				t.Fatalf("error = %v, want ErrInvalidToken", err)
			}
		})
	}
}

func TestExpiredTokenIsRefusedOnceItsLifetimeEnds(t *testing.T) {
	base := idptest.KeycloakURL(t)
	v := newVerifier(t, base)
	if _, err := verify(v, idptest.Token(t, base, idptest.ProviderA)); err != nil { // warm discovery + keys
		t.Fatal(err)
	}
	token, expiresAt := idptest.ShortLivedToken(t, base, 4*time.Second)

	id, err := verify(v, token)
	if err != nil {
		t.Skipf("token expired before it could be checked (slow environment): %v", err)
	}
	if id.ProviderID != "provider-short" || !id.IsProvider() {
		t.Fatalf("identity before expiry = %+v", id)
	}
	time.Sleep(time.Until(expiresAt) + 1500*time.Millisecond)
	if _, err = verify(v, token); !errors.Is(err, auth.ErrExpiredToken) {
		t.Fatalf("after expiry error = %v, want ErrExpiredToken", err)
	}
}

func TestVerifierFailsClosedWhenTheIdPIsUnreachable(t *testing.T) {
	base := idptest.KeycloakURL(t)
	token := idptest.Token(t, base, idptest.ProviderA)
	// Nothing listens on port 1: no keys can be fetched, so nothing is trusted.
	v := newVerifier(t, base, func(o *auth.Options) {
		o.JWKSURL = "http://127.0.0.1:1/certs"
		o.HTTPClient = &http.Client{Timeout: 2 * time.Second}
	})
	if _, err := verify(v, token); !errors.Is(err, auth.ErrKeysUnavailable) {
		t.Fatalf("error = %v, want ErrKeysUnavailable", err)
	}
}
