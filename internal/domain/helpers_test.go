package domain

import (
	"encoding/binary"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

func brl(t testing.TB, amount string) Money {
	t.Helper()
	m, err := ParseMoney(amount, "BRL")
	if err != nil {
		t.Fatalf("ParseMoney(%q): %v", amount, err)
	}
	return m
}

// seqIDs returns a deterministic generator: 1, 2, 3, ...
func seqIDs() IDGenerator {
	var n uint64
	return func() UUID {
		n++
		var u UUID
		binary.BigEndian.PutUint64(u[8:], n)
		return u
	}
}

func id(n uint64) UUID {
	var u UUID
	binary.BigEndian.PutUint64(u[8:], n)
	return u
}

var hash64 = strings.Repeat("ab", 32)

func newWallet(t testing.TB, balance string) *Wallet {
	t.Helper()
	w, err := RehydrateWallet(WalletState{ID: id(100), PlayerID: "player-1", Balance: brl(t, balance), Version: 1, CreatedAt: t0, UpdatedAt: t0})
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func params(kind Kind, amount, ref string) ExternalTransactionParams {
	return ExternalTransactionParams{
		ID: id(200), ProviderID: "prov", ExternalTransactionID: "ext-1", IdempotencyKey: "key-1", RequestHash: hash64,
		Kind: kind, PlayerID: "player-1", WalletID: id(100), RoundID: "round-1", GameID: "game-1",
		Money: mustParse(amount), ReferenceExternalTransactionID: ref, Now: t0,
	}
}

func mustParse(amount string) Money {
	m, err := ParseMoney(amount, "BRL")
	if err != nil {
		panic(err)
	}
	return m
}

func newTx(t testing.TB, kind Kind, amount, ref string) *WagerTransaction {
	t.Helper()
	tx, err := NewExternalTransaction(params(kind, amount, ref))
	if err != nil {
		t.Fatalf("NewExternalTransaction(%s): %v", kind, err)
	}
	return tx
}

func wantCode(t testing.TB, err error, code string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error %s, got nil", code)
	}
	de, ok := err.(*Error)
	if !ok {
		t.Fatalf("expected *Error %s, got %T: %v", code, err, err)
	}
	if de.Code != code {
		t.Fatalf("expected code %s, got %s (%v)", code, de.Code, err)
	}
}
