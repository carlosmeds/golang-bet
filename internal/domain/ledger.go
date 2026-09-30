package domain

import "time"

// LedgerEntry is an immutable, append-only record of one balance movement
// (REQ-028). Invariants: positive amount, one currency, non-negative balances,
// and after = before ± amount according to Direction.
type LedgerEntry struct {
	id            UUID
	walletID      UUID
	transactionID UUID
	direction     Direction
	amount        Money
	balanceBefore Money
	balanceAfter  Money
	createdAt     time.Time
}

// LedgerEntryState is the persisted shape used for rehydration and snapshots.
type LedgerEntryState struct {
	ID            UUID
	WalletID      UUID
	TransactionID UUID
	Direction     Direction
	Amount        Money
	BalanceBefore Money
	BalanceAfter  Money
	CreatedAt     time.Time
}

// NewLedgerEntry records a wallet Movement for a transaction.
func NewLedgerEntry(id, walletID, transactionID UUID, mv Movement, now time.Time) (*LedgerEntry, error) {
	return RehydrateLedgerEntry(LedgerEntryState{
		ID: id, WalletID: walletID, TransactionID: transactionID, Direction: mv.Direction,
		Amount: mv.Amount, BalanceBefore: mv.Before, BalanceAfter: mv.After, CreatedAt: now,
	})
}

// RehydrateLedgerEntry rebuilds an entry from stored state, re-validating the
// balance arithmetic.
func RehydrateLedgerEntry(s LedgerEntryState) (*LedgerEntry, error) {
	e := &LedgerEntry{id: s.ID, walletID: s.WalletID, transactionID: s.TransactionID, direction: s.Direction,
		amount: s.Amount, balanceBefore: s.BalanceBefore, balanceAfter: s.BalanceAfter, createdAt: s.CreatedAt.UTC()}
	if err := e.validate(); err != nil {
		return nil, err
	}
	return e, nil
}

func (e *LedgerEntry) validate() error {
	for _, c := range []struct {
		name string
		id   UUID
	}{{"ledger entry id", e.id}, {"ledger wallet id", e.walletID}, {"ledger transaction id", e.transactionID}} {
		if err := validateUUID(c.name, c.id); err != nil {
			return err
		}
	}
	if !e.direction.valid() {
		return newInvalid(CodeInvariantViolation, "ledger direction %q is invalid", string(e.direction))
	}
	if err := sameCurrency(e.amount, e.balanceBefore); err != nil {
		return err
	}
	if err := sameCurrency(e.amount, e.balanceAfter); err != nil {
		return err
	}
	if !e.amount.IsPositive() {
		return newInvalid(CodeInvalidAmount, "ledger amount must be greater than zero")
	}
	if e.balanceBefore.IsNegative() || e.balanceAfter.IsNegative() {
		return newInvalid(CodeInvariantViolation, "ledger balances cannot be negative")
	}
	var want Money
	var err error
	if e.direction == DirectionCredit {
		want, err = e.balanceBefore.Add(e.amount)
	} else {
		want, err = e.balanceBefore.Sub(e.amount)
	}
	if err != nil {
		return err
	}
	if want != e.balanceAfter {
		return newInvalid(CodeInvariantViolation, "ledger balance after %s does not equal %s %s %s",
			e.balanceAfter, e.balanceBefore, map[Direction]string{DirectionCredit: "+", DirectionDebit: "-"}[e.direction], e.amount)
	}
	if e.createdAt.IsZero() {
		return newInvalid(CodeInvariantViolation, "ledger timestamp is required")
	}
	return nil
}

func (e *LedgerEntry) ID() UUID             { return e.id }
func (e *LedgerEntry) WalletID() UUID       { return e.walletID }
func (e *LedgerEntry) TransactionID() UUID  { return e.transactionID }
func (e *LedgerEntry) Direction() Direction { return e.direction }
func (e *LedgerEntry) Amount() Money        { return e.amount }
func (e *LedgerEntry) BalanceBefore() Money { return e.balanceBefore }
func (e *LedgerEntry) BalanceAfter() Money  { return e.balanceAfter }
func (e *LedgerEntry) CreatedAt() time.Time { return e.createdAt }

// State returns an immutable snapshot for persistence.
func (e *LedgerEntry) State() LedgerEntryState {
	return LedgerEntryState{ID: e.id, WalletID: e.walletID, TransactionID: e.transactionID, Direction: e.direction,
		Amount: e.amount, BalanceBefore: e.balanceBefore, BalanceAfter: e.balanceAfter, CreatedAt: e.createdAt}
}

// Signed returns the movement as a signed amount (credit positive, debit negative).
func (e *LedgerEntry) Signed() (Money, error) {
	if e.direction == DirectionDebit {
		return e.amount.Neg()
	}
	return e.amount, nil
}

// CalculateBalance reconstructs a wallet balance from its ledger entries
// (OPENING included) for reconciliation. The result is signed and uses
// overflow-checked arithmetic.
func CalculateBalance(currency Currency, entries []*LedgerEntry) (Money, error) {
	total, err := Zero(currency)
	if err != nil {
		return Money{}, err
	}
	for _, e := range entries {
		s, err := e.Signed()
		if err != nil {
			return Money{}, err
		}
		if total, err = total.Add(s); err != nil {
			return Money{}, err
		}
	}
	return total, nil
}
