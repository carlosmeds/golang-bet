package domain

import "time"

// OpenWalletInput describes a wallet opening. InitialBalance carries the
// currency and the (non-negative) opening amount.
type OpenWalletInput struct {
	PlayerID       string
	InitialBalance Money
	CorrelationID  string
	Now            time.Time
	NewID          IDGenerator
}

// OpenWalletResult is everything one opening commit must persist atomically.
// For a zero balance only Wallet is set (REQ-027).
type OpenWalletResult struct {
	Wallet  *Wallet
	Opening *WagerTransaction // PROCESSED OPENING, nil for a zero balance
	Ledger  *LedgerEntry      // CREDIT from 0, nil for a zero balance
	Events  []Event           // WagerTransactionProcessed then WalletBalanceChanged
}

// OpenWallet creates a wallet at version 1. A zero balance yields just the
// wallet; a positive balance also yields the PROCESSED internal OPENING
// transaction, its credit ledger entry and the processed and balance-changed
// events (REQ-018, REQ-026, REQ-027, D16). A negative balance is rejected.
func OpenWallet(in OpenWalletInput) (OpenWalletResult, error) {
	if err := in.InitialBalance.check(); err != nil {
		return OpenWalletResult{}, err
	}
	if in.InitialBalance.IsNegative() {
		return OpenWalletResult{}, newInvalid(CodeInvalidAmount, "initial balance cannot be negative")
	}
	wallet, err := newWalletWithBalance(in.NewID.next(), in.PlayerID, in.InitialBalance, in.Now)
	if err != nil {
		return OpenWalletResult{}, err
	}
	res := OpenWalletResult{Wallet: wallet}
	if in.InitialBalance.IsZero() {
		return res, nil
	}

	opening, err := NewOpeningTransaction(in.NewID.next(), wallet.id, in.PlayerID, in.InitialBalance, in.Now)
	if err != nil {
		return OpenWalletResult{}, err
	}
	zero, _ := Zero(in.InitialBalance.currency)
	mv := Movement{Direction: DirectionCredit, Amount: in.InitialBalance, Before: zero, After: in.InitialBalance, Version: 1}
	ledger, err := NewLedgerEntry(in.NewID.next(), wallet.id, opening.s.ID, mv, in.Now)
	if err != nil {
		return OpenWalletResult{}, err
	}
	ctx := func() EventContext {
		return EventContext{EventID: in.NewID.next(), CorrelationID: in.CorrelationID,
			CausationID: opening.s.ID.String(), OccurredAt: in.Now}
	}
	processed, err := NewWagerTransactionProcessed(ctx(), opening)
	if err != nil {
		return OpenWalletResult{}, err
	}
	changed, err := NewWalletBalanceChanged(ctx(), wallet.id, opening.s.ID, mv)
	if err != nil {
		return OpenWalletResult{}, err
	}
	res.Opening, res.Ledger, res.Events = opening, ledger, []Event{processed, changed}
	return res, nil
}
