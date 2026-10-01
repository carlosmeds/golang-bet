package outbox_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"go.uber.org/fx"
	"go.uber.org/fx/fxtest"
	"wagering/internal/domain"
	"wagering/internal/storage/pg"
	"wagering/internal/workers/outbox"
	"wagering/migrations"
)

// These tests need PostgreSQL: set WAGERING_TEST_ADMIN_URL to an admin DSN,
// e.g. postgres://wagering:wagering_password@localhost:54320/postgres?sslmode=disable.
// Each test creates and drops its own database. Crashes are simulated by
// abandoning a claim (no acknowledgement) or by failing the acknowledgement,
// and lease expiry by moving lease_expires_at into the past.

type env struct {
	t     *testing.T
	url   string
	store *pg.Store
}

func newEnv(t *testing.T) *env {
	t.Helper()
	admin := os.Getenv("WAGERING_TEST_ADMIN_URL")
	if admin == "" {
		t.Skip("set WAGERING_TEST_ADMIN_URL for PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, admin)
	if err != nil {
		t.Fatal(err)
	}
	db := "wagering_outbox_" + strings.ReplaceAll(domain.NewUUID().String(), "-", "")
	if _, err = conn.Exec(ctx, "CREATE DATABASE "+db); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, e := conn.Exec(ctx, "DROP DATABASE IF EXISTS "+db+" WITH (FORCE)"); e != nil {
			t.Errorf("drop database: %v", e)
		}
		conn.Close(ctx)
	})
	u, err := url.Parse(admin)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + db
	e := &env{t: t, url: u.String()}
	e.store = e.open()
	names, err := migrations.FS.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	var ups []string
	for _, n := range names {
		if strings.HasSuffix(n.Name(), ".up.sql") {
			ups = append(ups, n.Name())
		}
	}
	sort.Strings(ups)
	for _, name := range ups {
		sql, err := migrations.FS.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = e.store.Pool.Exec(ctx, string(sql)); err != nil {
			t.Fatalf("migration %s: %v", name, err)
		}
	}
	return e
}

// open simulates a process start: a new pool on the same database.
func (e *env) open() *pg.Store {
	s, err := pg.Open(context.Background(), e.url)
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(s.Close)
	return s
}

type data struct {
	N int `json:"n"`
}

// insert commits one outbox event per call, like a financial transaction does.
func (e *env) insert(aggregate domain.UUID, n int) domain.UUID {
	e.t.Helper()
	ev := domain.Envelope[data]{
		EventMeta: domain.EventMeta{
			EventID: domain.NewUUID(), EventType: domain.EventWalletBalanceChanged, AggregateID: aggregate,
			CorrelationID: "corr-1", CausationID: "cause-1", OccurredAt: time.Now().UTC(), Version: domain.EventVersion,
		},
		Data: data{N: n},
	}
	err := e.store.WithTx(context.Background(), func(tx *pg.Tx) error {
		return tx.InsertOutbox(context.Background(), ev, time.Now().UTC())
	})
	if err != nil {
		e.t.Fatal(err)
	}
	return ev.EventID
}

func (e *env) exec(sql string, args ...any) {
	e.t.Helper()
	if _, err := e.store.Pool.Exec(context.Background(), sql, args...); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) expireLeases() {
	e.t.Helper()
	e.exec(`UPDATE outbox_events SET lease_expires_at = now() - interval '1 second' WHERE lease_owner IS NOT NULL`)
}

func (e *env) publishedCount() int {
	e.t.Helper()
	var n int
	if err := e.store.Pool.QueryRow(context.Background(), `SELECT count(*) FROM outbox_events WHERE published_at IS NOT NULL`).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func (e *env) payload(id domain.UUID) string {
	e.t.Helper()
	var p string
	if err := e.store.Pool.QueryRow(context.Background(), `SELECT payload::text FROM outbox_events WHERE event_id=$1`, id.String()).Scan(&p); err != nil {
		e.t.Fatal(err)
	}
	return p
}

func testConfig(owner string) outbox.Config {
	c := outbox.DefaultConfig()
	c.Owner = owner
	c.Lease = 10 * time.Second
	c.SendTimeout = 2 * time.Second
	c.AckTimeout = 2 * time.Second
	c.PollInterval = 10 * time.Millisecond
	return c
}

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func (e *env) publisher(store outbox.Store, cfg outbox.Config, s outbox.Sender) *outbox.Publisher {
	e.t.Helper()
	p, err := outbox.NewPublisher(cfg, store, s, nil, discard())
	if err != nil {
		e.t.Fatal(err)
	}
	return p
}

// sink is an in-memory queue that records every accepted message.
type sink struct {
	mu   sync.Mutex
	msgs []outbox.Message
	fn   func(ctx context.Context, m outbox.Message) error
}

func (s *sink) Send(ctx context.Context, m outbox.Message) error {
	if s.fn != nil {
		if err := s.fn(ctx, m); err != nil {
			return err
		}
	}
	s.mu.Lock()
	s.msgs = append(s.msgs, m)
	s.mu.Unlock()
	return nil
}

func (s *sink) all() []outbox.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]outbox.Message(nil), s.msgs...)
}

func (s *sink) countBy(id domain.UUID) int {
	n := 0
	for _, m := range s.all() {
		if m.EventID == id {
			n++
		}
	}
	return n
}

// crashingStore fails every acknowledgement, like a process that dies after the
// broker accepted the message but before the row was marked.
type crashingStore struct{ *pg.Store }

var errCrash = errors.New("simulated crash before acknowledgement")

func (crashingStore) MarkPublished(context.Context, domain.UUID, string) error { return errCrash }

func TestPublishesCommittedEventsOnceWithStableIdentity(t *testing.T) {
	e := newEnv(t)
	agg := domain.NewUUID()
	ids := []domain.UUID{e.insert(agg, 1), e.insert(agg, 2), e.insert(agg, 3)}
	q := &sink{}
	p := e.publisher(e.store, testConfig("pub-a"), q)
	st, err := p.RunOnce(context.Background())
	if err != nil || st.Published != 3 {
		t.Fatalf("stats %+v err %v", st, err)
	}
	msgs := q.all()
	if len(msgs) != 3 {
		t.Fatalf("sent %d messages", len(msgs))
	}
	for i, m := range msgs {
		if m.EventID != ids[i] || m.MessageDeduplicationID != ids[i].String() || m.MessageGroupID != agg.String() {
			t.Errorf("message %d has identity %+v, want event %s in outbox order", i, m, ids[i])
		}
		if string(m.Body) != e.payload(ids[i]) && !jsonEqual(m.Body, e.payload(ids[i])) {
			t.Errorf("message %d is not the stored snapshot", i)
		}
	}
	var leases int
	if err := e.store.Pool.QueryRow(context.Background(), `SELECT count(*) FROM outbox_events WHERE lease_owner IS NOT NULL OR published_at IS NULL OR attempt_count <> 1`).Scan(&leases); err != nil || leases != 0 {
		t.Fatalf("published rows must be final with one attempt and no lease: %d %v", leases, err)
	}
	if st, _ := p.RunOnce(context.Background()); st.Claimed != 0 {
		t.Fatalf("published events must not be claimed again: %+v", st)
	}
}

func jsonEqual(body []byte, stored string) bool {
	norm := func(s string) string { return strings.Join(strings.Fields(s), "") }
	return norm(string(body)) == norm(stored)
}

func TestClaimedEventsCarryTheOutboxSequence(t *testing.T) {
	e := newEnv(t)
	agg := domain.NewUUID()
	ids := []domain.UUID{e.insert(agg, 1), e.insert(agg, 2), e.insert(agg, 3)}
	got, err := e.store.ClaimOutbox(context.Background(), "pub-a", 10, time.Minute)
	if err != nil || len(got) != 3 {
		t.Fatalf("claim: %v %v", got, err)
	}
	bySeq := map[domain.UUID]int64{}
	for _, c := range got {
		bySeq[c.EventID] = c.Seq
	}
	if !(bySeq[ids[0]] > 0 && bySeq[ids[0]] < bySeq[ids[1]] && bySeq[ids[1]] < bySeq[ids[2]]) {
		t.Fatalf("seq must follow insertion order: %v", bySeq)
	}
}

func TestConcurrentClaimsNeverOverlap(t *testing.T) {
	e := newEnv(t)
	for i := 0; i < 60; i++ {
		e.insert(domain.NewUUID(), i)
	}
	stores := []*pg.Store{e.store, e.open(), e.open(), e.open()}
	var mu sync.Mutex
	seen := map[domain.UUID]string{}
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i, s := range stores {
		wg.Add(1)
		go func() {
			defer wg.Done()
			owner := fmt.Sprintf("claimer-%d", i)
			<-start
			for {
				got, err := s.ClaimOutbox(context.Background(), owner, 7, time.Minute)
				if err != nil {
					t.Error(err)
					return
				}
				if len(got) == 0 {
					return
				}
				mu.Lock()
				for _, c := range got {
					if prev, dup := seen[c.EventID]; dup {
						t.Errorf("event %s claimed by %s and %s at the same time", c.EventID, prev, owner)
					}
					seen[c.EventID] = owner
				}
				mu.Unlock()
			}
		}()
	}
	close(start)
	wg.Wait()
	if len(seen) != 60 {
		t.Fatalf("claimed %d of 60 events", len(seen))
	}
}

// Acceptance: two publishers avoid simultaneous claims.
func TestTwoPublishersPublishEachEventExactlyOnce(t *testing.T) {
	e := newEnv(t)
	const aggregates, perAggregate = 12, 5
	want := map[domain.UUID]int{}
	for a := 0; a < aggregates; a++ {
		agg := domain.NewUUID()
		for n := 0; n < perAggregate; n++ {
			want[e.insert(agg, n)] = n
		}
	}
	q := &sink{fn: func(ctx context.Context, m outbox.Message) error {
		time.Sleep(2 * time.Millisecond) // widen the window in which a duplicate claim would show
		return nil
	}}
	cfgA, cfgB := testConfig("pub-a"), testConfig("pub-b")
	cfgA.BatchSize, cfgB.BatchSize = 9, 9
	a, b := e.publisher(e.store, cfgA, q), e.publisher(e.open(), cfgB, q)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wg sync.WaitGroup
	for _, p := range []*outbox.Publisher{a, b} {
		wg.Add(1)
		go func() { defer wg.Done(); _ = p.Run(ctx) }()
	}
	deadline := time.Now().Add(20 * time.Second)
	for e.publishedCount() < len(want) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	wg.Wait()
	if n := e.publishedCount(); n != len(want) {
		t.Fatalf("published %d of %d", n, len(want))
	}
	for id := range want {
		if n := q.countBy(id); n != 1 {
			t.Errorf("event %s was sent %d times by two publishers", id, n)
		}
	}
	if got := len(q.all()); got != len(want) {
		t.Errorf("sent %d messages for %d events", got, len(want))
	}
}

// Acceptance: crash before publish recovers with the same eventId.
func TestCrashBeforePublishIsRecoveredAfterLeaseExpiry(t *testing.T) {
	e := newEnv(t)
	id := e.insert(domain.NewUUID(), 1)
	// Publisher A claims and dies before sending anything.
	claimed, err := e.store.ClaimOutbox(context.Background(), "pub-a", 10, time.Minute)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim: %v %v", claimed, err)
	}
	q := &sink{}
	b := e.publisher(e.open(), testConfig("pub-b"), q)
	if st, err := b.RunOnce(context.Background()); err != nil || st.Claimed != 0 {
		t.Fatalf("B must not take an event whose lease is live: %+v %v", st, err)
	}
	e.expireLeases()
	st, err := b.RunOnce(context.Background())
	if err != nil || st.Published != 1 {
		t.Fatalf("B must recover the abandoned event: %+v %v", st, err)
	}
	msgs := q.all()
	if len(msgs) != 1 || msgs[0].EventID != id || msgs[0].MessageDeduplicationID != id.String() {
		t.Fatalf("recovered message %+v, want event %s", msgs, id)
	}
	// A late acknowledgement from the crashed owner is refused.
	if err := e.store.MarkPublished(context.Background(), id, "pub-a"); !errors.Is(err, pg.ErrConcurrentUpdate) {
		t.Errorf("a stale owner must not acknowledge: %v", err)
	}
}

// Acceptance: crash after publish recovers with the same eventId.
func TestCrashAfterPublishRepublishesSameEventID(t *testing.T) {
	e := newEnv(t)
	agg := domain.NewUUID()
	id := e.insert(agg, 1)
	stored := e.payload(id)
	q := &sink{}
	// A: broker accepts, then the process dies before MarkPublished.
	a := e.publisher(crashingStore{e.store}, testConfig("pub-a"), q)
	st, err := a.RunOnce(context.Background())
	if err != nil || st.Errors != 1 || st.Published != 0 {
		t.Fatalf("A: %+v %v", st, err)
	}
	if e.publishedCount() != 0 {
		t.Fatal("the row must still be unpublished after the crash")
	}
	b := e.publisher(e.open(), testConfig("pub-b"), q)
	if st, _ := b.RunOnce(context.Background()); st.Claimed != 0 {
		t.Fatalf("B must wait for the lease: %+v", st)
	}
	e.expireLeases()
	if st, err := b.RunOnce(context.Background()); err != nil || st.Published != 1 {
		t.Fatalf("B: %+v %v", st, err)
	}
	msgs := q.all()
	if len(msgs) != 2 {
		t.Fatalf("expected a repeat publication, got %d messages", len(msgs))
	}
	for _, m := range msgs {
		if m.EventID != id || m.MessageDeduplicationID != id.String() || m.MessageGroupID != agg.String() {
			t.Errorf("repeat publication changed identity: %+v", m)
		}
	}
	if string(msgs[0].Body) != string(msgs[1].Body) {
		t.Error("both publications must carry the identical snapshot")
	}
	if e.payload(id) != stored {
		t.Error("the stored snapshot changed")
	}
	if e.publishedCount() != 1 {
		t.Error("the event must end up published")
	}
}

func TestLeaseExpiringDuringSendLeavesEventToNewOwner(t *testing.T) {
	e := newEnv(t)
	id := e.insert(domain.NewUUID(), 1)
	q := &sink{}
	var once sync.Once
	q.fn = func(ctx context.Context, m outbox.Message) error {
		once.Do(e.expireLeases) // the send "takes longer than the lease"
		return nil
	}
	a := e.publisher(e.store, testConfig("pub-a"), q)
	st, err := a.RunOnce(context.Background())
	if err != nil || st.LeaseLost != 1 || st.Published != 0 {
		t.Fatalf("A must not acknowledge with an expired lease: %+v %v", st, err)
	}
	b := e.publisher(e.open(), testConfig("pub-b"), q)
	if st, err := b.RunOnce(context.Background()); err != nil || st.Published != 1 {
		t.Fatalf("B: %+v %v", st, err)
	}
	if n := q.countBy(id); n != 2 {
		t.Errorf("expected the same event twice, got %d", n)
	}
}

func TestFailedSendBacksOffExponentiallyAndRecordsError(t *testing.T) {
	e := newEnv(t)
	id := e.insert(domain.NewUUID(), 1)
	cfg := testConfig("pub-a")
	cfg.Backoff = outbox.Backoff{BaseDelay: 10 * time.Second, MaxDelay: 40 * time.Second}
	fail := true
	q := &sink{fn: func(context.Context, outbox.Message) error {
		if fail {
			return errors.New("queue unavailable")
		}
		return nil
	}}
	p := e.publisher(e.store, cfg, q)
	for i, want := range []float64{10, 20, 40, 40} {
		st, err := p.RunOnce(context.Background())
		if err != nil || st.Retried != 1 {
			t.Fatalf("attempt %d: %+v %v", i+1, st, err)
		}
		var wait float64
		var attempts int
		var lastErr string
		var leased bool
		err = e.store.Pool.QueryRow(context.Background(),
			`SELECT extract(epoch FROM next_attempt_at - now()), attempt_count, last_error, lease_owner IS NOT NULL FROM outbox_events WHERE event_id=$1`, id.String()).
			Scan(&wait, &attempts, &lastErr, &leased)
		if err != nil {
			t.Fatal(err)
		}
		if wait < want-2 || wait > want || attempts != i+1 || lastErr != "queue unavailable" || leased {
			t.Fatalf("attempt %d: next in %.1fs (want ~%.0fs), attempts %d, last_error %q, leased %v", i+1, wait, want, attempts, lastErr, leased)
		}
		if st, _ := p.RunOnce(context.Background()); st.Claimed != 0 {
			t.Fatalf("attempt %d: an event that is not due must not be claimed", i+1)
		}
		e.exec(`UPDATE outbox_events SET next_attempt_at = now() WHERE event_id=$1`, id.String())
	}
	fail = false
	if st, err := p.RunOnce(context.Background()); err != nil || st.Published != 1 {
		t.Fatalf("after the broker recovers: %+v %v", st, err)
	}
	if e.publishedCount() != 1 {
		t.Fatal("the event must be published after earlier failures")
	}
}

func TestFailedEventDoesNotLetLaterEventOfSameAggregateOvertake(t *testing.T) {
	e := newEnv(t)
	agg := domain.NewUUID()
	first, second := e.insert(agg, 1), e.insert(agg, 2)
	other := e.insert(domain.NewUUID(), 3)
	q := &sink{fn: func(_ context.Context, m outbox.Message) error {
		if m.EventID == first {
			return errors.New("throttled")
		}
		return nil
	}}
	p := e.publisher(e.store, testConfig("pub-a"), q)
	st, err := p.RunOnce(context.Background())
	if err != nil || st.Retried != 1 || st.Skipped != 1 || st.Published != 1 {
		t.Fatalf("stats %+v err %v", st, err)
	}
	if q.countBy(second) != 0 || q.countBy(other) != 1 {
		t.Fatalf("second must wait for first; the other aggregate is unaffected")
	}
}

func TestShutdownStopsClaimingAndFinishesInFlightEvent(t *testing.T) {
	e := newEnv(t)
	for i := 0; i < 5; i++ {
		e.insert(domain.NewUUID(), i)
	}
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	q := &sink{fn: func(ctx context.Context, m outbox.Message) error {
		once.Do(func() { close(started) })
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}}
	cfg := testConfig("pub-a")
	cfg.Concurrency = 1
	cfg.BatchSize = 5
	p := e.publisher(e.store, cfg, q)
	p.Start()
	<-started
	stopped := make(chan error, 1)
	go func() { stopped <- p.Stop(context.Background()) }()
	time.Sleep(50 * time.Millisecond)
	close(release)
	if err := <-stopped; err != nil {
		t.Fatal(err)
	}
	if n := e.publishedCount(); n != 1 {
		t.Fatalf("only the event in flight may finish during shutdown, published %d", n)
	}
	// The rest stay claimed until the lease ends, then another instance takes them.
	e.expireLeases()
	q2 := &sink{}
	b := e.publisher(e.open(), testConfig("pub-b"), q2)
	if st, err := b.RunOnce(context.Background()); err != nil || st.Published != 4 {
		t.Fatalf("B: %+v %v", st, err)
	}
}

func TestStopDeadlineCancelsSendAndKeepsEventUnpublished(t *testing.T) {
	e := newEnv(t)
	id := e.insert(domain.NewUUID(), 1)
	started := make(chan struct{})
	q := &sink{fn: func(ctx context.Context, m outbox.Message) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	}}
	p := e.publisher(e.store, testConfig("pub-a"), q)
	p.Start()
	<-started
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := p.Stop(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v", err)
	}
	if e.publishedCount() != 0 {
		t.Fatal("a cancelled send must not mark the event published")
	}
	e.expireLeases()
	q2 := &sink{}
	b := e.publisher(e.open(), testConfig("pub-b"), q2)
	if st, err := b.RunOnce(context.Background()); err != nil || st.Published != 1 || q2.countBy(id) != 1 {
		t.Fatalf("B: %+v %v", st, err)
	}
}

func TestPublishedOnlyAfterCommit(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	ev := domain.Envelope[data]{
		EventMeta: domain.EventMeta{EventID: domain.NewUUID(), EventType: domain.EventWalletBalanceChanged, AggregateID: domain.NewUUID(),
			CorrelationID: "c", OccurredAt: time.Now().UTC(), Version: 1},
	}
	q := &sink{}
	p := e.publisher(e.store, testConfig("pub-a"), q)
	rollback := errors.New("rollback")
	err := e.store.WithTx(ctx, func(tx *pg.Tx) error {
		if err := tx.InsertOutbox(ctx, ev, time.Now().UTC()); err != nil {
			return err
		}
		// Inside the open transaction the row is invisible to the publisher.
		if st, err := p.RunOnce(ctx); err != nil || st.Claimed != 0 {
			t.Errorf("uncommitted event was visible: %+v %v", st, err)
		}
		return rollback
	})
	if !errors.Is(err, rollback) {
		t.Fatal(err)
	}
	if st, _ := p.RunOnce(ctx); st.Claimed != 0 || len(q.all()) != 0 {
		t.Fatal("a rolled back event must never be published")
	}
}

func TestFxModuleRunsPublisherWithLifecycle(t *testing.T) {
	e := newEnv(t)
	id := e.insert(domain.NewUUID(), 1)
	q := &sink{}
	t.Setenv("OUTBOX_POLL_INTERVAL", "10ms")
	var pub *outbox.Publisher
	app := fxtest.New(t,
		fx.NopLogger,
		fx.Supply(e.store),
		fx.Provide(func() outbox.Sender { return q }),
		outbox.Module,
		fx.Populate(&pub),
	)
	app.RequireStart()
	deadline := time.Now().Add(10 * time.Second)
	for e.publishedCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	app.RequireStop()
	if q.countBy(id) != 1 || e.publishedCount() != 1 {
		t.Fatalf("the module must publish the event once: sent %d", q.countBy(id))
	}
	late := e.insert(domain.NewUUID(), 2)
	time.Sleep(100 * time.Millisecond)
	if q.countBy(late) != 0 {
		t.Fatal("a stopped publisher must not claim new events")
	}
}
