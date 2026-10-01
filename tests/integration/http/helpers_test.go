// Package httpauth_test exercises the HTTP API with real Keycloak tokens, a
// real PostgreSQL database and the real Fx-composed service (REQ-047, REQ-052
// to REQ-054, REQ-059 to REQ-061, REQ-078).
package httpauth_test

import (
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"wagering/tests/integration/auth/idptest"
)

// world is a started service plus one token per real principal.
type world struct {
	*idptest.App
	internal, provA, provB string
	seq                    atomic.Int64
}

func newWorld(t *testing.T) *world {
	t.Helper()
	app := idptest.StartApp(t)
	return &world{
		App:      app,
		internal: idptest.Token(t, app.Keycloak, idptest.Internal),
		provA:    idptest.Token(t, app.Keycloak, idptest.ProviderA),
		provB:    idptest.Token(t, app.Keycloak, idptest.ProviderB),
	}
}

// next returns a unique suffix so tests never collide on identifiers.
func (w *world) next(prefix string) string { return fmt.Sprintf("%s-%d", prefix, w.seq.Add(1)) }

func (w *world) wallet(t *testing.T, amount string) string {
	t.Helper()
	return w.OpenWallet(w.internal, w.next("player"), amount)
}

// bet submits a BET as the given provider and expects it to be created.
func (w *world) bet(t *testing.T, token, provider, wallet, player, external, key, amount string) idptest.Response {
	t.Helper()
	r := w.Submit(token, key, idptest.Operation(provider, external, wallet, player, "BET", amount))
	if r.Status != http.StatusCreated {
		t.Fatalf("BET %s/%s: status %d body %s", provider, external, r.Status, r.Raw)
	}
	return r
}

// player reads the player of a wallet through the internal API.
func (w *world) player(t *testing.T, wallet string) string {
	t.Helper()
	r := w.Do(idptest.Request{Method: http.MethodGet, Path: "/wallets/" + wallet, Token: w.internal})
	if r.Status != http.StatusOK {
		t.Fatalf("read wallet: %d %s", r.Status, r.Raw)
	}
	return r.JSON(t)["playerId"].(string)
}

func amountOf(t *testing.T, obj map[string]any, field string) string {
	t.Helper()
	m, ok := obj[field].(map[string]any)
	if !ok {
		t.Fatalf("field %q missing in %v", field, obj)
	}
	return m["amount"].(string)
}

// assertUnchanged fails when any financial table or the wallet moved.
func assertUnchanged(t *testing.T, w *world, what string, before idptest.Counts, wallet string, minor, version int64) {
	t.Helper()
	if after := w.Counts(); after != before {
		t.Errorf("%s changed the database: before {%s} after {%s}", what, before, after)
	}
	if wallet != "" {
		if m, v := w.Balance(wallet); m != minor || v != version {
			t.Errorf("%s moved the wallet: balance %d->%d version %d->%d", what, minor, m, version, v)
		}
	}
}

func mustNotContain(t *testing.T, what string, body []byte, secrets ...string) {
	t.Helper()
	for _, s := range secrets {
		if s != "" && strings.Contains(string(body), s) {
			t.Errorf("%s leaks %q: %s", what, s, body)
		}
	}
}
