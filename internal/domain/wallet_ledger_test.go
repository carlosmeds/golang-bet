package domain

import (
	"errors"
	"testing"
	"time"
)

func TestNewWalletStartsAtVersionOneWithZeroBalance(t *testing.T) {
	w, err := NewWallet(id(1), "p1", "BRL", t0)
	if err != nil {
		t.Fatal(err)
	}
	if w.Version() != 1 || !w.Balance().IsZero() || w.Currency() != "BRL" || w.CreatedAt() != t0 {
		t.Fatalf("unexpected wallet %+v", w.State())
	}
}

func TestWalletConstructorsValidate(t *testing.T) {
	if _, err := NewWallet(UUID{}, "p1", "BRL", t0); err == nil {
		t.Error("nil wallet id accepted")
	}
	if _, err := NewWallet(id(1), "", "BRL", t0); err == nil {
		t.Error("empty player accepted")
	}
	if _, err := NewWallet(id(1), " p1", "BRL", t0); err == nil {
		t.Error("padded player accepted")
	}
	if _, err := NewWallet(id(1), "p1", "brl", t0); err == nil {
		t.Error("bad currency accepted")
	}
	if _, err := NewWallet(id(1), "p1", "BRL", time.Time{}); err == nil {
		t.Error("zero time accepted")
	}
}

func TestRehydrateWalletEnforcesInvariants(t *testing.T) {
	good := WalletState{ID: id(1), PlayerID: "p", Balance: brl(t, "5.00"), Version: 3, CreatedAt: t0, UpdatedAt: t0.Add(time.Hour)}
	w, err := RehydrateWallet(good)
	if err != nil || w.State() != good {
		t.Fatalf("rehydrate: %v %+v", err, w)
	}
	neg, _ := NewMoney(-1, "BRL")
	bads := map[string]WalletState{}
	add := func(name string, mut func(*WalletState)) {
		s := good
		mut(&s)
		bads[name] = s
	}
	add("negative balance", func(s *WalletState) { s.Balance = neg })
	add("uninitialized balance", func(s *WalletState) { s.Balance = Money{} })
	add("version zero", func(s *WalletState) { s.Version = 0 })
	add("nil id", func(s *WalletState) { s.ID = UUID{} })
	add("updated before created", func(s *WalletState) { s.UpdatedAt = t0.Add(-time.Second) })
	add("no player", func(s *WalletState) { s.PlayerID = "" })
	for name, s := range bads {
		if _, err := RehydrateWallet(s); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestWalletCreditDebitVersionAndBalance(t *testing.T) {
	w := newWallet(t, "100.00")
	mv, err := w.Debit(brl(t, "30.00"), t0.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if mv.Direction != DirectionDebit || mv.Before.Amount() != "100.00" || mv.After.Amount() != "70.00" || mv.Version != 2 {
		t.Fatalf("movement %+v", mv)
	}
	if w.Balance().Amount() != "70.00" || w.Version() != 2 || !w.UpdatedAt().Equal(t0.Add(time.Second)) {
		t.Fatalf("wallet %+v", w.State())
	}
	mv, err = w.Credit(brl(t, "5.50"), t0.Add(2*time.Second))
	if err != nil || mv.After.Amount() != "75.50" || mv.Version != 3 || w.Version() != 3 {
		t.Fatalf("credit %+v %v", mv, err)
	}
	// Debit of the exact balance is allowed and leaves zero.
	if _, err := w.Debit(brl(t, "75.50"), t0.Add(3*time.Second)); err != nil || !w.Balance().IsZero() {
		t.Fatalf("exact debit: %v %v", err, w.Balance())
	}
}

func TestWalletRejectsInvalidMovementsWithoutChange(t *testing.T) {
	w := newWallet(t, "10.00")
	before := w.State()
	usd, _ := NewMoney(100, "USD")
	zero, _ := Zero("BRL")
	neg, _ := NewMoney(-100, "BRL")
	if _, err := w.Debit(brl(t, "10.01"), t0); !errors.Is(err, ErrInsufficientFunds) {
		t.Errorf("overdraft: %v", err)
	}
	for name, fn := range map[string]func() error{
		"debit currency":  func() error { _, e := w.Debit(usd, t0); return e },
		"credit currency": func() error { _, e := w.Credit(usd, t0); return e },
		"debit zero":      func() error { _, e := w.Debit(zero, t0); return e },
		"credit zero":     func() error { _, e := w.Credit(zero, t0); return e },
		"credit negative": func() error { _, e := w.Credit(neg, t0); return e },
		"debit negative":  func() error { _, e := w.Debit(neg, t0); return e },
		"uninitialized":   func() error { _, e := w.Credit(Money{}, t0); return e },
		"no time":         func() error { _, e := w.Credit(brl(t, "1"), time.Time{}); return e },
	} {
		if fn() == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if w.State() != before {
		t.Errorf("wallet changed by failed operations: %+v", w.State())
	}
	if w.CanDebit(brl(t, "10.01")) || !w.CanDebit(brl(t, "10.00")) || w.CanDebit(usd) {
		t.Error("CanDebit")
	}
}

func TestWalletCreditOverflowLeavesWalletUnchanged(t *testing.T) {
	max, _ := NewMoney(1<<63-1, "BRL")
	w, err := RehydrateWallet(WalletState{ID: id(1), PlayerID: "p", Balance: max, Version: 1, CreatedAt: t0, UpdatedAt: t0})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Credit(brl(t, "0.01"), t0); !errors.Is(err, ErrAmountOverflow) {
		t.Errorf("overflow: %v", err)
	}
	if w.Version() != 1 {
		t.Error("version changed")
	}
}

func TestLedgerEntryArithmetic(t *testing.T) {
	w := newWallet(t, "10.00")
	mv, _ := w.Credit(brl(t, "2.00"), t0)
	e, err := NewLedgerEntry(id(1), w.ID(), id(2), mv, t0)
	if err != nil {
		t.Fatal(err)
	}
	if e.Direction() != DirectionCredit || e.BalanceAfter().Amount() != "12.00" {
		t.Fatalf("entry %+v", e.State())
	}
	back, err := RehydrateLedgerEntry(e.State())
	if err != nil || back.State() != e.State() {
		t.Fatalf("rehydrate: %v", err)
	}
	if s, _ := e.Signed(); s.Amount() != "2.00" {
		t.Errorf("signed credit %v", s)
	}

	good := e.State()
	bads := map[string]func(*LedgerEntryState){
		"wrong after":       func(s *LedgerEntryState) { s.BalanceAfter = brl(t, "13.00") },
		"wrong direction":   func(s *LedgerEntryState) { s.Direction = DirectionDebit },
		"bad direction":     func(s *LedgerEntryState) { s.Direction = "SIDEWAYS" },
		"zero amount":       func(s *LedgerEntryState) { s.Amount = brl(t, "0") },
		"currency mismatch": func(s *LedgerEntryState) { s.Amount, _ = NewMoney(200, "USD") },
		"nil wallet":        func(s *LedgerEntryState) { s.WalletID = UUID{} },
		"nil transaction":   func(s *LedgerEntryState) { s.TransactionID = UUID{} },
		"nil id":            func(s *LedgerEntryState) { s.ID = UUID{} },
		"no time":           func(s *LedgerEntryState) { s.CreatedAt = time.Time{} },
		"uninitialized":     func(s *LedgerEntryState) { s.BalanceBefore = Money{} },
		"negative balance": func(s *LedgerEntryState) {
			s.BalanceBefore, _ = NewMoney(-200, "BRL")
			s.BalanceAfter = brl(t, "0")
		},
	}
	for name, mut := range bads {
		s := good
		mut(&s)
		if _, err := RehydrateLedgerEntry(s); err == nil {
			t.Errorf("%s accepted", name)
		}
	}

	dmv, _ := w.Debit(brl(t, "4.00"), t0)
	d, err := NewLedgerEntry(id(3), w.ID(), id(4), dmv, t0)
	if err != nil {
		t.Fatal(err)
	}
	if s, _ := d.Signed(); s.Amount() != "-4.00" {
		t.Errorf("signed debit %v", s)
	}
	total, err := CalculateBalance("BRL", []*LedgerEntry{e, d})
	if err != nil || total.Amount() != "-2.00" { // 2.00 - 4.00, entries are not a full history here
		t.Errorf("CalculateBalance = %v, %v", total, err)
	}
}
