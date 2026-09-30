package reference

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"wagering/internal/domain"
	wagering "wagering/internal/usecase/wagering"
)

type fakeClaimer struct {
	mu      sync.Mutex
	batches [][]domain.UUID
	err     error
	calls   int
	owner   string
	lease   time.Duration
}

func (f *fakeClaimer) ClaimTransactions(_ context.Context, owner string, _ int, lease time.Duration) ([]domain.UUID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.owner, f.lease = owner, lease
	if f.err != nil {
		return nil, f.err
	}
	if len(f.batches) == 0 {
		return nil, nil
	}
	b := f.batches[0]
	f.batches = f.batches[1:]
	return b, nil
}

type fakeRetrier func(ctx context.Context, id domain.UUID) (wagering.Result, error)

func (f fakeRetrier) RetryPending(ctx context.Context, id domain.UUID) (wagering.Result, error) {
	return f(ctx, id)
}

type recorder struct {
	mu       sync.Mutex
	claimed  []int
	attempts []Attempt
}

func (r *recorder) Claimed(n int) { r.mu.Lock(); r.claimed = append(r.claimed, n); r.mu.Unlock() }
func (r *recorder) Attempted(a Attempt) {
	r.mu.Lock()
	r.attempts = append(r.attempts, a)
	r.mu.Unlock()
}

func testConfig() Config {
	c := DefaultConfig()
	c.Owner = "test-owner"
	c.BatchSize = 3
	c.Concurrency = 2
	c.Lease = 2 * time.Second
	c.ItemTimeout = time.Second
	c.PollInterval = 5 * time.Millisecond
	return c
}

func ids(n int) []domain.UUID {
	out := make([]domain.UUID, n)
	for i := range out {
		out[i] = domain.NewUUID()
	}
	return out
}

func TestConfigValidation(t *testing.T) {
	if err := DefaultConfig().Validate(); err != nil {
		t.Fatalf("default config: %v", err)
	}
	bad := map[string]func(*Config){
		"batch":       func(c *Config) { c.BatchSize = 0 },
		"concurrency": func(c *Config) { c.Concurrency = 0 },
		"lease":       func(c *Config) { c.Lease = 0 },
		"timeout>=lease": func(c *Config) {
			c.ItemTimeout = c.Lease
		},
		"poll":  func(c *Config) { c.PollInterval = 0 },
		"retry": func(c *Config) { c.Retry.TTL = 0 },
	}
	for name, mutate := range bad {
		c := DefaultConfig()
		mutate(&c)
		if c.Validate() == nil {
			t.Errorf("%s: expected validation error", name)
		}
	}
}

func TestLoadConfigFromEnvironment(t *testing.T) {
	t.Setenv("REFERENCE_RETRY_TTL", "2h")
	t.Setenv("REFERENCE_RETRY_MAX_ATTEMPTS", "7")
	t.Setenv("REFERENCE_WORKER_LEASE", "90s")
	c, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if c.Retry.TTL != 2*time.Hour || c.Retry.MaxAttempts != 7 || c.Lease != 90*time.Second {
		t.Fatalf("unexpected config %+v", c)
	}
	t.Setenv("REFERENCE_WORKER_LEASE", "soon")
	if _, err = LoadConfig(); err == nil {
		t.Fatal("expected parse error")
	}
	t.Setenv("REFERENCE_WORKER_LEASE", "10s")
	t.Setenv("REFERENCE_WORKER_ITEM_TIMEOUT", "10s")
	if _, err = LoadConfig(); err == nil {
		t.Fatal("expected item timeout >= lease to be rejected")
	}
}

func TestRunOnceErrorOnOneRowDoesNotStopBatch(t *testing.T) {
	batch := ids(3)
	claimer := &fakeClaimer{batches: [][]domain.UUID{batch}}
	rec := &recorder{}
	var seen atomic.Int32
	w, err := NewWorker(testConfig(), claimer, fakeRetrier(func(_ context.Context, id domain.UUID) (wagering.Result, error) {
		seen.Add(1)
		if id == batch[1] {
			return wagering.Result{}, errors.New("database unavailable")
		}
		return wagering.Result{Replay: true}, nil
	}), rec, nil)
	if err != nil {
		t.Fatal(err)
	}
	st, err := w.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.Claimed != 3 || st.Stale != 2 || st.Errors != 1 || seen.Load() != 3 {
		t.Fatalf("stats %+v seen %d", st, seen.Load())
	}
	if claimer.owner != "test-owner" || claimer.lease != 2*time.Second {
		t.Fatalf("claim used owner %q lease %v", claimer.owner, claimer.lease)
	}
	if len(rec.attempts) != 3 || len(rec.claimed) != 1 || rec.claimed[0] != 3 {
		t.Fatalf("observer saw %+v", rec)
	}
}

func TestRunOnceClaimErrorIsReturned(t *testing.T) {
	boom := errors.New("pool closed")
	w, _ := NewWorker(testConfig(), &fakeClaimer{err: boom}, fakeRetrier(nil), nil, nil)
	if _, err := w.RunOnce(context.Background()); !errors.Is(err, boom) {
		t.Fatalf("got %v", err)
	}
}

func TestItemTimeoutIsAppliedToEachRow(t *testing.T) {
	cfg := testConfig()
	cfg.ItemTimeout = 30 * time.Millisecond
	w, _ := NewWorker(cfg, &fakeClaimer{batches: [][]domain.UUID{ids(1)}}, fakeRetrier(func(ctx context.Context, _ domain.UUID) (wagering.Result, error) {
		<-ctx.Done()
		return wagering.Result{}, ctx.Err()
	}), nil, nil)
	st, err := w.RunOnce(context.Background())
	if err != nil || st.Errors != 1 {
		t.Fatalf("stats %+v err %v", st, err)
	}
}

func TestRunDrainsFullBatchesAndStopsOnCancel(t *testing.T) {
	claimer := &fakeClaimer{batches: [][]domain.UUID{ids(3), ids(3), ids(1)}}
	var done atomic.Int32
	w, _ := NewWorker(testConfig(), claimer, fakeRetrier(func(context.Context, domain.UUID) (wagering.Result, error) {
		done.Add(1)
		return wagering.Result{Replay: true}, nil
	}), nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan error, 1)
	go func() { finished <- w.Run(ctx) }()
	deadline := time.After(2 * time.Second)
	for done.Load() < 7 {
		select {
		case <-deadline:
			t.Fatalf("only %d rows processed", done.Load())
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}
}

func TestStopWaitsForInFlightRowThenReleases(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	w, _ := NewWorker(testConfig(), &fakeClaimer{batches: [][]domain.UUID{ids(1)}}, fakeRetrier(func(ctx context.Context, _ domain.UUID) (wagering.Result, error) {
		close(started)
		select {
		case <-release:
			return wagering.Result{Replay: true}, nil
		case <-ctx.Done():
			return wagering.Result{}, ctx.Err()
		}
	}), nil, nil)
	w.Start()
	w.Start() // idempotent
	<-started
	stopped := make(chan error, 1)
	go func() { stopped <- w.Stop(context.Background()) }()
	select {
	case <-stopped:
		t.Fatal("Stop returned while a row was still in flight")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	if err := <-stopped; err != nil {
		t.Fatalf("graceful stop: %v", err)
	}
	if err := w.Stop(context.Background()); err != nil {
		t.Fatalf("second Stop: %v", err)
	}
}

func TestStopDeadlineCancelsInFlightRow(t *testing.T) {
	started := make(chan struct{})
	cancelled := make(chan struct{})
	w, _ := NewWorker(testConfig(), &fakeClaimer{batches: [][]domain.UUID{ids(1)}}, fakeRetrier(func(ctx context.Context, _ domain.UUID) (wagering.Result, error) {
		close(started)
		<-ctx.Done()
		close(cancelled)
		return wagering.Result{}, ctx.Err()
	}), nil, nil)
	w.Start()
	<-started
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := w.Stop(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline error, got %v", err)
	}
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("in-flight row was not cancelled")
	}
}
