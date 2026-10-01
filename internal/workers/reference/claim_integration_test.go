package reference_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"wagering/internal/domain"
	wagering "wagering/internal/usecase/wagering"
	"wagering/internal/workers/reference"
)

// claimState is the full retry-control state of one row: what a refused retry
// must leave untouched.
type claimState struct {
	Status   string
	Attempts int
	Next     time.Time
	Owner    string
}

func (e *env) claimState(external string) claimState {
	e.t.Helper()
	var c claimState
	var owner *string
	err := e.store.Pool.QueryRow(context.Background(), `SELECT status, attempt_count, next_attempt_at, lease_owner FROM wager_transactions WHERE external_transaction_id=$1`, external).Scan(&c.Status, &c.Attempts, &c.Next, &owner)
	if err != nil {
		e.t.Fatal(err)
	}
	if owner != nil {
		c.Owner = *owner
	}
	return c
}

func (e *env) noEffects(external string) {
	e.t.Helper()
	if e.events(external, "WagerTransactionRejected") != 0 || e.events(external, "WagerTransactionProcessed") != 0 || e.ledger(external) != 0 {
		e.t.Fatalf("%s: unexpected rejection/processed event or ledger entry", external)
	}
}

func (e *env) outboxCount() int { return e.count(`SELECT count(*) FROM outbox_events`) }

// An owner whose lease expired and was taken over by a second worker must not
// consume the attempt budget, move the schedule or steal the new owner's lease.
func TestExpiredLeaseOwnerCannotConsumeAttemptBudget(t *testing.T) {
	e := newEnv(t)
	policy := e.policy(3, 24*time.Hour)
	wallet := e.wallet(10000)
	svcA := e.service(e.store, policy)
	storeB := e.open()
	svcB := e.service(storeB, policy)
	ctx := context.Background()
	e.exec(svcA, operation("refund-e", "never", domain.KindRefund, wallet, 500))
	e.makeDue("refund-e")

	claimed, err := e.store.ClaimTransactions(ctx, "worker-a", 10, 300*time.Millisecond)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("A claim: %v %v", claimed, err)
	}
	id := claimed[0]
	time.Sleep(450 * time.Millisecond) // A stalls past its lease
	if got, err := storeB.ClaimTransactions(ctx, "worker-b", 10, time.Minute); err != nil || len(got) != 1 || got[0] != id {
		t.Fatalf("B takeover: %v %v", got, err)
	}
	before, events := e.claimState("refund-e"), e.outboxCount()
	if before.Owner != "worker-b" || before.Attempts != 1 {
		t.Fatalf("state before stale call: %+v", before)
	}

	for i := 0; i < 3; i++ { // A wakes up and tries repeatedly
		if _, err = svcA.RetryPending(ctx, id, "worker-a"); !errors.Is(err, wagering.ErrClaimNotHeld) {
			t.Fatalf("stale owner A: %v", err)
		}
	}
	if after := e.claimState("refund-e"); after != before {
		t.Fatalf("stale owner changed the row: %+v -> %+v", before, after)
	}
	if e.outboxCount() != events {
		t.Fatal("stale owner wrote an outbox event")
	}
	e.noEffects("refund-e")

	// B, the live owner, consumes exactly one attempt and reschedules.
	res, err := svcB.RetryPending(ctx, id, "worker-b")
	if err != nil || res.Replay || res.Transaction.Status() != domain.StatusPendingReference {
		t.Fatalf("B attempt: %+v %v", res, err)
	}
	afterB := e.claimState("refund-e")
	if afterB.Attempts != 2 || afterB.Owner != "" || !afterB.Next.After(before.Next) {
		t.Fatalf("B should have used one attempt and rescheduled: %+v", afterB)
	}
	// A's late retry still changes nothing: the lease is gone and the row is not due.
	if _, err = svcA.RetryPending(ctx, id, "worker-a"); !errors.Is(err, wagering.ErrClaimNotHeld) {
		t.Fatalf("A after B's attempt: %v", err)
	}
	if again := e.claimState("refund-e"); again != afterB {
		t.Fatalf("late stale call changed the row: %+v -> %+v", afterB, again)
	}

	// The budget (3) is still intact: attempt 3 is the last and exhausts it.
	e.makeDue("refund-e")
	w := e.worker(storeB, svcB, "worker-b", 10)
	if st := e.runOnce(w); st.Rejected != 1 {
		t.Fatalf("third attempt should exhaust the budget: %+v", st)
	}
	if r := e.row("refund-e"); r.Status != "REJECTED" || r.Failure != string(domain.FailureReferenceNotFound) {
		t.Fatalf("row %+v", r)
	}
	if e.events("refund-e", "WagerTransactionRejected") != 1 {
		t.Fatal("exactly one rejection event expected")
	}
}

// A retry before next_attempt_at must not count, reschedule or reject, with or
// without a lease, even once the TTL has already passed.
func TestEarlyRetryCannotConsumeAttemptBudget(t *testing.T) {
	e := newEnv(t)
	policy := e.policy(3, time.Hour)
	wallet := e.wallet(10000)
	svc := e.service(e.store, policy)
	ctx := context.Background()
	pending := e.exec(svc, operation("refund-early", "never", domain.KindRefund, wallet, 500))
	id := pending.Transaction.ID()
	before, events := e.claimState("refund-early"), e.outboxCount()
	if before.Attempts != 1 || !before.Next.After(time.Now()) {
		t.Fatalf("expected a future schedule: %+v", before)
	}
	e.clock.Advance(2 * time.Hour) // TTL passed: an early retry must not reject either

	// No lease at all.
	if _, err := svc.RetryPending(ctx, id, "worker-a"); !errors.Is(err, wagering.ErrClaimNotHeld) {
		t.Fatalf("unleased early retry: %v", err)
	}
	// A live lease on a row that is not due (as left by a stale claim).
	if _, err := e.store.Pool.Exec(ctx, `UPDATE wager_transactions SET lease_owner='worker-a', lease_expires_at=now()+interval '1 minute' WHERE id=$1`, id.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RetryPending(ctx, id, "worker-a"); !errors.Is(err, wagering.ErrClaimNotHeld) {
		t.Fatalf("leased early retry: %v", err)
	}
	if _, err := svc.RetryPending(ctx, id, "worker-other"); !errors.Is(err, wagering.ErrClaimNotHeld) {
		t.Fatalf("another owner's early retry: %v", err)
	}
	want := before
	want.Owner = "worker-a"
	if after := e.claimState("refund-early"); after != want {
		t.Fatalf("early retry changed the row: %+v -> %+v", want, after)
	}
	if e.outboxCount() != events {
		t.Fatal("early retry wrote an outbox event")
	}
	e.noEffects("refund-early")
	if bal, _ := e.balance(wallet); bal != 10000 {
		t.Fatalf("balance %d", bal)
	}
}

// Two live workers race for one due row: exactly one holds the lease and
// resolves it once; the other never consumes an attempt or touches the wallet.
func TestTwoWorkersDueValidLeaseResolvesOnce(t *testing.T) {
	e := newEnv(t)
	policy := e.policy(2, 24*time.Hour) // a single wasted attempt would exhaust it
	wallet := e.wallet(10000)
	svcA := e.service(e.store, policy)
	storeB := e.open()
	svcB := e.service(storeB, policy)
	ctx := context.Background()
	pending := e.exec(svcA, operation("refund-r", "bet-r", domain.KindRefund, wallet, 300))
	e.exec(svcA, operation("bet-r", "", domain.KindBet, wallet, 300))
	e.makeDue("refund-r")
	id := pending.Transaction.ID()

	a, err := e.store.ClaimTransactions(ctx, "worker-a", 10, time.Minute)
	if err != nil || len(a) != 1 {
		t.Fatalf("A claim: %v %v", a, err)
	}
	if b, err := storeB.ClaimTransactions(ctx, "worker-b", 10, time.Minute); err != nil || len(b) != 0 {
		t.Fatalf("B must not claim A's live lease: %v %v", b, err)
	}

	var wg sync.WaitGroup
	var resA, resB wagering.Result
	var errA, errB error
	wg.Add(2)
	go func() { defer wg.Done(); resA, errA = svcA.RetryPending(ctx, id, "worker-a") }()
	go func() { defer wg.Done(); resB, errB = svcB.RetryPending(ctx, id, "worker-b") }()
	wg.Wait()

	if errA != nil || resA.Replay || resA.Transaction.Status() != domain.StatusProcessed {
		t.Fatalf("lease holder A: %+v %v", resA, errA)
	}
	// B either saw no claim, or waited on the locks and saw the final state.
	if !(errors.Is(errB, wagering.ErrClaimNotHeld) || (errB == nil && resB.Replay)) {
		t.Fatalf("B must have no effect: %+v %v", resB, errB)
	}
	if r := e.row("refund-r"); r.Status != "PROCESSED" || r.Attempts != 1 || r.Lease || r.Next {
		t.Fatalf("row %+v", r)
	}
	if e.ledger("refund-r") != 1 || e.events("refund-r", "WagerTransactionProcessed") != 1 || e.events("refund-r", "WagerTransactionRejected") != 0 {
		t.Fatal("refund must resolve exactly once")
	}
	if bal, _ := e.balance(wallet); bal != 10000 {
		t.Fatalf("balance %d", bal)
	}
}

// staleClaimer hands a worker ids it claimed earlier, after another instance
// took the lease over: the worker must report them stale, not as errors.
type staleClaimer struct{ ids []domain.UUID }

func (c *staleClaimer) ClaimTransactions(context.Context, string, int, time.Duration) ([]domain.UUID, error) {
	ids := c.ids
	c.ids = nil
	return ids, nil
}

func TestWorkerReportsLostClaimAsStaleWithoutSpendingAttempt(t *testing.T) {
	e := newEnv(t)
	policy := e.policy(3, 24*time.Hour)
	wallet := e.wallet(1000)
	svc := e.service(e.store, policy)
	ctx := context.Background()
	e.exec(svc, operation("refund-w", "never", domain.KindRefund, wallet, 100))
	e.makeDue("refund-w")
	ids, err := e.store.ClaimTransactions(ctx, "worker-a", 10, 200*time.Millisecond)
	if err != nil || len(ids) != 1 {
		t.Fatalf("claim: %v %v", ids, err)
	}
	time.Sleep(300 * time.Millisecond)
	if got, err := e.open().ClaimTransactions(ctx, "worker-b", 10, time.Minute); err != nil || len(got) != 1 {
		t.Fatalf("takeover: %v %v", got, err)
	}
	before := e.claimState("refund-w")

	cfg := reference.DefaultConfig()
	cfg.Owner, cfg.Retry, cfg.Lease, cfg.ItemTimeout = "worker-a", policy, 5*time.Second, 3*time.Second
	w, err := reference.NewWorker(cfg, &staleClaimer{ids: ids}, svc, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if st := e.runOnce(w); st.Claimed != 1 || st.Stale != 1 || st.Errors != 0 {
		t.Fatalf("lost claim must be stale: %+v", st)
	}
	if after := e.claimState("refund-w"); after != before {
		t.Fatalf("stale worker changed the row: %+v -> %+v", before, after)
	}
}
