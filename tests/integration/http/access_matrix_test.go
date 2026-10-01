package httpauth_test

import (
	"net/http"
	"testing"
	"time"

	"wagering/tests/integration/auth/idptest"
)

type principal struct {
	name     string
	token    string
	raw      string // verbatim Authorization header
	provider string // provider the principal acts as ("" if none)
	class    string // anonymous | rejected | provider | internal | noscope
}

type endpoint struct {
	name   string
	class  string // "internal" or "provider": the scope the route needs
	method string
	path   func(f *fixture, p principal) string
	body   func(f *fixture, p principal) any
	key    bool // sends an Idempotency-Key
	ok     int  // status when authorized
}

// fixture holds the data the matrix endpoints operate on.
type fixture struct {
	wallet, player string
	seed           map[string]seeded // provider -> an existing transaction of that provider
	w              *world
}

type seeded struct{ id, external string }

func newFixture(t *testing.T, w *world) *fixture {
	f := &fixture{w: w, wallet: w.wallet(t, "1000.00"), seed: map[string]seeded{}}
	f.player = w.player(t, f.wallet)
	for provider, token := range map[string]string{"provider-a": w.provA, "provider-b": w.provB} {
		ext := w.next("seed-" + provider)
		r := w.bet(t, token, provider, f.wallet, f.player, ext, w.next("seed-key"), "1.00")
		f.seed[provider] = seeded{id: r.JSON(t)["transactionId"].(string), external: ext}
	}
	return f
}

func endpoints(w *world) []endpoint {
	return []endpoint{
		{name: "POST /wallets", class: "internal", method: http.MethodPost, ok: http.StatusCreated,
			path: func(*fixture, principal) string { return "/wallets" },
			body: func(*fixture, principal) any {
				return map[string]any{"playerId": w.next("matrix-player"), "initialBalance": map[string]string{"amount": "5.00", "currency": "BRL"}}
			}},
		{name: "GET /wallets/{id}", class: "internal", method: http.MethodGet, ok: http.StatusOK,
			path: func(f *fixture, _ principal) string { return "/wallets/" + f.wallet }},
		{name: "GET /wallets/{id}/ledger", class: "internal", method: http.MethodGet, ok: http.StatusOK,
			path: func(f *fixture, _ principal) string { return "/wallets/" + f.wallet + "/ledger" }},
		{name: "POST /wallets/{id}/reconciliation", class: "internal", method: http.MethodPost, ok: http.StatusOK,
			path: func(f *fixture, _ principal) string { return "/wallets/" + f.wallet + "/reconciliation" }},
		{name: "POST /wagering/transactions", class: "provider", method: http.MethodPost, key: true, ok: http.StatusCreated,
			path: func(*fixture, principal) string { return "/wagering/transactions" },
			body: func(f *fixture, p principal) any {
				provider := p.provider
				if provider == "" {
					provider = "provider-a" // a well-formed request for a caller that may not make it
				}
				return idptest.Operation(provider, w.next("matrix-ext"), f.wallet, f.player, "BET", "1.00")
			}},
		{name: "GET /wagering/transactions/{id}", class: "provider", method: http.MethodGet, ok: http.StatusOK,
			path: func(f *fixture, p principal) string { return "/wagering/transactions/" + f.ownSeed(p).id }},
		{name: "GET /providers/{p}/wagering/transactions/{ext}", class: "provider", method: http.MethodGet, ok: http.StatusOK,
			path: func(f *fixture, p principal) string {
				s := f.ownSeed(p)
				provider := p.provider
				if provider == "" {
					provider = "provider-a"
				}
				return "/providers/" + provider + "/wagering/transactions/" + s.external
			}},
	}
}

// ownSeed is the principal's own transaction, or provider-a's for principals
// that have none (they must be refused before it matters).
func (f *fixture) ownSeed(p principal) seeded {
	if s, ok := f.seed[p.provider]; ok {
		return s
	}
	return f.seed["provider-a"]
}

// TestEndpointAccessMatrix drives every business route with every kind of
// principal (REQ-059, REQ-060, REQ-061). Each refusal must leave PostgreSQL and
// the wallet untouched.
func TestEndpointAccessMatrix(t *testing.T) {
	w := newWorld(t)
	f := newFixture(t, w)

	// A real token that lives 3s; by the time the matrix runs it is expired.
	expired, expiresAt := idptest.ShortLivedToken(t, w.Keycloak, 3*time.Second)
	noClaims := idptest.Token(t, w.Keycloak, idptest.NoClaims)
	master := idptest.MasterToken(t, w.Keycloak)
	time.Sleep(time.Until(expiresAt) + 1500*time.Millisecond)

	principals := []principal{
		{name: "no credentials", class: "anonymous"},
		{name: "empty bearer", raw: "Bearer ", class: "rejected"},
		{name: "garbage bearer", token: "garbage", class: "rejected"},
		{name: "basic scheme", raw: "Basic dXNlcjpwYXNz", class: "rejected"},
		{name: "expired IdP token", token: expired, class: "rejected"},
		{name: "other realm IdP token", token: master, class: "rejected"},
		{name: "IdP token without audience", token: noClaims, class: "rejected"},
		{name: "provider-a", token: w.provA, provider: "provider-a", class: "provider"},
		{name: "provider-b", token: w.provB, provider: "provider-b", class: "provider"},
		{name: "internal service", token: w.internal, class: "internal"},
	}

	for _, ep := range endpoints(w) {
		for _, p := range principals {
			t.Run(ep.name+"/"+p.name, func(t *testing.T) {
				want := http.StatusUnauthorized
				switch {
				case p.class == "provider" && ep.class == "provider", p.class == "internal" && ep.class == "internal":
					want = ep.ok
				case p.class == "provider" || p.class == "internal":
					want = http.StatusForbidden
				}
				before := w.Counts()
				minor, version := w.Balance(f.wallet)

				req := idptest.Request{Method: ep.method, Path: ep.path(f, p), Token: p.token, RawAuthorization: p.raw}
				if ep.body != nil {
					req.Body = ep.body(f, p)
				}
				if ep.key {
					req.Headers = map[string]string{"Idempotency-Key": w.next("matrix-key")}
				}
				r := w.Do(req)
				if r.Status != want {
					t.Fatalf("status %d, want %d; body %s", r.Status, want, r.Raw)
				}
				switch want {
				case http.StatusUnauthorized:
					if r.Code() != "UNAUTHENTICATED" || r.Header.Get("WWW-Authenticate") == "" {
						t.Errorf("401 without UNAUTHENTICATED envelope/challenge: %q %v", r.Raw, r.Header)
					}
				case http.StatusForbidden:
					if r.Code() != "FORBIDDEN" {
						t.Errorf("403 code = %q", r.Code())
					}
				}
				if want != ep.ok {
					assertUnchanged(t, w, p.name+" on "+ep.name, before, f.wallet, minor, version)
					mustNotContain(t, "refusal", r.Raw, f.wallet, f.seed["provider-a"].id, f.seed["provider-b"].id, p.token)
				}
			})
		}
	}
}

// TestNoLocalTokenIssuer: the service only validates tokens; it offers no
// password, login or token endpoint of its own (REQ-059).
func TestNoLocalTokenIssuer(t *testing.T) {
	w := newWorld(t)
	for _, path := range []string{"/token", "/oauth/token", "/auth/login", "/login", "/v1/token", "/.well-known/openid-configuration", "/protocol/openid-connect/token"} {
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			r := w.Do(idptest.Request{Method: method, Path: path, Body: map[string]string{"grant_type": "client_credentials", "client_id": "provider-a", "client_secret": "provider-a-secret"}})
			if r.Status != http.StatusNotFound && r.Status != http.StatusMethodNotAllowed {
				t.Errorf("%s %s = %d %s, want 404/405", method, path, r.Status, r.Raw)
			}
			mustNotContain(t, method+" "+path, r.Raw, "access_token")
		}
	}
}
