package wagering_test

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"wagering/internal/contract"
	"wagering/internal/domain"
	"wagering/internal/storage/pg"
	wager "wagering/internal/usecase/wagering"
	walletuse "wagering/internal/usecase/wallet"
	"wagering/migrations"
)

func store(t *testing.T) *pg.Store {
	t.Helper()
	admin := os.Getenv("WAGERING_TEST_ADMIN_URL")
	if admin == "" {
		t.Skip("set WAGERING_TEST_ADMIN_URL for PostgreSQL integration test")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, admin)
	if err != nil {
		t.Fatal(err)
	}
	db := "wagering_uc_" + strings.ReplaceAll(domain.NewUUID().String(), "-", "")
	if _, err = conn.Exec(ctx, "CREATE DATABASE "+db); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, e := conn.Exec(ctx, "DROP DATABASE IF EXISTS "+db+" WITH (FORCE)")
		if e != nil {
			t.Errorf("drop DB: %v", e)
		}
		conn.Close(ctx)
	})
	u, err := url.Parse(admin)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + db
	s, err := pg.Open(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
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
		if _, err = s.Pool.Exec(ctx, string(sql)); err != nil {
			t.Fatalf("migration %s: %v", name, err)
		}
	}
	return s
}
func amount(n int64) domain.Money { m, _ := domain.NewMoney(n, "BRL"); return m }
func op(id, ref string, kind domain.Kind, wallet domain.UUID, money domain.Money) contract.Operation {
	return contract.Operation{ProviderID: "provider-a", ExternalTransactionID: id, PlayerID: "player", WalletID: wallet, RoundID: "round", GameID: "game", Kind: kind, Money: money, ReferenceExternalTransactionID: ref}
}
func TestFinancialCommitReplayAndReferences(t *testing.T) {
	s := store(t)
	ctx := context.Background()
	wallets := walletuse.New(s)
	opened, err := wallets.Open(ctx, "player", amount(10000), "opening")
	if err != nil {
		t.Fatal(err)
	}
	svc := wager.New(s)
	walletID := opened.Wallet.ID()
	run := func(id, ref string, kind domain.Kind, money domain.Money) wager.Result {
		t.Helper()
		out, e := svc.Execute(ctx, op(id, ref, kind, walletID, money), "key-"+id, id, nil)
		if e != nil {
			t.Fatalf("%s: %v", id, e)
		}
		return out
	}
	bet := run("bet", "", domain.KindBet, amount(1000))
	if bet.Transaction.Status() != domain.StatusProcessed || bet.Transaction.ResultBalance().Minor() != 9000 {
		t.Fatalf("bet: %+v", bet.Transaction.State())
	}
	win := run("win", "bet", domain.KindWin, amount(2000))
	if win.Transaction.ResultBalance().Minor() != 11000 {
		t.Fatalf("win balance: %+v", win.Transaction.State())
	}
	replay, err := svc.Execute(ctx, op("bet", "", domain.KindBet, walletID, amount(1000)), "key-bet", "repeat", nil)
	if err != nil || !replay.Replay || replay.Transaction.ResultBalance().Minor() != 9000 {
		t.Fatalf("replay: %+v %v", replay, err)
	}
	loss := run("loss", "", domain.KindLoss, amount(0))
	if loss.Transaction.ResultBalance().Minor() != 11000 || *loss.WalletVersion != 3 {
		t.Fatalf("loss: %+v", loss)
	}
	refund := run("refund", "bet", domain.KindRefund, amount(1000))
	if refund.Transaction.ResultBalance().Minor() != 12000 {
		t.Fatalf("refund: %+v", refund)
	}
	rejected := run("rollback-bet", "bet", domain.KindRollback, amount(1000))
	if rejected.Transaction.Status() != domain.StatusRejected || rejected.Transaction.FailureCode() != domain.FailureAlreadyReversed {
		t.Fatalf("reversal collision: %+v", rejected.Transaction.State())
	}
	rollback := run("rollback-refund", "refund", domain.KindRollback, amount(1000))
	if rollback.Transaction.ResultBalance().Minor() != 11000 {
		t.Fatalf("rollback: %+v", rollback.Transaction.State())
	}
	if err := s.WithTx(ctx, func(tx *pg.Tx) error {
		w, e := tx.GetWallet(ctx, walletID)
		if e != nil {
			return e
		}
		if w.Balance().Minor() != 11000 || w.Version() != 5 {
			return fmt.Errorf("wallet %s version %d", w.Balance(), w.Version())
		}
		entries, e := tx.ListLedger(ctx, walletID)
		if e != nil {
			return e
		}
		if len(entries) != 5 {
			return fmt.Errorf("ledger count %d", len(entries))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
func TestPendingReferenceAndInboxReplay(t *testing.T) {
	s := store(t)
	ctx := context.Background()
	opened, err := walletuse.New(s).Open(ctx, "player", amount(10000), "opening")
	if err != nil {
		t.Fatal(err)
	}
	wid := opened.Wallet.ID()
	svc := wager.New(s)
	refundOp := op("refund", "future-bet", domain.KindRefund, wid, amount(500))
	inbox := &wager.Inbox{Consumer: "inbound", MessageID: "msg-1", PayloadHash: strings.Repeat("a", 64)}
	pending, err := svc.Execute(ctx, refundOp, "key-refund", "refund", inbox)
	if err != nil || pending.Transaction.Status() != domain.StatusPendingReference {
		t.Fatalf("pending: %+v %v", pending, err)
	}
	again, err := svc.Execute(ctx, refundOp, "key-refund", "refund", inbox)
	if err != nil || !again.Replay || again.Transaction.ID() != pending.Transaction.ID() {
		t.Fatalf("inbox replay: %+v %v", again, err)
	}
	bad := *inbox
	bad.PayloadHash = strings.Repeat("b", 64)
	if _, err = svc.Execute(ctx, refundOp, "key-refund", "refund", &bad); !errors.Is(err, domain.ErrInboxHashConflict) {
		t.Fatalf("hash conflict: %v", err)
	}
	_, err = svc.Execute(ctx, op("future-bet", "", domain.KindBet, wid, amount(500)), "key-future-bet", "bet", nil)
	if err != nil {
		t.Fatal(err)
	}
	// A retry needs a live lease on a due row: without a claim it is refused.
	if _, err = svc.RetryPending(ctx, pending.Transaction.ID(), "worker-a"); !errors.Is(err, wager.ErrClaimNotHeld) {
		t.Fatalf("unclaimed retry: %v", err)
	}
	if _, err = s.Pool.Exec(ctx, `UPDATE wager_transactions SET next_attempt_at=now()-interval '1 second' WHERE id=$1`, pending.Transaction.ID().String()); err != nil {
		t.Fatal(err)
	}
	if claimed, e := s.ClaimTransactions(ctx, "worker-a", 10, time.Minute); e != nil || len(claimed) != 1 {
		t.Fatalf("claim: %v %v", claimed, e)
	}
	result, err := svc.RetryPending(ctx, pending.Transaction.ID(), "worker-a")
	if err != nil || result.Transaction.Status() != domain.StatusProcessed || result.Transaction.ResultBalance().Minor() != 10000 {
		t.Fatalf("resolved: %+v %v", result, err)
	}
}
func TestDuplicateConcurrentBet(t *testing.T) {
	s := store(t)
	ctx := context.Background()
	opened, err := walletuse.New(s).Open(ctx, "player", amount(10000), "opening")
	if err != nil {
		t.Fatal(err)
	}
	svc := wager.New(s)
	operation := op("same", "", domain.KindBet, opened.Wallet.ID(), amount(1000))
	const n = 16
	var wg sync.WaitGroup
	results := make(chan wager.Result, n)
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, e := svc.Execute(ctx, operation, "one-key", "same", nil)
			if e != nil {
				errs <- e
			} else {
				results <- out
			}
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	for e := range errs {
		t.Errorf("concurrent execute: %v", e)
	}
	var first domain.UUID
	for result := range results {
		if first.IsNil() {
			first = result.Transaction.ID()
		} else if first != result.Transaction.ID() {
			t.Errorf("different transaction IDs")
		}
	}
	if err := s.WithTx(ctx, func(tx *pg.Tx) error {
		w, e := tx.GetWallet(ctx, opened.Wallet.ID())
		if e != nil {
			return e
		}
		if w.Balance().Minor() != 9000 {
			return fmt.Errorf("balance %s", w.Balance())
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentHTTPAndSQSReplayCompletesInbox(t *testing.T) {
	s := store(t)
	ctx := context.Background()
	opened, err := walletuse.New(s).Open(ctx, "player", amount(10000), "opening")
	if err != nil {
		t.Fatal(err)
	}
	svc := wager.New(s)
	operation := op("cross-transport", "", domain.KindBet, opened.Wallet.ID(), amount(1000))
	inbox := &wager.Inbox{Consumer: "inbound", MessageID: "sqs-cross-transport", PayloadHash: strings.Repeat("a", 64)}
	type response struct {
		result wager.Result
		err    error
	}
	start := make(chan struct{})
	httpResult, sqsResult := make(chan response, 1), make(chan response, 1)
	go func() {
		<-start
		result, e := svc.Execute(ctx, operation, "cross-transport-key", "http", nil)
		httpResult <- response{result, e}
	}()
	go func() {
		<-start
		result, e := svc.Execute(ctx, operation, "cross-transport-key", "sqs", inbox)
		sqsResult <- response{result, e}
	}()
	close(start)
	http, sqs := <-httpResult, <-sqsResult
	if http.err != nil || sqs.err != nil {
		t.Fatalf("HTTP error=%v, SQS error=%v", http.err, sqs.err)
	}
	if http.result.Transaction.ID() != sqs.result.Transaction.ID() || http.result.Replay == sqs.result.Replay {
		t.Fatalf("HTTP and SQS must share one result, with one replay: HTTP=%+v SQS=%+v", http.result, sqs.result)
	}
	if err := s.WithTx(ctx, func(tx *pg.Tx) error {
		stored, linked, e := tx.GetInbox(ctx, inbox.Consumer, inbox.MessageID)
		if e != nil {
			return e
		}
		if !stored.IsCompleted() || stored.RequestHash() != inbox.PayloadHash || linked == nil || *linked != sqs.result.Transaction.ID() {
			return fmt.Errorf("inbox not completed for winner: completed=%t linked=%v", stored.IsCompleted(), linked)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var transactions, movements int
	if err := s.Pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM wager_transactions WHERE external_transaction_id='cross-transport'), (SELECT count(*) FROM ledger_entries WHERE transaction_id=$1)`, sqs.result.Transaction.ID().String()).Scan(&transactions, &movements); err != nil {
		t.Fatal(err)
	}
	if transactions != 1 || movements != 1 {
		t.Fatalf("transactions=%d financial movements=%d", transactions, movements)
	}
	again, err := svc.Execute(ctx, operation, "cross-transport-key", "sqs-replay", inbox)
	if err != nil || !again.Replay || again.Transaction.ID() != sqs.result.Transaction.ID() {
		t.Fatalf("SQS replay: %+v %v", again, err)
	}
	changed := *inbox
	changed.PayloadHash = strings.Repeat("b", 64)
	if _, err := svc.Execute(ctx, operation, "cross-transport-key", "sqs-conflict", &changed); !errors.Is(err, domain.ErrInboxHashConflict) {
		t.Fatalf("reused message ID with different hash: %v", err)
	}
	changedOperation := operation
	changedOperation.Money = amount(2000)
	otherInbox := &wager.Inbox{Consumer: inbox.Consumer, MessageID: "sqs-changed-operation", PayloadHash: strings.Repeat("c", 64)}
	if _, err := svc.Execute(ctx, changedOperation, "cross-transport-key", "hash-conflict", otherInbox); !errors.Is(err, domain.ErrHashMismatch) {
		t.Fatalf("reused key with different operation: %v", err)
	}
	otherInbox.MessageID = "sqs-changed-key"
	if _, err := svc.Execute(ctx, operation, "another-key", "external-conflict", otherInbox); !errors.Is(err, wager.ErrExternalIDConflict) {
		t.Fatalf("reused external ID with another key: %v", err)
	}
	var conflictingInboxRows int
	if err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM inbox_messages WHERE consumer=$1 AND message_id IN ('sqs-changed-operation','sqs-changed-key')`, inbox.Consumer).Scan(&conflictingInboxRows); err != nil {
		t.Fatal(err)
	}
	if conflictingInboxRows != 0 {
		t.Fatalf("conflicting deliveries wrote %d inbox rows", conflictingInboxRows)
	}
	if err := s.WithTx(ctx, func(tx *pg.Tx) error {
		wallet, e := tx.GetWallet(ctx, opened.Wallet.ID())
		if e != nil {
			return e
		}
		if wallet.Balance().Minor() != 9000 || wallet.Version() != 2 {
			return fmt.Errorf("wallet balance=%s version=%d", wallet.Balance(), wallet.Version())
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestCompetingReversals(t *testing.T) {
	s := store(t)
	ctx := context.Background()
	opened, err := walletuse.New(s).Open(ctx, "player", amount(10000), "opening")
	if err != nil {
		t.Fatal(err)
	}
	svc := wager.New(s)
	wid := opened.Wallet.ID()
	bet := op("bet", "", domain.KindBet, wid, amount(2500))
	if _, err := svc.Execute(ctx, bet, "bet-key", "bet", nil); err != nil {
		t.Fatal(err)
	}
	operations := []contract.Operation{op("refund", "bet", domain.KindRefund, wid, amount(2500)), op("rollback", "bet", domain.KindRollback, wid, amount(2500))}
	outcomes := make(chan domain.Status, 2)
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for i, operation := range operations {
		wg.Add(1)
		go func(i int, operation contract.Operation) {
			defer wg.Done()
			result, e := svc.Execute(ctx, operation, fmt.Sprintf("key-%d", i), operation.ExternalTransactionID, nil)
			if e != nil {
				errs <- e
			} else {
				outcomes <- result.Transaction.Status()
			}
		}(i, operation)
	}
	wg.Wait()
	close(outcomes)
	close(errs)
	for e := range errs {
		t.Error(e)
	}
	processed, rejected := 0, 0
	for status := range outcomes {
		if status == domain.StatusProcessed {
			processed++
		} else if status == domain.StatusRejected {
			rejected++
		}
	}
	if processed != 1 || rejected != 1 {
		t.Fatalf("outcomes processed=%d rejected=%d", processed, rejected)
	}
	if err := s.WithTx(ctx, func(tx *pg.Tx) error {
		w, e := tx.GetWallet(ctx, wid)
		if e != nil {
			return e
		}
		if w.Balance().Minor() != 10000 {
			return fmt.Errorf("balance %s", w.Balance())
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
