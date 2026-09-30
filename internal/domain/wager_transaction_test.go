package domain

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestNewExternalTransactionRulesPerKind(t *testing.T) {
	ok := []struct {
		kind        Kind
		amount, ref string
	}{
		{KindBet, "10.00", ""}, {KindWin, "10.00", ""}, {KindWin, "10.00", "bet-1"},
		{KindLoss, "0.00", ""}, {KindRefund, "10.00", "bet-1"}, {KindRollback, "10.00", "bet-1"},
	}
	for _, c := range ok {
		tx := newTx(t, c.kind, c.amount, c.ref)
		if tx.Status() != StatusPending || tx.Origin() != OriginExternal || tx.Kind() != c.kind {
			t.Errorf("%s: unexpected %+v", c.kind, tx.State())
		}
	}
	bad := []struct {
		name        string
		kind        Kind
		amount, ref string
		code        string
	}{
		{"BET zero", KindBet, "0.00", "", CodeInvalidAmount},
		{"WIN zero", KindWin, "0.00", "", CodeInvalidAmount},
		{"LOSS positive", KindLoss, "1.00", "", CodeInvalidAmount},
		{"REFUND zero", KindRefund, "0.00", "bet-1", CodeInvalidAmount},
		{"ROLLBACK zero", KindRollback, "0.00", "bet-1", CodeInvalidAmount},
		{"REFUND no ref", KindRefund, "1.00", "", CodeInvalidOperation},
		{"ROLLBACK no ref", KindRollback, "1.00", "", CodeInvalidOperation},
		{"BET with ref", KindBet, "1.00", "x", CodeInvalidOperation},
		{"LOSS with ref", KindLoss, "0.00", "x", CodeInvalidOperation},
		{"OPENING external", KindOpening, "1.00", "", CodeOpeningNotAllowed},
		{"unknown kind", "TIP", "1.00", "", CodeInvalidOperation},
	}
	for _, c := range bad {
		_, err := NewExternalTransaction(params(c.kind, c.amount, c.ref))
		if err == nil {
			t.Errorf("%s accepted", c.name)
			continue
		}
		wantCode(t, err, c.code)
	}
	if _, err := ParseExternalKind("OPENING"); !errors.Is(err, ErrOpeningNotAllowed) {
		t.Errorf("ParseExternalKind(OPENING) = %v", err)
	}
	if _, err := ParseExternalKind("bet"); err == nil {
		t.Error("case-insensitive kind accepted")
	}
}

func TestNewExternalTransactionRequiresExternalIdentity(t *testing.T) {
	mut := map[string]func(*ExternalTransactionParams){
		"provider":    func(p *ExternalTransactionParams) { p.ProviderID = "" },
		"external id": func(p *ExternalTransactionParams) { p.ExternalTransactionID = "" },
		"key":         func(p *ExternalTransactionParams) { p.IdempotencyKey = "" },
		"hash":        func(p *ExternalTransactionParams) { p.RequestHash = "xyz" },
		"hash case":   func(p *ExternalTransactionParams) { p.RequestHash = strings.ToUpper(hash64) },
		"round":       func(p *ExternalTransactionParams) { p.RoundID = "" },
		"game":        func(p *ExternalTransactionParams) { p.GameID = "" },
		"player":      func(p *ExternalTransactionParams) { p.PlayerID = "" },
		"wallet":      func(p *ExternalTransactionParams) { p.WalletID = UUID{} },
		"id":          func(p *ExternalTransactionParams) { p.ID = UUID{} },
		"money":       func(p *ExternalTransactionParams) { p.Money = Money{} },
		"long id":     func(p *ExternalTransactionParams) { p.ProviderID = strings.Repeat("a", MaxIdentifierLength+1) },
		"control":     func(p *ExternalTransactionParams) { p.RoundID = "a\nb" },
		"padded":      func(p *ExternalTransactionParams) { p.GameID = "g " },
		"no time":     func(p *ExternalTransactionParams) { p.Now = time.Time{} },
	}
	for name, f := range mut {
		p := params(KindBet, "1.00", "")
		f(&p)
		if _, err := NewExternalTransaction(p); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestOpeningTransactionRules(t *testing.T) {
	tx, err := NewOpeningTransaction(id(1), id(2), "p", brl(t, "50.00"), t0)
	if err != nil {
		t.Fatal(err)
	}
	s := tx.State()
	if s.Origin != OriginInternal || s.Kind != KindOpening || s.Status != StatusProcessed || s.ResultBalance != s.Money ||
		s.ProviderID != "" || s.ExternalTransactionID != "" || s.IdempotencyKey != "" || s.RequestHash != "" ||
		s.RoundID != "" || s.GameID != "" || s.ReferenceExternalTransactionID != "" || s.CompletedAt.IsZero() {
		t.Fatalf("opening state %+v", s)
	}
	if _, err := NewOpeningTransaction(id(1), id(2), "p", brl(t, "0"), t0); err == nil {
		t.Error("zero opening accepted (REQ-027)")
	}
	if _, err := RehydrateWagerTransaction(s); err != nil {
		t.Errorf("rehydrate opening: %v", err)
	}
	// Opening cannot carry external identity or be non-PROCESSED.
	withProvider := s
	withProvider.ProviderID = "prov"
	if _, err := RehydrateWagerTransaction(withProvider); err == nil {
		t.Error("opening with provider accepted")
	}
	external := s
	external.Origin = OriginExternal
	if _, err := RehydrateWagerTransaction(external); err == nil {
		t.Error("opening with external origin accepted")
	}
	pending := s
	pending.Status, pending.CompletedAt, pending.ResultBalance = StatusPending, time.Time{}, Money{}
	if _, err := RehydrateWagerTransaction(pending); err == nil {
		t.Error("PENDING opening accepted")
	}
	for _, status := range []Status{StatusRejected, StatusFailed} {
		if err := tx.MarkRejected(FailureReferenceMismatch, Money{}, t0); !errors.Is(err, ErrInvalidTransition) {
			t.Errorf("opening transition to %s: %v", status, err)
		}
	}
	// A wallet can have an external kind of origin INTERNAL? No.
	bet := newTx(t, KindBet, "1.00", "").State()
	bet.Origin = OriginInternal
	if _, err := RehydrateWagerTransaction(bet); err == nil {
		t.Error("BET with internal origin accepted")
	}
}

func TestStateMachineTable(t *testing.T) {
	all := []Status{StatusPending, StatusPendingReference, StatusProcessed, StatusRejected, StatusFailed}
	allowed := map[[2]Status]bool{
		{StatusPending, StatusProcessed}: true, {StatusPending, StatusPendingReference}: true,
		{StatusPending, StatusRejected}: true, {StatusPending, StatusFailed}: true,
		{StatusPendingReference, StatusProcessed}: true, {StatusPendingReference, StatusRejected}: true,
		{StatusPendingReference, StatusFailed}: true,
	}
	for _, from := range all {
		for _, to := range all {
			if got := from.CanTransitionTo(to); got != allowed[[2]Status{from, to}] {
				t.Errorf("%s -> %s = %v", from, to, got)
			}
		}
	}
	for _, s := range []Status{StatusProcessed, StatusRejected, StatusFailed} {
		if !s.IsTerminal() {
			t.Errorf("%s not terminal", s)
		}
	}
}

func TestTransitionsPendingToTerminal(t *testing.T) {
	now := t0.Add(time.Minute)
	tx := newTx(t, KindBet, "10.00", "")
	if err := tx.MarkProcessed(brl(t, "90.00"), now); err != nil {
		t.Fatal(err)
	}
	if tx.Status() != StatusProcessed || tx.ResultBalance().Amount() != "90.00" || !tx.CompletedAt().Equal(now) || tx.FailureCode() != "" {
		t.Fatalf("%+v", tx.State())
	}
	rej := newTx(t, KindBet, "10.00", "")
	if err := rej.MarkRejected(FailureInsufficientFundsBet, brl(t, "5.00"), now); err != nil {
		t.Fatal(err)
	}
	if rej.Status() != StatusRejected || rej.FailureCode() != FailureInsufficientFundsBet || rej.ResultBalance().Amount() != "5.00" {
		t.Fatalf("%+v", rej.State())
	}
	failed := newTx(t, KindBet, "10.00", "")
	if err := failed.MarkFailed(now); err != nil {
		t.Fatal(err)
	}
	if failed.Status() != StatusFailed || failed.FailureCode() != FailurePermanent {
		t.Fatalf("%+v", failed.State())
	}
}

func TestTerminalStatesAreFinal(t *testing.T) {
	now := t0.Add(time.Minute)
	makers := map[string]func() *WagerTransaction{
		"processed": func() *WagerTransaction {
			x := newTx(t, KindBet, "1.00", "")
			_ = x.MarkProcessed(brl(t, "1"), now)
			return x
		},
		"rejected": func() *WagerTransaction {
			x := newTx(t, KindBet, "1.00", "")
			_ = x.MarkRejected(FailureInsufficientFundsBet, Money{}, now)
			return x
		},
		"failed": func() *WagerTransaction {
			x := newTx(t, KindBet, "1.00", "")
			_ = x.MarkFailed(now)
			return x
		},
	}
	for name, mk := range makers {
		tx := mk()
		before := tx.State()
		errs := []error{
			tx.MarkProcessed(brl(t, "1"), now), tx.MarkRejected(FailureAlreadyReversed, Money{}, now), tx.MarkFailed(now),
			tx.MarkPendingReference(now, DefaultRetryPolicy()), tx.RecordReferenceAttempt(now, DefaultRetryPolicy()),
		}
		for i, err := range errs {
			if !errors.Is(err, ErrInvalidTransition) {
				t.Errorf("%s #%d: %v", name, i, err)
			}
		}
		if tx.State() != before {
			t.Errorf("%s: terminal state mutated", name)
		}
	}
}

func TestInvalidOutcomesAreRefusedAtomically(t *testing.T) {
	tx := newTx(t, KindBet, "1.00", "")
	before := tx.State()
	usd, _ := NewMoney(100, "USD")
	neg, _ := NewMoney(-1, "BRL")
	for name, err := range map[string]error{
		"processed without result":      tx.MarkProcessed(Money{}, t0),
		"processed wrong currency":      tx.MarkProcessed(usd, t0),
		"processed negative result":     tx.MarkProcessed(neg, t0),
		"rejected with failure code":    tx.MarkRejected(FailurePermanent, Money{}, t0),
		"rejected with unknown code":    tx.MarkRejected("WHATEVER", Money{}, t0),
		"rejected empty code":           tx.MarkRejected("", Money{}, t0),
		"pending reference without ref": tx.MarkPendingReference(t0, DefaultRetryPolicy()),
		"pending reference bad policy":  tx.MarkPendingReference(t0, RetryPolicy{}),
	} {
		if err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if tx.State() != before {
		t.Errorf("failed transitions mutated the transaction: %+v", tx.State())
	}
}

func TestPendingReferenceBackoffAndAttempts(t *testing.T) {
	policy := RetryPolicy{BaseDelay: time.Second, MaxDelay: 10 * time.Second, TTL: time.Hour}
	tx := newTx(t, KindRefund, "5.00", "bet-1")
	if err := tx.MarkPendingReference(t0, policy); err != nil {
		t.Fatal(err)
	}
	if tx.Status() != StatusPendingReference || tx.AttemptCount() != 1 || !tx.NextAttemptAt().Equal(t0.Add(time.Second)) {
		t.Fatalf("%+v", tx.State())
	}
	if err := tx.MarkPendingReference(t0, policy); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("PENDING_REFERENCE -> PENDING_REFERENCE via MarkPendingReference: %v", err)
	}
	want := []time.Duration{2, 4, 8, 10, 10}
	now := t0
	for i, w := range want {
		now = now.Add(time.Minute)
		if err := tx.RecordReferenceAttempt(now, policy); err != nil {
			t.Fatal(err)
		}
		if tx.AttemptCount() != i+2 || !tx.NextAttemptAt().Equal(now.Add(w*time.Second)) {
			t.Errorf("attempt %d: count=%d next=%v want +%ds", i+2, tx.AttemptCount(), tx.NextAttemptAt().Sub(now), w)
		}
	}
	// Resolution clears retry bookkeeping.
	if err := tx.MarkProcessed(brl(t, "1"), now); err != nil {
		t.Fatal(err)
	}
	if !tx.NextAttemptAt().IsZero() || tx.AttemptCount() != 6 {
		t.Errorf("after processed: %+v", tx.State())
	}
	pend := newTx(t, KindBet, "1.00", "")
	if err := pend.RecordReferenceAttempt(t0, policy); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("attempt on PENDING: %v", err)
	}
}

func TestRetryPolicy(t *testing.T) {
	p := DefaultRetryPolicy()
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []RetryPolicy{{}, {BaseDelay: time.Second, MaxDelay: time.Millisecond, TTL: time.Hour}, {BaseDelay: time.Second, MaxDelay: time.Second, TTL: 0}, {BaseDelay: time.Second, MaxDelay: time.Second, TTL: 1, MaxAttempts: -1}} {
		if bad.Validate() == nil {
			t.Errorf("%+v accepted", bad)
		}
	}
	// Backoff is capped at 5 minutes and never overflows.
	if p.Backoff(1) != time.Second || p.Backoff(2) != 2*time.Second || p.Backoff(9) != 256*time.Second || p.Backoff(10) != 5*time.Minute {
		t.Errorf("backoff %v %v %v %v", p.Backoff(1), p.Backoff(2), p.Backoff(9), p.Backoff(10))
	}
	if p.Backoff(1_000_000) != 5*time.Minute {
		t.Error("large attempt not capped")
	}
	if p.Exhausted(5, t0, t0.Add(time.Hour)) {
		t.Error("exhausted before TTL")
	}
	if !p.Exhausted(5, t0, t0.Add(24*time.Hour)) {
		t.Error("not exhausted at TTL")
	}
	capped := p
	capped.MaxAttempts = 3
	if capped.Exhausted(2, t0, t0) || !capped.Exhausted(3, t0, t0) {
		t.Error("max attempts")
	}
}

func TestRehydrateWagerTransactionEnforcesInvariants(t *testing.T) {
	processed := newTx(t, KindBet, "1.00", "")
	_ = processed.MarkProcessed(brl(t, "9.00"), t0.Add(time.Second))
	good := processed.State()
	if tx, err := RehydrateWagerTransaction(good); err != nil || tx.State() != good {
		t.Fatalf("rehydrate processed: %v", err)
	}

	refPending := newTx(t, KindRefund, "1.00", "bet-1")
	_ = refPending.MarkPendingReference(t0, DefaultRetryPolicy())
	if _, err := RehydrateWagerTransaction(refPending.State()); err != nil {
		t.Fatalf("rehydrate pending reference: %v", err)
	}

	cases := map[string]func(*WagerTransactionState){
		"processed without completion": func(s *WagerTransactionState) { s.CompletedAt = time.Time{} },
		"processed without result":     func(s *WagerTransactionState) { s.ResultBalance = Money{} },
		"processed with failure":       func(s *WagerTransactionState) { s.FailureCode = FailureAlreadyReversed },
		"processed with next attempt":  func(s *WagerTransactionState) { s.NextAttemptAt = t0 },
		"pending with completion":      func(s *WagerTransactionState) { s.Status = StatusPending },
		"rejected without code":        func(s *WagerTransactionState) { s.Status = StatusRejected; s.ResultBalance = Money{} },
		"failed with rejection code": func(s *WagerTransactionState) {
			s.Status = StatusFailed
			s.FailureCode = FailureAlreadyReversed
			s.ResultBalance = Money{}
		},
		"unknown status":             func(s *WagerTransactionState) { s.Status = "DONE" },
		"unknown kind":               func(s *WagerTransactionState) { s.Kind = "TIP" },
		"completion before creation": func(s *WagerTransactionState) { s.CompletedAt = s.CreatedAt.Add(-time.Second) },
		"updated before created":     func(s *WagerTransactionState) { s.UpdatedAt = s.CreatedAt.Add(-time.Second) },
		"negative attempts":          func(s *WagerTransactionState) { s.AttemptCount = -1 },
		"zero amount bet":            func(s *WagerTransactionState) { s.Money = brl(t, "0") },
		"result currency differs":    func(s *WagerTransactionState) { s.ResultBalance, _ = NewMoney(1, "USD") },
		"negative result":            func(s *WagerTransactionState) { s.ResultBalance, _ = NewMoney(-1, "BRL") },
		"bad hash":                   func(s *WagerTransactionState) { s.RequestHash = "zz" },
	}
	for name, mut := range cases {
		s := good
		mut(&s)
		if _, err := RehydrateWagerTransaction(s); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	prs := refPending.State()
	prs.AttemptCount = 0
	if _, err := RehydrateWagerTransaction(prs); err == nil {
		t.Error("PENDING_REFERENCE with zero attempts accepted")
	}
	prs = refPending.State()
	prs.NextAttemptAt = time.Time{}
	if _, err := RehydrateWagerTransaction(prs); err == nil {
		t.Error("PENDING_REFERENCE without next attempt accepted")
	}
	prs = refPending.State()
	prs.ReferenceExternalTransactionID = ""
	if _, err := RehydrateWagerTransaction(prs); err == nil {
		t.Error("REFUND without reference accepted on rehydrate")
	}
}

func TestReplayHashCheck(t *testing.T) {
	tx := newTx(t, KindBet, "1.00", "")
	if err := tx.CheckReplay(hash64); err != nil {
		t.Errorf("same hash: %v", err)
	}
	err := tx.CheckReplay(strings.Repeat("cd", 32))
	if !errors.Is(err, ErrHashMismatch) || !IsConflict(err) {
		t.Errorf("different hash: %v", err)
	}
	if !tx.MatchesHash(hash64) {
		t.Error("MatchesHash")
	}
}
