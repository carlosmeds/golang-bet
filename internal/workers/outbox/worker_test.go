package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"wagering/internal/domain"
	"wagering/internal/storage/pg"
)

type testData struct {
	N int `json:"n"`
}

// testEvent builds a claimed row whose payload is a valid typed envelope.
func testEvent(t testing.TB, aggregate domain.UUID, n int) pg.ClaimedEvent {
	t.Helper()
	env := domain.Envelope[testData]{
		EventMeta: domain.EventMeta{
			EventID:       domain.NewUUID(),
			EventType:     domain.EventWalletBalanceChanged,
			AggregateID:   aggregate,
			CorrelationID: "corr-1",
			OccurredAt:    time.Now().UTC().Add(-time.Second),
			Version:       domain.EventVersion,
		},
		Data: testData{N: n},
	}
	payload, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	return pg.ClaimedEvent{EventID: env.EventID, Payload: payload}
}

type retryCall struct {
	id     domain.UUID
	owner  string
	after  time.Duration
	reason string
}

// fakeStore serves pre-set events once and records acknowledgements.
type fakeStore struct {
	mu        sync.Mutex
	events    []pg.ClaimedEvent
	claimErr  error
	markErr   error
	retryErr  error
	owners    []string
	limits    []int
	leases    []time.Duration
	published []domain.UUID
	retries   []retryCall
}

func (f *fakeStore) ClaimOutbox(_ context.Context, owner string, limit int, lease time.Duration) ([]pg.ClaimedEvent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.owners, f.limits, f.leases = append(f.owners, owner), append(f.limits, limit), append(f.leases, lease)
	if f.claimErr != nil {
		return nil, f.claimErr
	}
	out := f.events
	f.events = nil
	for i := range out {
		out[i].LeaseOwner = owner
	}
	return out, nil
}

func (f *fakeStore) MarkPublished(_ context.Context, id domain.UUID, owner string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.markErr != nil {
		return f.markErr
	}
	f.published = append(f.published, id)
	return nil
}

func (f *fakeStore) RetryOutbox(_ context.Context, id domain.UUID, owner string, after time.Duration, reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.retryErr != nil {
		return f.retryErr
	}
	f.retries = append(f.retries, retryCall{id, owner, after, reason})
	return nil
}

// fakeSender records messages; fn (optional) decides the outcome.
type fakeSender struct {
	mu   sync.Mutex
	sent []Message
	fn   func(ctx context.Context, m Message) error
}

func (f *fakeSender) Send(ctx context.Context, m Message) error {
	f.mu.Lock()
	f.sent = append(f.sent, m)
	fn := f.fn
	f.mu.Unlock()
	if fn != nil {
		return fn(ctx, m)
	}
	return nil
}

func (f *fakeSender) messages() []Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Message(nil), f.sent...)
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
	c.BatchSize = 10
	c.Lease = 10 * time.Second
	c.SendTimeout = time.Second
	c.AckTimeout = time.Second
	c.PollInterval = 5 * time.Millisecond
	return c
}

func newTestPublisher(t *testing.T, cfg Config, st Store, snd Sender, obs Observer) *Publisher {
	t.Helper()
	p, err := NewPublisher(cfg, st, snd, obs, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestConfigValidation(t *testing.T) {
	if err := DefaultConfig().Validate(); err != nil {
		t.Fatalf("defaults must be valid: %v", err)
	}
	bad := map[string]func(*Config){
		"base delay":      func(c *Config) { c.Backoff.BaseDelay = 0 },
		"max below base":  func(c *Config) { c.Backoff.MaxDelay = c.Backoff.BaseDelay - 1 },
		"batch":           func(c *Config) { c.BatchSize = 0 },
		"concurrency":     func(c *Config) { c.Concurrency = 0 },
		"lease":           func(c *Config) { c.Lease = 0 },
		"send timeout":    func(c *Config) { c.SendTimeout = 0 },
		"ack timeout":     func(c *Config) { c.AckTimeout = 0 },
		"poll":            func(c *Config) { c.PollInterval = 0 },
		"timeouts>=lease": func(c *Config) { c.SendTimeout, c.AckTimeout, c.Lease = 5*time.Second, 5*time.Second, 10*time.Second },
	}
	for name, mutate := range bad {
		c := DefaultConfig()
		mutate(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s: expected a validation error", name)
		}
	}
	if _, err := NewPublisher(DefaultConfig(), nil, &fakeSender{}, nil, nil); err == nil {
		t.Error("a nil store must be rejected")
	}
	if _, err := NewPublisher(DefaultConfig(), &fakeStore{}, nil, nil, nil); err == nil {
		t.Error("a nil sender must be rejected")
	}
	p, err := NewPublisher(DefaultConfig(), &fakeStore{}, &fakeSender{}, nil, nil)
	if err != nil || p.Owner() == "" {
		t.Fatalf("an owner must be generated: %q %v", p.Owner(), err)
	}
}

func TestBackoffDoublesAndCaps(t *testing.T) {
	b := Backoff{BaseDelay: time.Second, MaxDelay: 5 * time.Minute}
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 32 * time.Second, 64 * time.Second, 128 * time.Second, 256 * time.Second, 5 * time.Minute, 5 * time.Minute}
	for i, w := range want {
		if got := b.Delay(i + 1); got != w {
			t.Errorf("attempt %d: %v, want %v", i+1, got, w)
		}
	}
	if got := b.Delay(10_000); got != 5*time.Minute {
		t.Errorf("a huge attempt count must stay capped, got %v", got)
	}
}

func TestLoadConfigFromEnvironment(t *testing.T) {
	t.Setenv("OUTBOX_BACKOFF_BASE_DELAY", "2s")
	t.Setenv("OUTBOX_BACKOFF_MAX_DELAY", "1m")
	t.Setenv("OUTBOX_BATCH_SIZE", "7")
	t.Setenv("OUTBOX_CONCURRENCY", "3")
	t.Setenv("OUTBOX_LEASE", "20s")
	t.Setenv("OUTBOX_SEND_TIMEOUT", "4s")
	t.Setenv("OUTBOX_ACK_TIMEOUT", "2s")
	t.Setenv("OUTBOX_POLL_INTERVAL", "250ms")
	c, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if c.Backoff.BaseDelay != 2*time.Second || c.Backoff.MaxDelay != time.Minute || c.BatchSize != 7 || c.Concurrency != 3 ||
		c.Lease != 20*time.Second || c.SendTimeout != 4*time.Second || c.AckTimeout != 2*time.Second || c.PollInterval != 250*time.Millisecond {
		t.Fatalf("unexpected config %+v", c)
	}
	t.Setenv("OUTBOX_BATCH_SIZE", "many")
	if _, err := LoadConfig(); err == nil {
		t.Error("a malformed number must fail")
	}
	t.Setenv("OUTBOX_BATCH_SIZE", "7")
	t.Setenv("OUTBOX_SEND_TIMEOUT", "30s")
	if _, err := LoadConfig(); err == nil {
		t.Error("a send timeout above the lease must fail validation")
	}
}

func TestPublishSendsStoredEnvelopeWithStableIdentity(t *testing.T) {
	agg := domain.NewUUID()
	e := testEvent(t, agg, 1)
	st := &fakeStore{events: []pg.ClaimedEvent{e}}
	snd, obs := &fakeSender{}, &recorder{}
	p := newTestPublisher(t, testConfig(), st, snd, obs)
	stats, err := p.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats.Claimed != 1 || stats.Published != 1 {
		t.Fatalf("stats %+v", stats)
	}
	got := snd.messages()
	if len(got) != 1 {
		t.Fatalf("sent %d messages", len(got))
	}
	m := got[0]
	if string(m.Body) != string(e.Payload) {
		t.Error("the stored snapshot must be sent unchanged")
	}
	if m.MessageGroupID != agg.String() || m.MessageDeduplicationID != e.EventID.String() || m.EventID != e.EventID || m.EventType != domain.EventWalletBalanceChanged {
		t.Errorf("unexpected message identity %+v", m)
	}
	if len(st.published) != 1 || st.published[0] != e.EventID || len(st.retries) != 0 {
		t.Errorf("acknowledgement: published %v retries %v", st.published, st.retries)
	}
	if st.owners[0] != "test-owner" || st.limits[0] != 10 || st.leases[0] != 10*time.Second {
		t.Errorf("claim parameters %v %v %v", st.owners, st.limits, st.leases)
	}
	if len(obs.claimed) != 1 || obs.claimed[0] != 1 || len(obs.attempts) != 1 {
		t.Fatalf("observer %+v", obs)
	}
	a := obs.attempts[0]
	if a.Result != ResultPublished || a.EventID != e.EventID || a.EventType != domain.EventWalletBalanceChanged || a.Lag < time.Second {
		t.Errorf("attempt %+v", a)
	}
}

func TestSendFailureSchedulesBackoffFromAttemptCount(t *testing.T) {
	e0, e3 := testEvent(t, domain.NewUUID(), 0), testEvent(t, domain.NewUUID(), 3)
	e3.AttemptCount = 3
	st := &fakeStore{events: []pg.ClaimedEvent{e0, e3}}
	boom := errors.New("queue unavailable")
	snd := &fakeSender{fn: func(context.Context, Message) error { return boom }}
	cfg := testConfig()
	cfg.Backoff = Backoff{BaseDelay: time.Second, MaxDelay: time.Minute}
	obs := &recorder{}
	p := newTestPublisher(t, cfg, st, snd, obs)
	stats, err := p.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats.Retried != 2 || stats.Published != 0 {
		t.Fatalf("stats %+v", stats)
	}
	delays := map[domain.UUID]time.Duration{}
	for _, r := range st.retries {
		delays[r.id] = r.after
		if r.owner != "test-owner" || r.reason != boom.Error() {
			t.Errorf("retry %+v", r)
		}
	}
	if delays[e0.EventID] != time.Second || delays[e3.EventID] != 8*time.Second {
		t.Errorf("delays %v: want 1s for a first failure and 8s after three earlier failures", delays)
	}
	for _, a := range obs.attempts {
		if a.Result != ResultRetry || !errors.Is(a.Err, boom) {
			t.Errorf("attempt %+v", a)
		}
	}
}

func TestRetryReasonIsBounded(t *testing.T) {
	st := &fakeStore{events: []pg.ClaimedEvent{testEvent(t, domain.NewUUID(), 1)}}
	long := errors.New(string(make([]byte, 5000)))
	snd := &fakeSender{fn: func(context.Context, Message) error { return long }}
	p := newTestPublisher(t, testConfig(), st, snd, nil)
	if _, err := p.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(st.retries) != 1 || len(st.retries[0].reason) != 500 {
		t.Fatalf("last_error must be truncated to 500 bytes: %+v", st.retries)
	}
}

func TestEventsOfOneAggregateAreSentInOrderAndStopAtFirstFailure(t *testing.T) {
	agg := domain.NewUUID()
	e1, e2, e3 := testEvent(t, agg, 1), testEvent(t, agg, 2), testEvent(t, agg, 3)
	st := &fakeStore{events: []pg.ClaimedEvent{e1, e2, e3}}
	var calls atomic.Int32
	snd := &fakeSender{fn: func(_ context.Context, m Message) error {
		if calls.Add(1) == 2 {
			return errors.New("broker error")
		}
		return nil
	}}
	obs := &recorder{}
	p := newTestPublisher(t, testConfig(), st, snd, obs)
	stats, err := p.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats.Published != 1 || stats.Retried != 1 || stats.Skipped != 1 {
		t.Fatalf("stats %+v", stats)
	}
	sent := snd.messages()
	if len(sent) != 2 || sent[0].EventID != e1.EventID || sent[1].EventID != e2.EventID {
		t.Fatalf("e3 must not overtake the failed e2: %+v", sent)
	}
	if len(st.published) != 1 || st.published[0] != e1.EventID || len(st.retries) != 1 || st.retries[0].id != e2.EventID {
		t.Errorf("published %v retries %v", st.published, st.retries)
	}
	var skipped []domain.UUID
	for _, a := range obs.attempts {
		if a.Result == ResultSkipped {
			skipped = append(skipped, a.EventID)
		}
	}
	if len(skipped) != 1 || skipped[0] != e3.EventID {
		t.Errorf("skipped %v", skipped)
	}
}

func TestClaimOrderIsRestoredBySeqBeforePublishing(t *testing.T) {
	agg := domain.NewUUID()
	e1, e2, e3 := testEvent(t, agg, 1), testEvent(t, agg, 2), testEvent(t, agg, 3)
	e1.Seq, e2.Seq, e3.Seq = 10, 20, 30
	// RETURNING handed the rows back shuffled.
	st := &fakeStore{events: []pg.ClaimedEvent{e3, e1, e2}}
	snd := &fakeSender{}
	p := newTestPublisher(t, testConfig(), st, snd, nil)
	if stats, err := p.RunOnce(context.Background()); err != nil || stats.Published != 3 {
		t.Fatalf("stats %+v err %v", stats, err)
	}
	sent := snd.messages()
	if len(sent) != 3 || sent[0].EventID != e1.EventID || sent[1].EventID != e2.EventID || sent[2].EventID != e3.EventID {
		t.Fatalf("events of one aggregate must be sent in seq order: %+v", sent)
	}
}

func TestShuffledClaimWithFailureSkipsLaterSeqNotLaterArrival(t *testing.T) {
	agg := domain.NewUUID()
	first, second := testEvent(t, agg, 1), testEvent(t, agg, 2)
	first.Seq, second.Seq = 1, 2
	st := &fakeStore{events: []pg.ClaimedEvent{second, first}}
	snd := &fakeSender{fn: func(_ context.Context, m Message) error {
		if m.EventID == first.EventID {
			return errors.New("broker error")
		}
		return nil
	}}
	p := newTestPublisher(t, testConfig(), st, snd, nil)
	stats, err := p.RunOnce(context.Background())
	if err != nil || stats.Retried != 1 || stats.Skipped != 1 || len(snd.messages()) != 1 || snd.messages()[0].EventID != first.EventID {
		t.Fatalf("the lower seq must be tried first and block the higher one: %+v %v", stats, err)
	}
}

func TestDistinctAggregatesArePublishedInParallel(t *testing.T) {
	const n = 4
	var events []pg.ClaimedEvent
	for i := 0; i < n; i++ {
		events = append(events, testEvent(t, domain.NewUUID(), i))
	}
	st := &fakeStore{events: events}
	var inflight, peak atomic.Int32
	release := make(chan struct{})
	snd := &fakeSender{fn: func(ctx context.Context, _ Message) error {
		cur := inflight.Add(1)
		for {
			p := peak.Load()
			if cur <= p || peak.CompareAndSwap(p, cur) {
				break
			}
		}
		if cur == n {
			close(release)
		}
		select {
		case <-release:
		case <-ctx.Done():
			return ctx.Err()
		}
		inflight.Add(-1)
		return nil
	}}
	cfg := testConfig()
	cfg.Concurrency = n
	p := newTestPublisher(t, cfg, st, snd, nil)
	stats, err := p.RunOnce(context.Background())
	if err != nil || stats.Published != n || peak.Load() != n {
		t.Fatalf("stats %+v peak %d err %v", stats, peak.Load(), err)
	}
}

func TestMalformedStoredEventIsNotSentAndDoesNotBlockOthers(t *testing.T) {
	good := testEvent(t, domain.NewUUID(), 1)
	notJSON := pg.ClaimedEvent{EventID: domain.NewUUID(), Payload: []byte("nope")}
	other := testEvent(t, domain.NewUUID(), 2)
	other.EventID = domain.NewUUID() // header no longer matches the row
	st := &fakeStore{events: []pg.ClaimedEvent{notJSON, other, good}}
	snd, obs := &fakeSender{}, &recorder{}
	p := newTestPublisher(t, testConfig(), st, snd, obs)
	stats, err := p.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats.Errors != 2 || stats.Published != 1 {
		t.Fatalf("stats %+v", stats)
	}
	if m := snd.messages(); len(m) != 1 || m[0].EventID != good.EventID {
		t.Fatalf("only the valid event may be sent: %+v", m)
	}
}

func TestLeaseLostAfterSendIsReportedNotRetried(t *testing.T) {
	st := &fakeStore{events: []pg.ClaimedEvent{testEvent(t, domain.NewUUID(), 1)}, markErr: pg.ErrConcurrentUpdate}
	obs := &recorder{}
	p := newTestPublisher(t, testConfig(), st, &fakeSender{}, obs)
	stats, err := p.RunOnce(context.Background())
	if err != nil || stats.LeaseLost != 1 || len(st.retries) != 0 {
		t.Fatalf("stats %+v retries %v err %v", stats, st.retries, err)
	}
	if obs.attempts[0].Result != ResultLeaseLost {
		t.Errorf("attempt %+v", obs.attempts[0])
	}
}

func TestUnrecordedPublishIsAnErrorThatStaysLeased(t *testing.T) {
	st := &fakeStore{events: []pg.ClaimedEvent{testEvent(t, domain.NewUUID(), 1)}, markErr: errors.New("connection reset")}
	p := newTestPublisher(t, testConfig(), st, &fakeSender{}, nil)
	stats, err := p.RunOnce(context.Background())
	if err != nil || stats.Errors != 1 || len(st.published) != 0 || len(st.retries) != 0 {
		t.Fatalf("stats %+v err %v", stats, err)
	}
}

func TestRetryBookkeepingFailureIsAnError(t *testing.T) {
	st := &fakeStore{events: []pg.ClaimedEvent{testEvent(t, domain.NewUUID(), 1)}, retryErr: errors.New("db down")}
	snd := &fakeSender{fn: func(context.Context, Message) error { return errors.New("broker down") }}
	p := newTestPublisher(t, testConfig(), st, snd, nil)
	stats, err := p.RunOnce(context.Background())
	if err != nil || stats.Errors != 1 {
		t.Fatalf("stats %+v err %v", stats, err)
	}
}

func TestSendTimeoutIsAppliedToEachEvent(t *testing.T) {
	st := &fakeStore{events: []pg.ClaimedEvent{testEvent(t, domain.NewUUID(), 1)}}
	snd := &fakeSender{fn: func(ctx context.Context, _ Message) error {
		<-ctx.Done()
		return ctx.Err()
	}}
	cfg := testConfig()
	cfg.SendTimeout = 20 * time.Millisecond
	p := newTestPublisher(t, cfg, st, snd, nil)
	stats, err := p.RunOnce(context.Background())
	if err != nil || stats.Retried != 1 {
		t.Fatalf("stats %+v err %v", stats, err)
	}
	if len(st.retries) != 1 || st.retries[0].reason != context.DeadlineExceeded.Error() {
		t.Errorf("retries %+v", st.retries)
	}
}

func TestLeaseBudgetStopsSendsThatCouldOutliveTheClaim(t *testing.T) {
	agg := domain.NewUUID()
	st := &fakeStore{events: []pg.ClaimedEvent{testEvent(t, agg, 1), testEvent(t, agg, 2)}}
	cfg := testConfig()
	cfg.Lease, cfg.SendTimeout, cfg.AckTimeout = 400*time.Millisecond, 100*time.Millisecond, 100*time.Millisecond
	snd := &fakeSender{fn: func(context.Context, Message) error {
		time.Sleep(250 * time.Millisecond) // first send eats most of the lease
		return nil
	}}
	p := newTestPublisher(t, cfg, st, snd, nil)
	stats, err := p.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats.Published != 1 || stats.Skipped != 1 || len(snd.messages()) != 1 {
		t.Fatalf("the second event must be left for the next claim: %+v sent %d", stats, len(snd.messages()))
	}
}

func TestClaimErrorIsReturned(t *testing.T) {
	boom := errors.New("db down")
	p := newTestPublisher(t, testConfig(), &fakeStore{claimErr: boom}, &fakeSender{}, nil)
	if _, err := p.RunOnce(context.Background()); !errors.Is(err, boom) {
		t.Fatalf("got %v", err)
	}
}

func TestRunDrainsFullBatchesAndStopsOnCancel(t *testing.T) {
	cfg := testConfig()
	cfg.BatchSize = 2
	cfg.PollInterval = time.Hour // only the full-batch shortcut may drive the loop
	var batches atomic.Int32
	st := &batchStore{total: 4, fake: &fakeStore{}, t: t, batches: &batches}
	p := newTestPublisher(t, cfg, st, &fakeSender{}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- p.Run(ctx) }()
	deadline := time.After(3 * time.Second)
	for batches.Load() < 3 {
		select {
		case <-deadline:
			t.Fatalf("expected the loop to drain full batches without waiting, got %d claims", batches.Load())
		case <-time.After(time.Millisecond):
		}
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not stop after cancel")
	}
}

// batchStore hands out events up to total, then nothing.
type batchStore struct {
	mu      sync.Mutex
	total   int
	served  int
	fake    *fakeStore
	t       *testing.T
	batches *atomic.Int32
}

func (b *batchStore) ClaimOutbox(_ context.Context, owner string, limit int, _ time.Duration) ([]pg.ClaimedEvent, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.batches.Add(1)
	n := min(limit, b.total-b.served)
	var out []pg.ClaimedEvent
	for i := 0; i < n; i++ {
		out = append(out, testEvent(b.t, domain.NewUUID(), b.served+i))
	}
	b.served += n
	return out, nil
}
func (b *batchStore) MarkPublished(ctx context.Context, id domain.UUID, o string) error {
	return b.fake.MarkPublished(ctx, id, o)
}
func (b *batchStore) RetryOutbox(ctx context.Context, id domain.UUID, o string, d time.Duration, r string) error {
	return b.fake.RetryOutbox(ctx, id, o, d, r)
}

func TestStopWaitsForInFlightEventThenReleases(t *testing.T) {
	st := &fakeStore{events: []pg.ClaimedEvent{testEvent(t, domain.NewUUID(), 1), testEvent(t, domain.NewUUID(), 2)}}
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	snd := &fakeSender{fn: func(ctx context.Context, _ Message) error {
		once.Do(func() { close(started) })
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}}
	cfg := testConfig()
	cfg.Concurrency = 1
	p := newTestPublisher(t, cfg, st, snd, nil)
	p.Start()
	p.Start() // idempotent
	<-started
	stopped := make(chan error, 1)
	go func() { stopped <- p.Stop(context.Background()) }()
	select {
	case err := <-stopped:
		t.Fatalf("Stop returned before the in-flight event finished: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	select {
	case err := <-stopped:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Stop did not return")
	}
	if n := len(snd.messages()); n != 1 {
		t.Errorf("after stop only the event in flight may have been sent, sent %d", n)
	}
	if len(st.published) != 1 {
		t.Errorf("the in-flight event must be acknowledged: %v", st.published)
	}
	if err := p.Stop(context.Background()); err != nil {
		t.Errorf("a second Stop must be a no-op, got %v", err)
	}
}

func TestStopDeadlineCancelsInFlightSend(t *testing.T) {
	st := &fakeStore{events: []pg.ClaimedEvent{testEvent(t, domain.NewUUID(), 1)}}
	started := make(chan struct{})
	snd := &fakeSender{fn: func(ctx context.Context, _ Message) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	}}
	p := newTestPublisher(t, testConfig(), st, snd, nil)
	p.Start()
	<-started
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := p.Stop(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected the stop deadline error, got %v", err)
	}
	if len(st.published) != 0 {
		t.Error("a cancelled send must not be acknowledged")
	}
}
