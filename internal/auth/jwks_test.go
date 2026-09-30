package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func TestDiscoveryAndCaching(t *testing.T) {
	p, k, v := setup(t)
	tok := k.sign(t, hdr("RS256", testKid), claimsFor(p, nil))
	for i := 0; i < 5; i++ {
		if _, err := v.Verify(context.Background(), tok); err != nil {
			t.Fatal(err)
		}
	}
	if p.discHit.Load() != 1 || p.jwksHit.Load() != 1 {
		t.Fatalf("discovery=%d jwks=%d, want 1/1", p.discHit.Load(), p.jwksHit.Load())
	}
}

func TestExplicitJWKSURLSkipsDiscovery(t *testing.T) {
	p, k, _ := setup(t)
	v := p.verifier(t, func(o *Options) { o.JWKSURL = p.server.URL + "/jwks" })
	if _, err := v.Verify(context.Background(), k.sign(t, hdr("RS256", testKid), claimsFor(p, nil))); err != nil {
		t.Fatal(err)
	}
	if p.discHit.Load() != 0 {
		t.Fatal("discovery must not run when JWKSURL is set")
	}
}

func TestDiscoveryIssuerMismatchFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"issuer":"https://other","jwks_uri":"http://127.0.0.1:1/jwks"}`))
	}))
	defer srv.Close()
	v, _ := New(Options{Issuer: srv.URL, Audience: testAudience})
	k := newRSAKey(t, testKid, 2048)
	tok := k.sign(t, hdr("RS256", testKid), map[string]any{"iss": srv.URL})
	if _, err := v.Verify(context.Background(), tok); !errors.Is(err, ErrKeysUnavailable) {
		t.Fatalf("err = %v", err)
	}
}

func TestKeyRotationRefreshesOnUnknownKidWithCooldown(t *testing.T) {
	clock := &fakeClock{t: testNow}
	p := newIDP(t)
	k1 := newRSAKey(t, "k1", 2048)
	k2 := newRSAKey(t, "k2", 2048)
	p.publish(k1.jwk())
	v := p.verifier(t, func(o *Options) { o.Now = clock.now })
	ctx := context.Background()

	if _, err := v.Verify(ctx, k1.sign(t, hdr("RS256", "k1"), claimsFor(p, map[string]any{"exp": testNow.Add(time.Hour).Unix()}))); err != nil {
		t.Fatal(err)
	}
	p.publish(k1.jwk(), k2.jwk()) // rotation
	tok2 := k2.sign(t, hdr("RS256", "k2"), claimsFor(p, map[string]any{"exp": testNow.Add(time.Hour).Unix()}))

	// Inside the cooldown an unknown kid does not trigger a fetch.
	if _, err := v.Verify(ctx, tok2); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("within cooldown err = %v", err)
	}
	if p.jwksHit.Load() != 1 {
		t.Fatalf("jwks hits = %d, want 1", p.jwksHit.Load())
	}
	clock.advance(11 * time.Second)
	if _, err := v.Verify(ctx, tok2); err != nil {
		t.Fatalf("after cooldown: %v", err)
	}
	if p.jwksHit.Load() != 2 {
		t.Fatalf("jwks hits = %d, want 2", p.jwksHit.Load())
	}
}

func TestRandomKidsDoNotAmplifyFetches(t *testing.T) {
	p, k, v := setup(t)
	if _, err := v.Verify(context.Background(), k.sign(t, hdr("RS256", testKid), claimsFor(p, nil))); err != nil {
		t.Fatal(err)
	}
	before := p.jwksHit.Load()
	for i := 0; i < 50; i++ {
		_, _ = v.Verify(context.Background(), k.sign(t, hdr("RS256", "rand"+string(rune('a'+i%26))), claimsFor(p, nil)))
	}
	if got := p.jwksHit.Load() - before; got > 1 {
		t.Fatalf("%d extra fetches for unknown kids", got)
	}
}

func TestRevokedKeyDroppedAfterTTL(t *testing.T) {
	clock := &fakeClock{t: testNow}
	p := newIDP(t)
	k1 := newRSAKey(t, "k1", 2048)
	k2 := newRSAKey(t, "k2", 2048)
	p.publish(k1.jwk())
	v := p.verifier(t, func(o *Options) { o.Now = clock.now; o.KeyTTL = time.Minute })
	claims := claimsFor(p, map[string]any{"exp": testNow.Add(time.Hour).Unix()})
	tok := k1.sign(t, hdr("RS256", "k1"), claims)
	if _, err := v.Verify(context.Background(), tok); err != nil {
		t.Fatal(err)
	}
	p.publish(k2.jwk()) // k1 removed
	clock.advance(2 * time.Minute)
	if _, err := v.Verify(context.Background(), tok); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("removed key still accepted: %v", err)
	}
}

func TestIdPDownFailsClosedButStaleKeysKeepWorking(t *testing.T) {
	clock := &fakeClock{t: testNow}
	p := newIDP(t)
	k := newRSAKey(t, testKid, 2048)
	p.publish(k.jwk())
	v := p.verifier(t, func(o *Options) { o.Now = clock.now; o.KeyTTL = time.Minute })
	tok := k.sign(t, hdr("RS256", testKid), claimsFor(p, map[string]any{"exp": testNow.Add(time.Hour).Unix()}))

	// Cold cache with IdP down: fail closed with 503-class error.
	p.down.Store(true)
	if _, err := v.Verify(context.Background(), tok); !errors.Is(err, ErrKeysUnavailable) {
		t.Fatalf("cold err = %v", err)
	}
	p.down.Store(false)
	clock.advance(time.Second)
	if _, err := v.Verify(context.Background(), tok); err != nil {
		t.Fatal(err)
	}
	// Stale cache + IdP down: known key still verifies.
	p.down.Store(true)
	clock.advance(5 * time.Minute)
	if _, err := v.Verify(context.Background(), tok); err != nil {
		t.Fatalf("stale err = %v", err)
	}
	// ...but an unknown kid reports unavailability rather than "invalid".
	other := newRSAKey(t, "zzz", 2048)
	clock.advance(time.Minute)
	if _, err := v.Verify(context.Background(), other.sign(t, hdr("RS256", "zzz"), claimsFor(p, nil))); !errors.Is(err, ErrKeysUnavailable) {
		t.Fatalf("unknown err = %v", err)
	}
}

func TestWeakAndUnsupportedKeysIgnored(t *testing.T) {
	p := newIDP(t)
	weak := newRSAKey(t, "weak", 1024)
	good := newRSAKey(t, "good", 2048)
	sigUseEnc := good.jwk()
	sigUseEnc.Kid, sigUseEnc.Use = "enc", "enc"
	p.publish(weak.jwk(), sigUseEnc, jwk{Kty: "oct", Kid: "sym", N: "AA"}, good.jwk())
	v := p.verifier(t)
	claims := claimsFor(p, nil)
	for _, kid := range []string{"weak", "enc", "sym"} {
		_, err := v.Verify(context.Background(), weak.sign(t, hdr("RS256", kid), claims))
		if !errors.Is(err, ErrInvalidToken) {
			t.Errorf("kid %s: err = %v", kid, err)
		}
	}
	if _, err := v.Verify(context.Background(), good.sign(t, hdr("RS256", "good"), claims)); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyRespectsContext(t *testing.T) {
	p := newIDP(t)
	release := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer slow.Close()
	defer close(release)
	k := newRSAKey(t, testKid, 2048)
	v, _ := New(Options{Issuer: p.issuer(), Audience: testAudience, JWKSURL: slow.URL, Now: func() time.Time { return testNow }})
	tok := k.sign(t, hdr("RS256", testKid), claimsFor(p, nil))

	already, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := v.Verify(already, tok); !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-cancelled err = %v", err)
	}

	ctx, cancel2 := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel2()
	start := time.Now()
	if _, err := v.Verify(ctx, tok); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("verify ignored the caller deadline")
	}
}

func TestConcurrentVerifyShareOneFetch(t *testing.T) {
	var hits atomic.Int32
	p := newIDP(t)
	k := newRSAKey(t, testKid, 2048)
	p.publish(k.jwk())
	gate := make(chan struct{})
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		<-gate
		p.server.Config.Handler.ServeHTTP(w, r)
	}))
	defer proxy.Close()
	v, _ := New(Options{Issuer: p.issuer(), Audience: testAudience, JWKSURL: proxy.URL + "/jwks", Now: func() time.Time { return testNow }})
	tok := k.sign(t, hdr("RS256", testKid), claimsFor(p, nil))

	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := v.Verify(context.Background(), tok)
			errs <- err
		}()
	}
	time.Sleep(100 * time.Millisecond)
	close(gate)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if hits.Load() != 1 {
		t.Fatalf("fetches = %d, want 1", hits.Load())
	}
}
