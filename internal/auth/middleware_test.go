package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/fx"

	"wagering/internal/config"
	"wagering/internal/contract"
)

type harness struct {
	p       *idp
	k       rsaKey
	mw      *Middleware
	effects atomic.Int32 // counts executions of the protected handler
	logs    bytes.Buffer
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	p, k, v := setup(t)
	h := &harness{p: p, k: k, mw: NewMiddleware(v)}
	h.mw.log = slog.New(slog.NewTextHandler(&h.logs, nil))
	return h
}

func (h *harness) token(t *testing.T, o map[string]any) string {
	return h.k.sign(t, hdr("RS256", testKid), claimsFor(h.p, o))
}

func (h *harness) providerToken(t *testing.T, providerID string) string {
	return h.token(t, map[string]any{"provider_id": providerID})
}

func (h *harness) internalToken(t *testing.T) string {
	return h.token(t, map[string]any{"scope": ScopeInternal, "provider_id": nil, "sub": "internal-service"})
}

func (h *harness) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.effects.Add(1)
		id, _ := FromContext(r.Context())
		_ = json.NewEncoder(w).Encode(map[string]string{"sub": id.Subject, "provider": id.ProviderID})
	})
}

func do(handler http.Handler, authz ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/x", nil)
	for _, a := range authz {
		req.Header.Add("Authorization", a)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func errBody(t *testing.T, rec *httptest.ResponseRecorder) contract.ErrorBody {
	t.Helper()
	var b contract.ErrorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &b); err != nil {
		t.Fatalf("body %q: %v", rec.Body.String(), err)
	}
	return b
}

func TestAuthenticationFailuresAre401AndHaveNoEffects(t *testing.T) {
	h := newHarness(t)
	expired := h.token(t, map[string]any{"exp": testNow.Add(-time.Minute).Unix()})
	cases := map[string][]string{
		"absent":              nil,
		"empty bearer":        {"Bearer "},
		"basic scheme":        {"Basic dXNlcjpwYXNz"},
		"no scheme":           {h.providerToken(t, "provider-a")},
		"garbage":             {"Bearer garbage"},
		"bad signature":       {"Bearer " + h.providerToken(t, "provider-a")[:len(h.providerToken(t, "provider-a"))-4] + "AAAA"},
		"expired":             {"Bearer " + expired},
		"wrong audience":      {"Bearer " + h.token(t, map[string]any{"aud": "other"})},
		"two headers":         {"Bearer " + h.providerToken(t, "provider-a"), "Bearer " + h.providerToken(t, "provider-a")},
		"token with spaces":   {"Bearer a b"},
		"alg none":            {"Bearer " + encode(t, hdr("none", testKid)) + "." + encode(t, claimsFor(h.p, nil)) + "."},
		"forged other issuer": {"Bearer " + h.token(t, map[string]any{"iss": "https://evil"})},
	}
	for name, authz := range cases {
		for route, handler := range map[string]http.Handler{
			"authenticate": h.mw.Authenticate(h.handler()),
			"provider":     h.mw.Provider(h.handler()),
			"internal":     h.mw.Internal(h.handler()),
		} {
			t.Run(name+"/"+route, func(t *testing.T) {
				rec := do(handler, authz...)
				if rec.Code != http.StatusUnauthorized {
					t.Fatalf("status = %d body=%s", rec.Code, rec.Body)
				}
				if !strings.HasPrefix(rec.Header().Get("WWW-Authenticate"), "Bearer") {
					t.Fatalf("missing challenge: %v", rec.Header())
				}
				if b := errBody(t, rec); b.Code != CodeUnauthenticated {
					t.Fatalf("code = %s", b.Code)
				}
			})
		}
	}
	if h.effects.Load() != 0 {
		t.Fatalf("protected handler ran %d times for rejected requests", h.effects.Load())
	}
}

func TestExpiredChallengeAndNoReasonLeak(t *testing.T) {
	h := newHarness(t)
	tok := h.token(t, map[string]any{"exp": testNow.Add(-time.Minute).Unix()})
	rec := do(h.mw.Authenticate(h.handler()), "Bearer "+tok)
	if !strings.Contains(rec.Header().Get("WWW-Authenticate"), `error="invalid_token"`) {
		t.Fatalf("challenge = %q", rec.Header().Get("WWW-Authenticate"))
	}
	if strings.Contains(rec.Body.String(), "expired") || strings.Contains(rec.Body.String(), tok) {
		t.Fatalf("response leaks detail: %s", rec.Body)
	}
	if !strings.Contains(h.logs.String(), "expired") {
		t.Fatalf("reason should be logged: %s", h.logs.String())
	}
	if strings.Contains(h.logs.String(), tok) {
		t.Fatal("token must not be logged")
	}
}

func TestBearerSchemeIsCaseInsensitive(t *testing.T) {
	h := newHarness(t)
	rec := do(h.mw.Provider(h.handler()), "bearer "+h.providerToken(t, "provider-a"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestProviderEndpointsAuthorization(t *testing.T) {
	h := newHarness(t)
	route := h.mw.Provider(h.handler())

	rec := do(route, "Bearer "+h.providerToken(t, "provider-a"))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"provider":"provider-a"`) {
		t.Fatalf("provider: %d %s", rec.Code, rec.Body)
	}
	// Internal principals do not get business access by virtue of being internal.
	if rec := do(route, "Bearer "+h.internalToken(t)); rec.Code != http.StatusForbidden {
		t.Fatalf("internal on provider route = %d", rec.Code)
	}
	// Scope without provider claim, and provider claim without scope: forbidden.
	noClaim := h.token(t, map[string]any{"provider_id": nil})
	noScope := h.token(t, map[string]any{"scope": "openid"})
	for name, tok := range map[string]string{"no claim": noClaim, "no scope": noScope} {
		if rec := do(route, "Bearer "+tok); rec.Code != http.StatusForbidden {
			t.Fatalf("%s = %d", name, rec.Code)
		}
	}
	if h.effects.Load() != 1 {
		t.Fatalf("effects = %d, want 1", h.effects.Load())
	}
}

func TestInternalEndpointsForbiddenToProviders(t *testing.T) {
	h := newHarness(t)
	route := h.mw.Internal(h.handler())
	rec := do(route, "Bearer "+h.providerToken(t, "provider-a"))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d", rec.Code)
	}
	if b := errBody(t, rec); b.Code != CodeForbidden {
		t.Fatalf("code = %s", b.Code)
	}
	// A provider cannot self-grant internal access by claiming it in the
	// provider claim or an unrelated claim.
	spoof := h.token(t, map[string]any{"provider_id": ScopeInternal, "scope": "openid " + ScopeProvider, "internal": true})
	if rec := do(route, "Bearer "+spoof); rec.Code != http.StatusForbidden {
		t.Fatalf("spoof status = %d", rec.Code)
	}
	if h.effects.Load() != 0 {
		t.Fatal("handler ran for forbidden request")
	}
	if rec := do(route, "Bearer "+h.internalToken(t)); rec.Code != http.StatusOK {
		t.Fatalf("internal status = %d", rec.Code)
	}
}

func TestRequireWithoutAuthenticateFailsClosed(t *testing.T) {
	h := newHarness(t)
	if rec := do(h.mw.RequireInternal(h.handler())); rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d", rec.Code)
	}
	if h.effects.Load() != 0 {
		t.Fatal("handler ran")
	}
}

func TestIdPUnavailableIs503(t *testing.T) {
	h := newHarness(t)
	tok := h.providerToken(t, "provider-a")
	// New verifier with a cold cache against a dead IdP.
	h.p.down.Store(true)
	v := h.p.verifier(t)
	mw := NewMiddleware(v)
	rec := do(mw.Provider(h.handler()), "Bearer "+tok)
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("status = %d headers=%v", rec.Code, rec.Header())
	}
	if h.effects.Load() != 0 {
		t.Fatal("handler ran")
	}
}

func TestCancelledRequestDoesNotReachHandler(t *testing.T) {
	h := newHarness(t)
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Authorization", "Bearer "+h.providerToken(t, "provider-a"))
	ctx, cancel := context.WithCancel(req.Context())
	cancel()
	rec := httptest.NewRecorder()
	h.mw.Provider(h.handler()).ServeHTTP(rec, req.WithContext(ctx))
	if rec.Code != http.StatusServiceUnavailable || h.effects.Load() != 0 {
		t.Fatalf("status = %d effects=%d", rec.Code, h.effects.Load())
	}
}

func TestCorrelationIDInErrorBody(t *testing.T) {
	h := newHarness(t)
	h.mw.CorrelationID = func(r *http.Request) string { return "corr-1" }
	if b := errBody(t, do(h.mw.Provider(h.handler()))); b.CorrelationID != "corr-1" {
		t.Fatalf("correlation = %q", b.CorrelationID)
	}
}

func TestAuthorizeProviderIsolation(t *testing.T) {
	a := WithIdentity(context.Background(), NewIdentity("s", "provider-a", ScopeProvider))
	internal := WithIdentity(context.Background(), NewIdentity("s", "", ScopeInternal))
	noScope := WithIdentity(context.Background(), NewIdentity("s", "provider-a"))

	if err := AuthorizeProvider(a, "provider-a"); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		ctx      context.Context
		provider string
		want     error
	}{
		"other provider":    {a, "provider-b", ErrProviderMismatch},
		"empty provider":    {a, "", ErrProviderMismatch},
		"case differs":      {a, "Provider-A", ErrProviderMismatch},
		"internal identity": {internal, "provider-a", ErrForbidden},
		"no scope":          {noScope, "provider-a", ErrForbidden},
		"anonymous":         {context.Background(), "provider-a", ErrForbidden},
	} {
		if err := AuthorizeProvider(tc.ctx, tc.provider); err != tc.want {
			t.Errorf("%s: err = %v, want %v", name, err, tc.want)
		}
	}
	if got, err := ProviderScope(a); err != nil || got != "provider-a" {
		t.Fatalf("scope = %q %v", got, err)
	}
	if _, err := ProviderScope(internal); err != ErrForbidden {
		t.Fatalf("internal scope err = %v", err)
	}
}

func TestWriteErrorMapsProviderMismatchTo403(t *testing.T) {
	h := newHarness(t)
	rec := httptest.NewRecorder()
	if !h.mw.WriteError(rec, httptest.NewRequest(http.MethodGet, "/x", nil), ErrProviderMismatch) || rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d", rec.Code)
	}
	if h.mw.WriteError(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x", nil), context.DeadlineExceeded) {
		// context errors are not auth errors; the caller handles them
		t.Fatal("non-auth error must not be handled")
	}
}

func TestEndToEndProviderBinding(t *testing.T) {
	// A handler doing what T08 is expected to do: bind the body's providerId
	// to the token before any side effect.
	h := newHarness(t)
	var stored atomic.Int32
	handler := h.mw.Provider(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := AuthorizeProvider(r.Context(), r.URL.Query().Get("providerId")); err != nil {
			h.mw.WriteError(w, r, err)
			return
		}
		stored.Add(1)
	}))
	get := func(tok, provider string) int {
		req := httptest.NewRequest(http.MethodPost, "/x?providerId="+provider, nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec.Code
	}
	if c := get(h.providerToken(t, "provider-a"), "provider-a"); c != 200 {
		t.Fatalf("own = %d", c)
	}
	if c := get(h.providerToken(t, "provider-a"), "provider-b"); c != 403 {
		t.Fatalf("cross = %d", c)
	}
	if stored.Load() != 1 {
		t.Fatalf("effects = %d", stored.Load())
	}
}

func TestFxModuleWiring(t *testing.T) {
	p := newIDP(t)
	var mw *Middleware
	var v *Verifier
	app := fx.New(
		fx.NopLogger,
		fx.Supply(config.Config{OIDCIssuerURL: p.issuer(), OIDCAudience: testAudience}),
		Module,
		fx.Populate(&mw, &v),
	)
	if err := app.Err(); err != nil {
		t.Fatal(err)
	}
	if mw == nil || v == nil {
		t.Fatal("module did not provide middleware and verifier")
	}
	if rec := do(mw.Provider(http.NotFoundHandler())); rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d", rec.Code)
	}
}
