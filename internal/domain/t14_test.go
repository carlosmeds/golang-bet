package domain

import (
	"testing"
	"time"
)

func TestT14GettersAndReferenceOf(t *testing.T) {
	now := time.Now().UTC()
	uid := NewUUID()
    m100, _ := NewMoney(100, "BRL")
    m10, _ := NewMoney(10, "BRL")
    m110, _ := NewMoney(110, "BRL")

	w, _ := RehydrateWallet(WalletState{
        ID: uid, PlayerID: "p", Balance: m100, Version: 1, CreatedAt: now, UpdatedAt: now,
    })

	if w.PlayerID() != "p" {
		t.Errorf("expected player ID p")
	}

	txParams := ExternalTransactionParams{
		ID: uid, ProviderID: "prov", ExternalTransactionID: "ext",
		IdempotencyKey: "key", RequestHash: "0000000000000000000000000000000000000000000000000000000000000000",
		Kind: KindRefund, PlayerID: "p", WalletID: uid,
		RoundID: "round", GameID: "game",
		Money: m10,
		ReferenceExternalTransactionID: "ref", Now: now,
	}
	tx, err := NewExternalTransaction(txParams)
	if err != nil { t.Fatal(err) }
	if tx.PlayerID() != "p" { t.Error("bad PlayerID") }
	if tx.ProviderID() != "prov" { t.Error("bad ProviderID") }
	if tx.ExternalTransactionID() != "ext" { t.Error("bad ext ID") }
	if tx.IdempotencyKey() != "key" { t.Error("bad key") }
	if tx.RequestHash() != "0000000000000000000000000000000000000000000000000000000000000000" { t.Error("bad hash") }
	if tx.RoundID() != "round" { t.Error("bad round") }
	if tx.GameID() != "game" { t.Error("bad game") }
	if tx.Money().Minor() != 10 { t.Error("bad money") }
	if tx.ReferenceExternalTransactionID() != "ref" { t.Error("bad ref ID") }
	if tx.CreatedAt() != now { t.Error("bad created") }
	if tx.UpdatedAt() != now { t.Error("bad updated") }
	
	refState := ReversalState{Refunded: true, RolledBack: false}
	ref := ReferenceOf(tx, refState)
	if ref.ExternalTransactionID != "ext" { t.Error("bad ref") }
	if ref.ProviderID != "prov" { t.Error("bad ref") }
	if ref.PlayerID != "p" { t.Error("bad ref") }
	if ref.WalletID != uid { t.Error("bad ref") }
	if ref.RoundID != "round" { t.Error("bad ref") }
	if ref.Money.Minor() != 10 { t.Error("bad ref") }
	if ref.Status != StatusPending { t.Error("bad ref status") }
	if ref.Kind != KindRefund { t.Error("bad ref kind") }
	if ref.Reversals.Refunded != true { t.Error("bad ref state") }

    le, err := RehydrateLedgerEntry(LedgerEntryState{
        ID: uid, WalletID: uid, TransactionID: uid,
        Direction: DirectionCredit, Amount: m10,
        BalanceBefore: m100, BalanceAfter: m110,
        CreatedAt: now,
    })
    if err != nil { t.Fatal(err) }
	if le.ID() != uid { t.Error("bad le id") }
	if le.Amount().Minor() != 10 { t.Error("bad le amount") }
	if le.CreatedAt() != now { t.Error("bad le time") }

    bal, err := CalculateBalance("BRL", []*LedgerEntry{le})
    if err != nil || bal.Minor() != 10 { t.Error("bad calc") }
}

func TestT14InboxAndOutboxGetters(t *testing.T) {
    now := time.Now().UTC()
    in, err := NewInboxMessage("cons", "msg", "0000000000000000000000000000000000000000000000000000000000000000", now)
    if err != nil { t.Fatal(err) }
    if in.ConsumerName() != "cons" { t.Error("bad cons") }
    if in.MessageID() != "msg" { t.Error("bad msg") }
    if in.RequestHash() != "0000000000000000000000000000000000000000000000000000000000000000" { t.Error("bad hash") }
    if in.ReceivedAt() != now { t.Error("bad recv") }
    
    in.Complete(now)
    if in.CompletedAt().IsZero() { t.Error("bad complete") }

    uid := NewUUID()
    m10, _ := NewMoney(10, "BRL")
    ctx := EventContext{EventID: uid, CausationID: uid.String(), CorrelationID: "corr", OccurredAt: now}
    mv := Movement{Direction: DirectionCredit, Amount: m10, Before: m10, After: m10, Version: 1}
    ev, err := NewWalletBalanceChanged(ctx, uid, uid, mv)
    if err != nil { t.Fatal(err) }
    out, err := NewOutboxEvent(ev, now)
    if err != nil { t.Fatal(err) }

    if out.OccurredAt() != now { t.Error("bad occur") }
    if !out.PublishedAt().IsZero() { t.Error("bad pub") }
}

func TestT14Kinds(t *testing.T) {
    if !KindBet.IsExternal() { t.Error("bet should be external") }
    if KindOpening.IsExternal() { t.Error("opening should not be external") }
    
    m10, _ := NewMoney(10, "BRL")
    if ValidateAmount(KindBet, m10) != nil { t.Error("bet amt valid") }
}
