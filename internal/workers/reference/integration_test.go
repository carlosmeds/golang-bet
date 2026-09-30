package reference_test

import (
	"context"
	"fmt"
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
	"wagering/internal/contract"
	"wagering/internal/domain"
	"wagering/internal/storage/pg"
	wagering "wagering/internal/usecase/wagering"
	walletuse "wagering/internal/usecase/wallet"
	"wagering/internal/workers/reference"
	"wagering/migrations"
)

// These tests need PostgreSQL: set WAGERING_TEST_ADMIN_URL to an admin DSN,
// e.g. postgres://user:pass@localhost:54320/postgres?sslmode=disable. Each
// test creates and drops its own database.

type env struct {
	t     *testing.T
	url   string
	store *pg.Store
	clock *clock
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
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
	db := "wagering_ref_" + strings.ReplaceAll(domain.NewUUID().String(), "-", "")
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
	e := &env{t: t, url: u.String(), clock: &clock{t: time.Now().UTC()}}
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

func (e *env) policy(maxAttempts int, ttl time.Duration) domain.RetryPolicy {
	return domain.RetryPolicy{BaseDelay: time.Second, MaxDelay: time.Minute, TTL: ttl, MaxAttempts: maxAttempts}
}

func (e *env) service(s *pg.Store, p domain.RetryPolicy) *wagering.Service {
	svc := wagering.New(s)
	svc.Retry = p
	svc.Now = e.clock.Now
	return svc
}

func (e *env) worker(s *pg.Store, svc *wagering.Service, owner string, batch int) *reference.Worker {
	cfg := reference.DefaultConfig()
	cfg.Owner = owner
	cfg.BatchSize = batch
	cfg.Lease = 5 * time.Second
	cfg.ItemTimeout = 3 * time.Second
	cfg.PollInterval = 20 * time.Millisecond
	cfg.Retry = svc.Retry
	w, err := reference.NewWorker(cfg, s, svc, nil, nil)
	if err != nil {
		e.t.Fatal(err)
	}
	return w
}

func (e *env) wallet(balance int64) domain.UUID {
	e.t.Helper()
	res, err := walletuse.New(e.store).Open(context.Background(), "player", money(balance), "open")
	if err != nil {
		e.t.Fatal(err)
	}
	return res.Wallet.ID()
}

// makeDue moves the retry schedule of one transaction (by external id) into
// the past so the next claim sees it, without sleeping through real backoff.
func (e *env) makeDue(external string) {
	e.t.Helper()
	tag, err := e.store.Pool.Exec(context.Background(), `UPDATE wager_transactions SET next_attempt_at = now() - interval '1 second' WHERE external_transaction_id=$1 AND status IN ('PENDING','PENDING_REFERENCE')`, external)
	if err != nil || tag.RowsAffected() != 1 {
		e.t.Fatalf("makeDue %s: rows=%d err=%v", external, tag.RowsAffected(), err)
	}
}

func (e *env) makeAllDue() {
	e.t.Helper()
	if _, err := e.store.Pool.Exec(context.Background(), `UPDATE wager_transactions SET next_attempt_at = now() - interval '1 second' WHERE status IN ('PENDING','PENDING_REFERENCE')`); err != nil {
		e.t.Fatal(err)
	}
}

func money(n int64) domain.Money { m, _ := domain.NewMoney(n, "BRL"); return m }

func operation(id, ref string, kind domain.Kind, wallet domain.UUID, amount int64) contract.Operation {
	return contract.Operation{ProviderID: "provider-a", ExternalTransactionID: id, PlayerID: "player", WalletID: wallet, RoundID: "round", GameID: "game", Kind: kind, Money: money(amount), ReferenceExternalTransactionID: ref}
}

func (e *env) exec(svc *wagering.Service, op contract.Operation) wagering.Result {
	e.t.Helper()
	res, err := svc.Execute(context.Background(), op, "key-"+op.ExternalTransactionID, op.ExternalTransactionID, nil)
	if err != nil {
		e.t.Fatalf("execute %s: %v", op.ExternalTransactionID, err)
	}
	return res
}

type row struct {
	Status      string
	Failure     string
	Attempts    int
	Next, Lease bool
}

func (e *env) row(external string) row {
	e.t.Helper()
	var r row
	var failure *string
	err := e.store.Pool.QueryRow(context.Background(), `SELECT status, failure_code, attempt_count, next_attempt_at IS NOT NULL, lease_owner IS NOT NULL FROM wager_transactions WHERE external_transaction_id=$1`, external).Scan(&r.Status, &failure, &r.Attempts, &r.Next, &r.Lease)
	if err != nil {
		e.t.Fatal(err)
	}
	if failure != nil {
		r.Failure = *failure
	}
	return r
}

func (e *env) count(query string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.store.Pool.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func (e *env) events(external, eventType string) int {
	return e.count(`SELECT count(*) FROM outbox_events o JOIN wager_transactions t ON o.causation_id=t.id::text WHERE t.external_transaction_id=$1 AND o.event_type=$2`, external, eventType)
}

func (e *env) ledger(external string) int {
	return e.count(`SELECT count(*) FROM ledger_entries l JOIN wager_transactions t ON t.id=l.transaction_id WHERE t.external_transaction_id=$1`, external)
}

func (e *env) balance(wallet domain.UUID) (int64, int64) {
	e.t.Helper()
	var bal, ver int64
	if err := e.store.Pool.QueryRow(context.Background(), `SELECT balance_amount, version FROM wallets WHERE id=$1`, wallet.String()).Scan(&bal, &ver); err != nil {
		e.t.Fatal(err)
	}
	return bal, ver
}

func (e *env) runOnce(w *reference.Worker) reference.Stats {
	e.t.Helper()
	st, err := w.RunOnce(context.Background())
	if err != nil {
		e.t.Fatal(err)
	}
	return st
}

func TestMissingReferenceSurvivesRestartAndResolvesLater(t *testing.T) {
	e := newEnv(t)
	policy := e.policy(0, 24*time.Hour)
	wallet := e.wallet(10000)

	pending := e.exec(e.service(e.store, policy), operation("refund-1", "bet-1", domain.KindRefund, wallet, 500))
	if pending.Transaction.Status() != domain.StatusPendingReference {
		t.Fatalf("expected PENDING_REFERENCE, got %s", pending.Transaction.Status())
	}
	if e.events("refund-1", "WagerTransactionPendingReference") != 1 {
		t.Fatal("pending-reference event missing")
	}

	// Restart: the first process is gone, everything it knew was in PostgreSQL.
	e.store.Close()
	store2 := e.open()
	e.store = store2
	svc2 := e.service(store2, policy)
	w := e.worker(store2, svc2, "instance-2", 10)

	if st := e.runOnce(w); st.Claimed != 0 {
		t.Fatalf("row claimed before its backoff elapsed: %+v", st)
	}
	e.exec(svc2, operation("bet-1", "", domain.KindBet, wallet, 500)) // reference finally arrives
	e.makeDue("refund-1")
	if st := e.runOnce(w); st.Claimed != 1 || st.Resolved != 1 {
		t.Fatalf("expected the refund to resolve, got %+v", st)
	}

	if r := e.row("refund-1"); r.Status != "PROCESSED" || r.Next || r.Lease {
		t.Fatalf("refund row after resolve: %+v", r)
	}
	if bal, _ := e.balance(wallet); bal != 10000 {
		t.Fatalf("bet debit plus refund credit should net to 10000, got %d", bal)
	}
	if e.ledger("refund-1") != 1 || e.events("refund-1", "WagerTransactionProcessed") != 1 {
		t.Fatal("refund must have exactly one ledger entry and one processed event")
	}
	if st := e.runOnce(w); st.Claimed != 0 {
		t.Fatalf("resolved row was claimed again: %+v", st)
	}
}

func TestRetryBacksOffExponentiallyWhileReferenceMissing(t *testing.T) {
	e := newEnv(t)
	policy := e.policy(0, 24*time.Hour)
	wallet := e.wallet(1000)
	svc := e.service(e.store, policy)
	e.exec(svc, operation("refund-b", "never", domain.KindRefund, wallet, 100))
	w := e.worker(e.store, svc, "w", 10)

	var gaps []time.Duration
	for i := 0; i < 3; i++ {
		e.makeDue("refund-b")
		e.clock.Advance(time.Minute) // keep well inside the TTL; only the gap matters
		e.runOnce(w)
		var next time.Time
		if err := e.store.Pool.QueryRow(context.Background(), `SELECT next_attempt_at FROM wager_transactions WHERE external_transaction_id='refund-b'`).Scan(&next); err != nil {
			t.Fatal(err)
		}
		gaps = append(gaps, next.Sub(e.clock.Now()))
	}
	// Attempt counts 2,3,4 schedule backoff 2s,4s,8s from "now".
	want := []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second}
	for i := range want {
		if d := gaps[i] - want[i]; d < -time.Millisecond || d > time.Millisecond {
			t.Fatalf("backoff %d = %v, want %v", i, gaps[i], want[i])
		}
	}
	if r := e.row("refund-b"); r.Attempts != 4 || r.Status != "PENDING_REFERENCE" {
		t.Fatalf("row %+v", r)
	}
}

func TestAttemptExhaustionRejectsWithStableCodeAndEvent(t *testing.T) {
	e := newEnv(t)
	policy := e.policy(3, 24*time.Hour)
	wallet := e.wallet(10000)
	svc := e.service(e.store, policy)
	e.exec(svc, operation("refund-x", "missing-bet", domain.KindRefund, wallet, 500))
	_, versionBefore := e.balance(wallet)
	w := e.worker(e.store, svc, "w", 10)

	e.makeDue("refund-x")
	if st := e.runOnce(w); st.Pending != 1 {
		t.Fatalf("second attempt should still wait: %+v", st)
	}
	e.makeDue("refund-x")
	if st := e.runOnce(w); st.Rejected != 1 {
		t.Fatalf("third attempt should exhaust: %+v", st)
	}

	r := e.row("refund-x")
	if r.Status != "REJECTED" || r.Failure != string(domain.FailureReferenceNotFound) || r.Next || r.Lease {
		t.Fatalf("row %+v", r)
	}
	if e.events("refund-x", "WagerTransactionRejected") != 1 {
		t.Fatal("rejection event missing or duplicated")
	}
	if e.events("refund-x", "WagerTransactionPendingReference") != 1 {
		t.Fatal("pending event must be emitted once, on entry only")
	}
	if e.ledger("refund-x") != 0 {
		t.Fatal("rejected operation must not write a ledger entry")
	}
	if bal, ver := e.balance(wallet); bal != 10000 || ver != versionBefore {
		t.Fatalf("wallet changed: balance %d version %d", bal, ver)
	}
	if st := e.runOnce(w); st.Claimed != 0 {
		t.Fatalf("terminal row claimed again: %+v", st)
	}
}

func TestTTLExpiryRejectsNotFoundAndUnresolved(t *testing.T) {
	e := newEnv(t)
	policy := e.policy(0, time.Hour)
	wallet := e.wallet(10000)
	svc := e.service(e.store, policy)
	e.exec(svc, operation("refund-t", "gone", domain.KindRefund, wallet, 500))
	// The rollback targets the refund, which stays unfinished until its own TTL.
	e.exec(svc, operation("rollback-t", "refund-t", domain.KindRollback, wallet, 500))
	if e.row("rollback-t").Status != "PENDING_REFERENCE" {
		t.Fatalf("rollback of an unfinished refund must wait: %+v", e.row("rollback-t"))
	}
	w := e.worker(e.store, svc, "w", 10)

	e.clock.Advance(90 * time.Minute) // past the 1h TTL
	e.makeDue("rollback-t")
	if st := e.runOnce(w); st.Rejected != 1 {
		t.Fatalf("rollback: %+v", st)
	}
	if r := e.row("rollback-t"); r.Status != "REJECTED" || r.Failure != string(domain.FailureReferenceUnresolved) {
		t.Fatalf("rollback row %+v", r)
	}
	e.makeDue("refund-t")
	if st := e.runOnce(w); st.Rejected != 1 {
		t.Fatalf("refund: %+v", st)
	}
	if r := e.row("refund-t"); r.Status != "REJECTED" || r.Failure != string(domain.FailureReferenceNotFound) {
		t.Fatalf("refund row %+v", r)
	}
	for _, id := range []string{"refund-t", "rollback-t"} {
		if e.events(id, "WagerTransactionRejected") != 1 || e.ledger(id) != 0 {
			t.Fatalf("%s: expected one rejection event and no ledger entry", id)
		}
	}
	if bal, _ := e.balance(wallet); bal != 10000 {
		t.Fatalf("balance %d", bal)
	}
}

func TestReferenceThatFailedIsRejectedDefinitively(t *testing.T) {
	e := newEnv(t)
	policy := e.policy(0, 24*time.Hour)
	wallet := e.wallet(1000)
	svc := e.service(e.store, policy)
	e.exec(svc, operation("refund-f", "bet-f", domain.KindRefund, wallet, 5000))
	bet := e.exec(svc, operation("bet-f", "", domain.KindBet, wallet, 5000))
	if bet.Transaction.FailureCode() != domain.FailureInsufficientFundsBet {
		t.Fatalf("bet should be rejected for funds: %+v", bet.Transaction.State())
	}
	w := e.worker(e.store, svc, "w", 10)
	e.makeDue("refund-f")
	if st := e.runOnce(w); st.Rejected != 1 {
		t.Fatalf("%+v", st)
	}
	if r := e.row("refund-f"); r.Status != "REJECTED" || r.Failure != string(domain.FailureReferenceFailed) {
		t.Fatalf("row %+v", r)
	}
	if e.events("refund-f", "WagerTransactionRejected") != 1 || e.ledger("refund-f") != 0 {
		t.Fatal("expected one rejection event and no ledger entry")
	}
}

func TestCompetingWorkersApplyEachOperationOnce(t *testing.T) {
	e := newEnv(t)
	policy := e.policy(0, 24*time.Hour)
	const n = 40
	wallet := e.wallet(100000)
	svc := e.service(e.store, policy)
	for i := 0; i < n; i++ {
		e.exec(svc, operation(fmt.Sprintf("refund-%d", i), fmt.Sprintf("bet-%d", i), domain.KindRefund, wallet, 100))
	}
	for i := 0; i < n; i++ {
		e.exec(svc, operation(fmt.Sprintf("bet-%d", i), "", domain.KindBet, wallet, 100))
	}
	e.makeAllDue()

	const workers = 4
	var mu sync.Mutex
	var total reference.Stats
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s := e.open() // separate pool per instance
			w := e.worker(s, e.service(s, policy), fmt.Sprintf("instance-%d", i), 3)
			for idle := 0; idle < 3; {
				st, err := w.RunOnce(context.Background())
				if err != nil {
					t.Error(err)
					return
				}
				mu.Lock()
				total.Claimed += st.Claimed
				total.Resolved += st.Resolved
				total.Stale += st.Stale
				total.Errors += st.Errors
				total.Pending += st.Pending
				mu.Unlock()
				if st.Claimed == 0 {
					idle++
					time.Sleep(10 * time.Millisecond)
				}
			}
		}(i)
	}
	wg.Wait()

	if total.Claimed != n || total.Resolved != n || total.Stale != 0 || total.Errors != 0 || total.Pending != 0 {
		t.Fatalf("each row must be claimed and resolved exactly once: %+v", total)
	}
	if bal, _ := e.balance(wallet); bal != 100000 {
		t.Fatalf("balance %d, want 100000", bal)
	}
	if got := e.count(`SELECT count(*) FROM ledger_entries`); got != 1+2*n {
		t.Fatalf("ledger entries %d, want %d", got, 1+2*n)
	}
	if got := e.count(`SELECT count(*) FROM wager_transactions WHERE kind='REFUND' AND status='PROCESSED'`); got != n {
		t.Fatalf("processed refunds %d", got)
	}
	if got := e.count(`SELECT count(*) FROM wager_transactions WHERE lease_owner IS NOT NULL`); got != 0 {
		t.Fatalf("%d leases left behind", got)
	}
}

func TestStaleClaimDoesNotDoubleApply(t *testing.T) {
	e := newEnv(t)
	policy := e.policy(0, 24*time.Hour)
	wallet := e.wallet(10000)
	svc := e.service(e.store, policy)
	pending := e.exec(svc, operation("refund-s", "bet-s", domain.KindRefund, wallet, 300))
	e.exec(svc, operation("bet-s", "", domain.KindBet, wallet, 300))

	// Two workers both hold the id (for example a lease expired mid-flight).
	const racers = 6
	results := make(chan wagering.Result, racers)
	var wg sync.WaitGroup
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := svc.RetryPending(context.Background(), pending.Transaction.ID())
			if err != nil {
				t.Error(err)
				return
			}
			results <- res
		}()
	}
	wg.Wait()
	close(results)
	applied := 0
	for r := range results {
		if !r.Replay {
			applied++
		}
	}
	if applied != 1 {
		t.Fatalf("%d racers applied the operation, want 1", applied)
	}
	if e.ledger("refund-s") != 1 {
		t.Fatal("refund ledger entry must be unique")
	}
	if bal, _ := e.balance(wallet); bal != 10000 {
		t.Fatalf("balance %d", bal)
	}
}

func TestLeaseBlocksCompetitorUntilExpiry(t *testing.T) {
	e := newEnv(t)
	wallet := e.wallet(1000)
	e.exec(e.service(e.store, e.policy(0, 24*time.Hour)), operation("refund-l", "nobody", domain.KindRefund, wallet, 100))
	e.makeDue("refund-l")
	ctx := context.Background()

	// Instance A claims and "crashes" without processing.
	claimed, err := e.store.ClaimTransactions(ctx, "instance-a", 10, 400*time.Millisecond)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("A claim: %v %v", claimed, err)
	}
	other := e.open()
	if got, err := other.ClaimTransactions(ctx, "instance-b", 10, time.Minute); err != nil || len(got) != 0 {
		t.Fatalf("B must not claim a live lease: %v %v", got, err)
	}
	time.Sleep(500 * time.Millisecond)
	got, err := other.ClaimTransactions(ctx, "instance-b", 10, time.Minute)
	if err != nil || len(got) != 1 || got[0] != claimed[0] {
		t.Fatalf("B must take over after the lease expired: %v %v", got, err)
	}
}

func TestWorkerLifecycleResolvesInBackground(t *testing.T) {
	e := newEnv(t)
	policy := e.policy(0, 24*time.Hour)
	wallet := e.wallet(5000)
	svc := e.service(e.store, policy)
	e.exec(svc, operation("refund-bg", "bet-bg", domain.KindRefund, wallet, 200))
	e.exec(svc, operation("bet-bg", "", domain.KindBet, wallet, 200))
	e.makeDue("refund-bg")

	w := e.worker(e.store, svc, "bg", 10)
	w.Start()
	deadline := time.Now().Add(5 * time.Second)
	for e.row("refund-bg").Status != "PROCESSED" {
		if time.Now().After(deadline) {
			t.Fatal("background worker did not resolve the refund")
		}
		time.Sleep(20 * time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := w.Stop(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestFxModuleStartsResolvesAndStops(t *testing.T) {
	e := newEnv(t)
	policy := e.policy(0, 24*time.Hour)
	wallet := e.wallet(5000)
	svc := e.service(e.store, policy)
	e.exec(svc, operation("refund-fx", "bet-fx", domain.KindRefund, wallet, 200))
	e.exec(svc, operation("bet-fx", "", domain.KindBet, wallet, 200))
	e.makeDue("refund-fx")

	t.Setenv("REFERENCE_WORKER_POLL_INTERVAL", "20ms")
	app := fxtest.New(t, fx.NopLogger, reference.Module, fx.Supply(e.store))
	app.RequireStart()
	deadline := time.Now().Add(5 * time.Second)
	for e.row("refund-fx").Status != "PROCESSED" {
		if time.Now().After(deadline) {
			t.Fatal("Fx-managed worker did not resolve the refund")
		}
		time.Sleep(20 * time.Millisecond)
	}
	app.RequireStop()
}
