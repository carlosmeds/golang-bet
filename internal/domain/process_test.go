package domain

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func betRef(t testing.TB, kind Kind, amount string, status Status) *Reference {
	t.Helper()
	return &Reference{
		ExternalTransactionID: "ref-1", Kind: kind, Status: status, ProviderID: "prov", PlayerID: "player-1",
		WalletID: id(100), RoundID: "round-1", Money: brl(t, amount),
	}
}

func run(t testing.TB, w *Wallet, tx *WagerTransaction, ref *Reference, now time.Time) ProcessResult {
	t.Helper()
	res, err := Process(ProcessInput{Wallet: w, Tx: tx, Reference: ref, Now: now, CorrelationID: "corr-1", NewID: seqIDs()})
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	return res
}

func eventTypes(res ProcessResult) []EventType {
	var out []EventType
	for _, e := range res.Events {
		out = append(out, e.Meta().EventType)
	}
	return out
}

func sameTypes(a, b []EventType) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestProcessBet(t *testing.T) {
	w := newWallet(t, "100.00")
	tx := newTx(t, KindBet, "30.00", "")
	res := run(t, w, tx, nil, t0.Add(time.Second))
	if res.Outcome != OutcomeProcessed || tx.Status() != StatusProcessed || w.Balance().Amount() != "70.00" || w.Version() != 2 {
		t.Fatalf("outcome %s tx=%+v wallet=%+v", res.Outcome, tx.State(), w.State())
	}
	if tx.ResultBalance().Amount() != "70.00" {
		t.Errorf("result balance %v", tx.ResultBalance())
	}
	l := res.Ledger
	if l == nil || l.Direction() != DirectionDebit || l.BalanceBefore().Amount() != "100.00" || l.BalanceAfter().Amount() != "70.00" || l.TransactionID() != tx.ID() {
		t.Fatalf("ledger %+v", l)
	}
	if !sameTypes(eventTypes(res), []EventType{EventWagerTransactionProcessed, EventWalletBalanceChanged}) {
		t.Errorf("events %v", eventTypes(res))
	}
}

func TestProcessBetInsufficientFundsRejects(t *testing.T) {
	w := newWallet(t, "100.00")
	before := w.State()
	tx := newTx(t, KindBet, "100.01", "")
	res := run(t, w, tx, nil, t0)
	if res.Outcome != OutcomeRejected || tx.Status() != StatusRejected || tx.FailureCode() != FailureInsufficientFundsBet {
		t.Fatalf("%s %+v", res.Outcome, tx.State())
	}
	if res.Ledger != nil || res.Movement != nil || w.State() != before {
		t.Error("rejected BET touched ledger or wallet (REQ-030)")
	}
	if !sameTypes(eventTypes(res), []EventType{EventWagerTransactionRejected}) {
		t.Errorf("events %v", eventTypes(res))
	}
	rej := res.Events[0].(Envelope[WagerTransactionRejectedData])
	if rej.Data.FailureCode != FailureInsufficientFundsBet || rej.Data.ObservedBalance == nil || rej.Data.ObservedBalance.Amount() != "100.00" {
		t.Errorf("rejected data %+v", rej.Data)
	}
}

func TestProcessTwoBetsOf80OnBalance100(t *testing.T) {
	w := newWallet(t, "100.00")
	a, b := newTx(t, KindBet, "80.00", ""), newTx(t, KindBet, "80.00", "")
	if run(t, w, a, nil, t0).Outcome != OutcomeProcessed {
		t.Fatal("first bet")
	}
	if run(t, w, b, nil, t0).Outcome != OutcomeRejected || w.Balance().Amount() != "20.00" {
		t.Fatalf("second bet: %+v", w.State())
	}
}

func TestProcessWin(t *testing.T) {
	w := newWallet(t, "10.00")
	tx := newTx(t, KindWin, "5.25", "")
	res := run(t, w, tx, nil, t0)
	if res.Outcome != OutcomeProcessed || w.Balance().Amount() != "15.25" || w.Version() != 2 || res.Ledger.Direction() != DirectionCredit {
		t.Fatalf("%+v %+v", res, w.State())
	}
}

func TestProcessWinWithBetReference(t *testing.T) {
	ok := func(mut func(*Reference), want FailureCode, wantOutcome Outcome) {
		t.Helper()
		w := newWallet(t, "10.00")
		tx := newTx(t, KindWin, "5.00", "ref-1")
		ref := betRef(t, KindBet, "2.00", StatusProcessed)
		if mut != nil {
			mut(ref)
		}
		res := run(t, w, tx, ref, t0)
		if res.Outcome != wantOutcome || tx.FailureCode() != want {
			t.Errorf("outcome=%s code=%q want %s %q", res.Outcome, tx.FailureCode(), wantOutcome, want)
		}
	}
	ok(nil, "", OutcomeProcessed) // amounts may differ for WIN
	ok(func(r *Reference) { r.RoundID = "other" }, FailureReferenceMismatch, OutcomeRejected)
	ok(func(r *Reference) { r.PlayerID = "other" }, FailureReferenceMismatch, OutcomeRejected)
	ok(func(r *Reference) { r.ProviderID = "other" }, FailureReferenceMismatch, OutcomeRejected)
	ok(func(r *Reference) { r.WalletID = id(999) }, FailureWalletMismatch, OutcomeRejected)
	ok(func(r *Reference) { r.Money, _ = NewMoney(200, "USD") }, FailureCurrencyMismatch, OutcomeRejected)
	ok(func(r *Reference) { r.Kind = KindWin }, FailureReferenceMismatch, OutcomeRejected)
	ok(func(r *Reference) { r.Status = StatusRejected }, FailureReferenceFailed, OutcomeRejected)
	ok(func(r *Reference) { r.Status = StatusFailed }, FailureReferenceFailed, OutcomeRejected)
	ok(func(r *Reference) { r.Status = StatusPending }, "", OutcomePending)
	ok(func(r *Reference) { r.Status = StatusPendingReference }, "", OutcomePending)
}

func TestProcessLoss(t *testing.T) {
	w := newWallet(t, "10.00")
	before := w.State()
	tx := newTx(t, KindLoss, "0.00", "")
	res := run(t, w, tx, nil, t0.Add(time.Hour))
	if res.Outcome != OutcomeProcessed || tx.Status() != StatusProcessed || tx.ResultBalance().Amount() != "10.00" {
		t.Fatalf("%+v", tx.State())
	}
	if res.Ledger != nil || res.Movement != nil {
		t.Error("LOSS wrote a ledger entry")
	}
	if w.Version() != before.Version || w.Balance() != before.Balance || !w.UpdatedAt().Equal(before.UpdatedAt) {
		t.Errorf("LOSS changed the wallet: %+v", w.State())
	}
	if !sameTypes(eventTypes(res), []EventType{EventWagerTransactionProcessed}) {
		t.Errorf("events %v", eventTypes(res))
	}
}

func TestProcessRefund(t *testing.T) {
	w := newWallet(t, "10.00")
	tx := newTx(t, KindRefund, "30.00", "ref-1")
	res := run(t, w, tx, betRef(t, KindBet, "30.00", StatusProcessed), t0)
	if res.Outcome != OutcomeProcessed || w.Balance().Amount() != "40.00" || res.Ledger.Direction() != DirectionCredit {
		t.Fatalf("%s %+v", res.Outcome, w.State())
	}
}

func TestProcessRefundRules(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*Reference)
		want FailureCode
	}{
		{"amount differs", func(r *Reference) { r.Money = brl(t, "29.99") }, FailureReferenceMismatch},
		{"ref is WIN", func(r *Reference) { r.Kind = KindWin }, FailureReferenceMismatch},
		{"ref is REFUND", func(r *Reference) { r.Kind = KindRefund }, FailureReferenceMismatch},
		{"ref is LOSS", func(r *Reference) { r.Kind = KindLoss }, FailureReferenceMismatch},
		{"ref rejected", func(r *Reference) { r.Status = StatusRejected }, FailureReferenceFailed},
		{"already refunded", func(r *Reference) { r.Reversals.Refunded = true }, FailureAlreadyReversed},
		{"already rolled back", func(r *Reference) { r.Reversals.RolledBack = true }, FailureAlreadyReversed},
		{"other wallet", func(r *Reference) { r.WalletID = id(7) }, FailureWalletMismatch},
		{"other round", func(r *Reference) { r.RoundID = "r2" }, FailureReferenceMismatch},
	}
	for _, c := range cases {
		w := newWallet(t, "10.00")
		before := w.State()
		tx := newTx(t, KindRefund, "30.00", "ref-1")
		ref := betRef(t, KindBet, "30.00", StatusProcessed)
		c.mut(ref)
		res := run(t, w, tx, ref, t0)
		if res.Outcome != OutcomeRejected || tx.FailureCode() != c.want || w.State() != before || res.Ledger != nil {
			t.Errorf("%s: outcome=%s code=%s", c.name, res.Outcome, tx.FailureCode())
		}
	}
}

func TestProcessRollbackOfBetCredits(t *testing.T) {
	w := newWallet(t, "70.00")
	tx := newTx(t, KindRollback, "30.00", "ref-1")
	res := run(t, w, tx, betRef(t, KindBet, "30.00", StatusProcessed), t0)
	if res.Outcome != OutcomeProcessed || w.Balance().Amount() != "100.00" || res.Ledger.Direction() != DirectionCredit {
		t.Fatalf("%s %+v", res.Outcome, w.State())
	}
}

func TestProcessRollbackOfWinAndRefundDebits(t *testing.T) {
	for _, kind := range []Kind{KindWin, KindRefund} {
		w := newWallet(t, "100.00")
		tx := newTx(t, KindRollback, "40.00", "ref-1")
		res := run(t, w, tx, betRef(t, kind, "40.00", StatusProcessed), t0)
		if res.Outcome != OutcomeProcessed || w.Balance().Amount() != "60.00" || res.Ledger.Direction() != DirectionDebit {
			t.Errorf("rollback of %s: %s %+v", kind, res.Outcome, w.State())
		}
	}
}

func TestProcessRollbackInsufficientFundsHasDistinctCode(t *testing.T) {
	w := newWallet(t, "10.00")
	before := w.State()
	tx := newTx(t, KindRollback, "40.00", "ref-1")
	res := run(t, w, tx, betRef(t, KindWin, "40.00", StatusProcessed), t0)
	if res.Outcome != OutcomeRejected || tx.FailureCode() != FailureInsufficientFundsReversal || w.State() != before || res.Ledger != nil {
		t.Fatalf("%s %s", res.Outcome, tx.FailureCode())
	}
	if FailureInsufficientFundsReversal == FailureInsufficientFundsBet {
		t.Fatal("codes must differ (REQ-041)")
	}
}

func TestProcessReversalExclusion(t *testing.T) {
	type tc struct {
		name string
		kind Kind // referenced kind
		rev  ReversalState
		want FailureCode
	}
	for _, c := range []tc{
		{"rollback of refunded BET", KindBet, ReversalState{Refunded: true}, FailureAlreadyReversed},
		{"rollback of rolled back BET", KindBet, ReversalState{RolledBack: true}, FailureAlreadyReversed},
		{"second rollback of WIN", KindWin, ReversalState{RolledBack: true}, FailureAlreadyReversed},
		{"second rollback of REFUND", KindRefund, ReversalState{RolledBack: true}, FailureAlreadyReversed},
		{"rollback of WIN (refund flag irrelevant)", KindWin, ReversalState{Refunded: true}, ""},
	} {
		w := newWallet(t, "100.00")
		tx := newTx(t, KindRollback, "10.00", "ref-1")
		ref := betRef(t, c.kind, "10.00", StatusProcessed)
		ref.Reversals = c.rev
		res := run(t, w, tx, ref, t0)
		if c.want == "" && res.Outcome != OutcomeProcessed || tx.FailureCode() != c.want {
			t.Errorf("%s: %s %q", c.name, res.Outcome, tx.FailureCode())
		}
	}
	// A rollback of a REFUND does not reopen a refund of the BET: the BET's
	// own reversal state still says Refunded.
	w := newWallet(t, "100.00")
	tx := newTx(t, KindRefund, "10.00", "ref-1")
	ref := betRef(t, KindBet, "10.00", StatusProcessed)
	ref.Reversals = ReversalState{Refunded: true, RolledBack: false}
	if res := run(t, w, tx, ref, t0); res.Outcome != OutcomeRejected || tx.FailureCode() != FailureAlreadyReversed {
		t.Errorf("refund after refund: %s", tx.FailureCode())
	}
}

func TestProcessRollbackRules(t *testing.T) {
	cases := map[string]func(*Reference){
		"ref LOSS":         func(r *Reference) { r.Kind = KindLoss },
		"ref ROLLBACK":     func(r *Reference) { r.Kind = KindRollback },
		"amount differs":   func(r *Reference) { r.Money = brl(t, "1.00") },
		"provider differs": func(r *Reference) { r.ProviderID = "x" },
	}
	for name, mut := range cases {
		w := newWallet(t, "100.00")
		tx := newTx(t, KindRollback, "10.00", "ref-1")
		ref := betRef(t, KindBet, "10.00", StatusProcessed)
		mut(ref)
		if res := run(t, w, tx, ref, t0); res.Outcome != OutcomeRejected || tx.FailureCode() != FailureReferenceMismatch {
			t.Errorf("%s: %s %q", name, res.Outcome, tx.FailureCode())
		}
	}
}

func TestProcessWalletAndCurrencyMismatch(t *testing.T) {
	w := newWallet(t, "100.00")
	p := params(KindBet, "1.00", "")
	p.WalletID = id(555)
	other, _ := NewExternalTransaction(p)
	if res := run(t, w, other, nil, t0); res.Outcome != OutcomeRejected || other.FailureCode() != FailureWalletMismatch {
		t.Errorf("wallet mismatch: %s", other.FailureCode())
	}
	p = params(KindBet, "1.00", "")
	p.PlayerID = "someone-else"
	other, _ = NewExternalTransaction(p)
	if res := run(t, w, other, nil, t0); res.Outcome != OutcomeRejected || other.FailureCode() != FailureWalletMismatch {
		t.Errorf("player mismatch: %s", other.FailureCode())
	}
	p = params(KindBet, "1.00", "")
	p.Money, _ = NewMoney(100, "USD")
	other, _ = NewExternalTransaction(p)
	if res := run(t, w, other, nil, t0); res.Outcome != OutcomeRejected || other.FailureCode() != FailureCurrencyMismatch {
		t.Errorf("currency mismatch: %s", other.FailureCode())
	}
	if w.Version() != 1 || w.Balance().Amount() != "100.00" {
		t.Error("mismatches changed the wallet")
	}
}

func TestProcessMissingReferenceWaitsThenRejects(t *testing.T) {
	policy := RetryPolicy{BaseDelay: time.Second, MaxDelay: 4 * time.Second, TTL: time.Hour, MaxAttempts: 3}
	w := newWallet(t, "100.00")
	before := w.State()
	tx := newTx(t, KindRefund, "10.00", "ref-1")
	step := func(now time.Time) ProcessResult {
		res, err := Process(ProcessInput{Wallet: w, Tx: tx, Now: now, Retry: policy, CorrelationID: "c", NewID: seqIDs()})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}

	res := step(t0)
	if res.Outcome != OutcomePending || tx.Status() != StatusPendingReference || tx.AttemptCount() != 1 ||
		!sameTypes(eventTypes(res), []EventType{EventWagerTransactionPendingReference}) {
		t.Fatalf("first: %s %+v %v", res.Outcome, tx.State(), eventTypes(res))
	}
	pe := res.Events[0].(Envelope[WagerTransactionPendingReferenceData])
	if pe.Data.AttemptCount != 1 || !pe.Data.NextAttemptAt.Equal(t0.Add(time.Second)) {
		t.Errorf("pending event %+v", pe.Data)
	}

	res = step(t0.Add(time.Second))
	if res.Outcome != OutcomePending || tx.AttemptCount() != 2 || len(res.Events) != 0 || !tx.NextAttemptAt().Equal(t0.Add(3*time.Second)) {
		t.Fatalf("second: %s %+v events=%d", res.Outcome, tx.State(), len(res.Events))
	}

	res = step(t0.Add(3 * time.Second)) // third unresolved lookup exhausts MaxAttempts=3
	if res.Outcome != OutcomeRejected || tx.FailureCode() != FailureReferenceNotFound || !sameTypes(eventTypes(res), []EventType{EventWagerTransactionRejected}) {
		t.Fatalf("third: %s %+v", res.Outcome, tx.State())
	}
	if w.State() != before {
		t.Error("waiting changed the wallet")
	}
}

func TestProcessPendingReferenceExpiresByTTLWithUnresolvedCode(t *testing.T) {
	policy := RetryPolicy{BaseDelay: time.Second, MaxDelay: time.Minute, TTL: time.Hour}
	w := newWallet(t, "100.00")
	tx := newTx(t, KindRollback, "10.00", "ref-1")
	ref := betRef(t, KindBet, "10.00", StatusPending)
	in := ProcessInput{Wallet: w, Tx: tx, Reference: ref, Now: t0, Retry: policy, CorrelationID: "c", NewID: seqIDs()}
	if res, err := Process(in); err != nil || res.Outcome != OutcomePending {
		t.Fatalf("%v %v", res.Outcome, err)
	}
	in.Now = t0.Add(time.Hour)
	res, err := Process(in)
	if err != nil || res.Outcome != OutcomeRejected || tx.FailureCode() != FailureReferenceUnresolved {
		t.Fatalf("%v %v %q", res.Outcome, err, tx.FailureCode())
	}
}

func TestProcessPendingReferenceResolvesOutOfOrder(t *testing.T) {
	w := newWallet(t, "70.00")
	tx := newTx(t, KindRefund, "30.00", "ref-1")
	if res := run(t, w, tx, nil, t0); res.Outcome != OutcomePending {
		t.Fatal(res.Outcome)
	}
	res := run(t, w, tx, betRef(t, KindBet, "30.00", StatusProcessed), t0.Add(time.Minute))
	if res.Outcome != OutcomeProcessed || tx.Status() != StatusProcessed || w.Balance().Amount() != "100.00" {
		t.Fatalf("%s %+v", res.Outcome, tx.State())
	}
	if tx.ResultBalance().Amount() != "100.00" {
		t.Errorf("result %v", tx.ResultBalance())
	}
	// A second Process on the terminal transaction is refused.
	if _, err := Process(ProcessInput{Wallet: w, Tx: tx, Now: t0, CorrelationID: "c"}); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("reprocess: %v", err)
	}
}

func TestProcessIsAtomicOnError(t *testing.T) {
	w := newWallet(t, "100.00")
	tx := newTx(t, KindBet, "10.00", "")
	wb, tb := w.State(), tx.State()
	// Invalid correlation id makes event construction fail after the wallet work.
	if _, err := Process(ProcessInput{Wallet: w, Tx: tx, Now: t0, CorrelationID: "", NewID: seqIDs()}); err == nil {
		t.Fatal("expected error")
	}
	if w.State() != wb || tx.State() != tb {
		t.Error("state changed despite error")
	}
	for name, in := range map[string]ProcessInput{
		"nil wallet":      {Tx: tx, Now: t0, CorrelationID: "c"},
		"nil tx":          {Wallet: w, Now: t0, CorrelationID: "c"},
		"no time":         {Wallet: w, Tx: tx, CorrelationID: "c"},
		"bad policy":      {Wallet: w, Tx: tx, Now: t0, CorrelationID: "c", Retry: RetryPolicy{BaseDelay: -1}},
		"stray reference": {Wallet: w, Tx: tx, Now: t0, CorrelationID: "c", Reference: betRef(t, KindBet, "1", StatusProcessed)},
	} {
		if _, err := Process(in); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	opening, _ := NewOpeningTransaction(id(1), id(100), "player-1", brl(t, "1.00"), t0)
	if _, err := Process(ProcessInput{Wallet: w, Tx: opening, Now: t0, CorrelationID: "c"}); err == nil {
		t.Error("OPENING processed as external")
	}
}

func TestProcessEventPayloadsAndOrder(t *testing.T) {
	w := newWallet(t, "100.00")
	tx := newTx(t, KindBet, "30.00", "")
	res := run(t, w, tx, nil, t0.Add(time.Second))
	processed := res.Events[0].(Envelope[WagerTransactionProcessedData])
	changed := res.Events[1].(Envelope[WalletBalanceChangedData])
	if processed.Data.ResultBalance.Amount() != "70.00" || processed.Data.Kind != KindBet || processed.Data.ProviderID != "prov" {
		t.Errorf("processed %+v", processed.Data)
	}
	d := changed.Data
	if d.WalletID != w.ID() || d.TransactionID != tx.ID() || d.Direction != DirectionDebit || d.Money.Amount() != "30.00" ||
		d.BalanceBefore.Amount() != "100.00" || d.BalanceAfter.Amount() != "70.00" || d.WalletVersion != 2 {
		t.Errorf("changed %+v", d)
	}
	for _, e := range res.Events {
		m := e.Meta()
		if m.AggregateID != w.ID() || m.CorrelationID != "corr-1" || m.CausationID != tx.ID().String() || m.Version != EventVersion || m.EventID.IsNil() {
			t.Errorf("meta %+v", m)
		}
	}
	if res.Events[0].Meta().EventID == res.Events[1].Meta().EventID {
		t.Error("event ids must be distinct")
	}
	if _, err := json.Marshal(res.Events[0]); err != nil {
		t.Fatal(err)
	}
}
