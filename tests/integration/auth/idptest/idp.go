// Package idptest is the shared harness for the container integration tests
// of authentication and HTTP authorization. It talks to the real Keycloak
// container provisioned by Compose; nothing here issues or signs tokens for
// the service under test, except the deliberately forged ones built by tests.
//
// Environment (tests skip when a variable is unset):
//
//	WAGERING_TEST_KEYCLOAK_URL  e.g. http://localhost:8082
//	WAGERING_TEST_ADMIN_URL     PostgreSQL admin DSN, e.g.
//	                            postgres://wagering:wagering_password@localhost:54320/wagering?sslmode=disable
//
// Optional: WAGERING_TEST_KEYCLOAK_ADMIN_USER / _PASSWORD (default admin/admin).
package idptest

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

// Realm and audience provisioned by infra/keycloak/realm-export.json.
const (
	Realm    = "wagering"
	Audience = "wagering-api"
)

// Client is a Keycloak service-account client from the provisioned realm.
type Client struct{ ID, Secret string }

var (
	// ProviderA and ProviderB carry provider_id and roles=wagering:provider.
	ProviderA = Client{"provider-a", "provider-a-secret"}
	ProviderB = Client{"provider-b", "provider-b-secret"}
	// Internal carries roles=wagering:internal and no provider_id.
	Internal = Client{"internal-service", "internal-secret"}
	// NoClaims is a valid realm client whose tokens have no audience,
	// provider or role claims.
	NoClaims = Client{"wagering-client", "wagering-secret"}
)

// KeycloakURL returns the Keycloak base URL or skips the test.
func KeycloakURL(t testing.TB) string {
	t.Helper()
	v := strings.TrimRight(strings.TrimSpace(os.Getenv("WAGERING_TEST_KEYCLOAK_URL")), "/")
	if v == "" {
		t.Skip("set WAGERING_TEST_KEYCLOAK_URL (e.g. http://localhost:8082) for real IdP integration tests")
	}
	return v
}

// Issuer is the `iss` that tokens fetched through base carry.
func Issuer(base string) string { return base + "/realms/" + Realm }

var httpClient = &http.Client{Timeout: 15 * time.Second}

// TokenResponse is the IdP's token endpoint answer.
type TokenResponse struct {
	Status      int
	AccessToken string `json:"access_token"`
	ExpiresIn   int    `json:"expires_in"`
	Error       string `json:"error"`
}

// RequestToken performs a client-credentials grant in realm.
func RequestToken(base, realm string, form url.Values) (TokenResponse, error) {
	req, err := http.NewRequest(http.MethodPost, base+"/realms/"+realm+"/protocol/openid-connect/token", strings.NewReader(form.Encode()))
	if err != nil {
		return TokenResponse{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := httpClient.Do(req)
	if err != nil {
		return TokenResponse{}, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	out := TokenResponse{Status: resp.StatusCode}
	_ = json.Unmarshal(body, &out)
	return out, nil
}

// Token fetches a real access token for c.
func Token(t testing.TB, base string, c Client) string {
	t.Helper()
	r, err := RequestToken(base, Realm, url.Values{"grant_type": {"client_credentials"}, "client_id": {c.ID}, "client_secret": {c.Secret}})
	if err != nil {
		t.Fatalf("token request for %s: %v", c.ID, err)
	}
	if r.Status != http.StatusOK || r.AccessToken == "" {
		t.Fatalf("token request for %s: status %d error %q (is the Compose realm current?)", c.ID, r.Status, r.Error)
	}
	return r.AccessToken
}

// MasterToken returns an access token of the IdP's master realm. It is a real,
// validly signed token that belongs to a different issuer.
func MasterToken(t testing.TB, base string) string {
	t.Helper()
	r, err := RequestToken(base, "master", url.Values{"grant_type": {"password"}, "client_id": {"admin-cli"}, "username": {adminUser()}, "password": {adminPassword()}})
	if err != nil || r.Status != http.StatusOK {
		t.Fatalf("master realm token: status %d err %v", r.Status, err)
	}
	return r.AccessToken
}

func adminUser() string { return envOr("WAGERING_TEST_KEYCLOAK_ADMIN_USER", "admin") }
func adminPassword() string {
	return envOr("WAGERING_TEST_KEYCLOAK_ADMIN_PASSWORD", "admin")
}
func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

// ShortLivedToken registers a throwaway client whose access tokens live for
// lifespan (a per-client Keycloak setting, so other realm clients and other
// test runs are unaffected), fetches one token and returns it with the
// instant it expires. The client is deleted when the test ends. The token
// carries provider_id "provider-short", roles=wagering:provider and the
// realm audience, exactly like a provider client.
func ShortLivedToken(t testing.TB, base string, lifespan time.Duration) (token string, expiresAt time.Time) {
	t.Helper()
	admin := MasterToken(t, base)
	clientID := fmt.Sprintf("t16-short-%d", time.Now().UnixNano())
	secret := "short-lived-secret"
	mapper := func(name, typ string, cfg map[string]string) map[string]any {
		return map[string]any{"name": name, "protocol": "openid-connect", "protocolMapper": typ, "config": cfg}
	}
	claim := func(name, claimName, value string) map[string]any {
		return mapper(name, "oidc-hardcoded-claim-mapper", map[string]string{
			"claim.name": claimName, "claim.value": value, "jsonType.label": "String",
			"access.token.claim": "true", "id.token.claim": "false", "userinfo.token.claim": "false",
		})
	}
	spec := map[string]any{
		"clientId": clientID, "secret": secret, "enabled": true, "protocol": "openid-connect",
		"publicClient": false, "serviceAccountsEnabled": true, "standardFlowEnabled": false, "directAccessGrantsEnabled": false,
		"attributes": map[string]string{"access.token.lifespan": fmt.Sprint(int(lifespan.Seconds()))},
		"protocolMappers": []any{
			claim("provider-mapper", "provider_id", "provider-short"),
			claim("scope-mapper", "roles", "wagering:provider"),
			mapper("audience-mapper", "oidc-audience-mapper", map[string]string{"included.client.audience": Audience, "access.token.claim": "true", "id.token.claim": "false"}),
		},
	}
	body, _ := json.Marshal(spec)
	adminReq := func(method, path string, payload []byte) *http.Response {
		req, err := http.NewRequestWithContext(context.Background(), method, base+"/admin/realms/"+Realm+path, bytes.NewReader(payload))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+admin)
		req.Header.Set("Content-Type", "application/json")
		resp, err := httpClient.Do(req)
		if err != nil {
			t.Fatalf("keycloak admin %s %s: %v", method, path, err)
		}
		return resp
	}
	resp := adminReq(http.MethodPost, "/clients", body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create short-lived client: status %d", resp.StatusCode)
	}
	location := resp.Header.Get("Location")
	id := location[strings.LastIndex(location, "/")+1:]
	t.Cleanup(func() {
		r := adminReq(http.MethodDelete, "/clients/"+id, nil)
		r.Body.Close()
	})
	issued := time.Now()
	r, err := RequestToken(base, Realm, url.Values{"grant_type": {"client_credentials"}, "client_id": {clientID}, "client_secret": {secret}})
	if err != nil || r.Status != http.StatusOK {
		t.Fatalf("short-lived token: status %d err %v", r.Status, err)
	}
	if r.ExpiresIn > int(lifespan.Seconds()) {
		t.Fatalf("IdP ignored the per-client lifespan: expires_in=%d", r.ExpiresIn)
	}
	return r.AccessToken, issued.Add(time.Duration(r.ExpiresIn) * time.Second)
}

// Claims decodes the payload of a compact JWT without verifying it.
func Claims(t testing.TB, token string) map[string]any {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("not a compact JWT: %d parts", len(parts))
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// Header decodes the JOSE header of a compact JWT.
func Header(t testing.TB, token string) map[string]any {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(strings.Split(token, ".")[0])
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}
