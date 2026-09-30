package domain

import (
	"errors"
	"testing"
)

func open(t testing.TB, amount string) (OpenWalletResult, error) {
	return OpenWallet(OpenWalletInput{PlayerID: "p1", InitialBalance: brl(t, amount), CorrelationID: "corr", Now: t0, NewID: seqIDs()})
}

func TestOpenWalletPositiveBalance(t *testing.T) {
	res, err := open(t, "100.00")
	if err != nil {
		t.Fatal(err)
	}
	w, o, l := res.Wallet, res.Opening, res.Ledger
	if w == nil || o == nil || l == nil || len(res.Events) != 2 {
		t.Fatalf("incomplete result %+v", res)
	}
	if w.Version() != 1 || w.Balance().Amount() != "100.00" {
		t.Errorf("wallet %+v", w.State())
	}
	if o.Kind() != KindOpening || o.Status() != StatusProcessed || o.Origin() != OriginInternal || o.WalletID() != w.ID() || o.ResultBalance().Amount() != "100.00" {
		t.Errorf("opening %+v", o.State())
	}
	if l.Direction() != DirectionCredit || l.BalanceBefore().Amount() != "0.00" || l.BalanceAfter().Amount() != "100.00" || l.TransactionID() != o.ID() || l.WalletID() != w.ID() {
		t.Errorf("ledger %+v", l.State())
	}
	if !sameTypes([]EventType{res.Events[0].Meta().EventType, res.Events[1].Meta().EventType},
		[]EventType{EventWagerTransactionProcessed, EventWalletBalanceChanged}) {
		t.Errorf("events %v", res.Events)
	}
	ch := res.Events[1].(Envelope[WalletBalanceChangedData]).Data
	if ch.WalletVersion != 1 || ch.Direction != DirectionCredit || ch.BalanceBefore.Amount() != "0.00" {
		t.Errorf("balance event %+v", ch)
	}
	pr := res.Events[0].(Envelope[WagerTransactionProcessedData]).Data
	if pr.Kind != KindOpening || pr.ProviderID != "" || pr.ExternalTransactionID != "" {
		t.Errorf("processed event %+v", pr)
	}
	// Balance is reconstructible from the ledger alone.
	if sum, _ := CalculateBalance("BRL", []*LedgerEntry{l}); sum != w.Balance() {
		t.Errorf("ledger sum %v != balance %v", sum, w.Balance())
	}
}

func TestOpenWalletZeroBalanceCreatesOnlyWallet(t *testing.T) {
	res, err := open(t, "0.00")
	if err != nil {
		t.Fatal(err)
	}
	if res.Wallet == nil || res.Wallet.Version() != 1 || !res.Wallet.Balance().IsZero() {
		t.Fatalf("wallet %+v", res.Wallet)
	}
	if res.Opening != nil || res.Ledger != nil || len(res.Events) != 0 {
		t.Errorf("zero opening produced records (REQ-027): %+v", res)
	}
}

func TestOpenWalletRejectsInvalidInput(t *testing.T) {
	neg, _ := NewMoney(-1, "BRL")
	for name, in := range map[string]OpenWalletInput{
		"negative":       {PlayerID: "p", InitialBalance: neg, CorrelationID: "c", Now: t0},
		"uninitialized":  {PlayerID: "p", CorrelationID: "c", Now: t0},
		"no player":      {InitialBalance: brl(t, "1"), CorrelationID: "c", Now: t0},
		"no correlation": {PlayerID: "p", InitialBalance: brl(t, "1"), Now: t0},
		"no time":        {PlayerID: "p", InitialBalance: brl(t, "1"), CorrelationID: "c"},
	} {
		if _, err := OpenWallet(in); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if _, err := OpenWallet(OpenWalletInput{InitialBalance: neg}); !errors.Is(err, ErrInvalidAmount) {
		t.Errorf("negative balance: %v", err)
	}
	// Zero opening does not need a correlation id (no events are built).
	if _, err := OpenWallet(OpenWalletInput{PlayerID: "p", InitialBalance: brl(t, "0"), Now: t0}); err != nil {
		t.Errorf("zero opening: %v", err)
	}
}

func TestVersionOnlyIncreasesOnPostCreationBalanceChange(t *testing.T) {
	res, _ := open(t, "50.00")
	w := res.Wallet
	mk := func(kind Kind, amount string, n uint64) *WagerTransaction {
		p := params(kind, amount, "")
		p.ID, p.WalletID, p.PlayerID = id(300+n), w.ID(), "p1"
		tx, err := NewExternalTransaction(p)
		if err != nil {
			t.Fatal(err)
		}
		return tx
	}
	loss := mk(KindLoss, "0.00", 1)
	if r := run(t, w, loss, nil, t0); r.Outcome != OutcomeProcessed || w.Version() != 1 {
		t.Fatalf("LOSS: %s version %d", r.Outcome, w.Version())
	}
	if r := run(t, w, mk(KindBet, "10.00", 2), nil, t0); r.Outcome != OutcomeProcessed || w.Version() != 2 {
		t.Fatalf("debit: %s version %d", r.Outcome, w.Version())
	}
	if r := run(t, w, mk(KindBet, "1000.00", 3), nil, t0); r.Outcome != OutcomeRejected || w.Version() != 2 {
		t.Fatalf("rejection: %s version %d", r.Outcome, w.Version())
	}
}
