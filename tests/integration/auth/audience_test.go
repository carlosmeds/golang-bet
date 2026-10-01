package auth_test

import (
	"encoding/json"
	"os"
	"regexp"
	"testing"

	"wagering/tests/integration/auth/idptest"
)

// The API audience is the dedicated wagering-api audience, never Keycloak's
// built-in "account" client (F-9): compose, the realm and the harness agree.
func TestDedicatedAPIAudienceIsConfiguredConsistently(t *testing.T) {
	if idptest.Audience != "wagering-api" {
		t.Fatalf("harness audience = %q, want wagering-api", idptest.Audience)
	}
	compose, err := os.ReadFile("../../../compose.yaml")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^\s*-\s*OIDC_AUDIENCE=(\S+)\s*$`).FindSubmatch(compose)
	if m == nil || string(m[1]) != idptest.Audience {
		t.Fatalf("compose OIDC_AUDIENCE = %q, want %q", m, idptest.Audience)
	}

	raw, err := os.ReadFile("../../../infra/keycloak/realm-export.json")
	if err != nil {
		t.Fatal(err)
	}
	var realm struct {
		Clients []struct {
			ClientID        string `json:"clientId"`
			ProtocolMappers []struct {
				ProtocolMapper string            `json:"protocolMapper"`
				Config         map[string]string `json:"config"`
			} `json:"protocolMappers"`
		} `json:"clients"`
	}
	if err := json.Unmarshal(raw, &realm); err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"provider-a": true, "provider-b": true, "internal-service": true}
	for _, c := range realm.Clients {
		var audiences []string
		for _, pm := range c.ProtocolMappers {
			if pm.ProtocolMapper != "oidc-audience-mapper" {
				continue
			}
			for _, k := range []string{"included.custom.audience", "included.client.audience"} {
				if v := pm.Config[k]; v != "" {
					audiences = append(audiences, v)
				}
			}
		}
		for _, a := range audiences {
			if a != idptest.Audience {
				t.Errorf("client %s maps audience %q, want only %q", c.ClientID, a, idptest.Audience)
			}
		}
		if want[c.ClientID] {
			if len(audiences) != 1 {
				t.Errorf("client %s audience mappers = %v, want [%s]", c.ClientID, audiences, idptest.Audience)
			}
			delete(want, c.ClientID)
		}
	}
	if len(want) != 0 {
		t.Errorf("realm clients missing the audience mapper: %v", want)
	}
}
