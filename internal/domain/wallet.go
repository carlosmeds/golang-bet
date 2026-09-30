package domain

import "time"

// Wallet is the balance aggregate of one (player, currency). Its balance is
// never negative; its version starts at 1 and increases only when a balance
// change happens after creation (REQ-014, REQ-015, REQ-017, REQ-018). Fields
// are private: state changes only through Credit and Debit, and every
// persisted change needs a matching LedgerEntry in the same SQL commit.
type Wallet struct {
	id        UUID
	playerID  string
	balance   Money
	version   int64
	createdAt time.Time
	updatedAt time.Time
}

// WalletState is the persisted shape used for rehydration and snapshots.
type WalletState struct {
	ID        UUID
	PlayerID  string
	Balance   Money
	Version   int64
	CreatedAt time.Time
	UpdatedAt time.Time
}

// NewWallet creates a wallet with a zero balance and version 1. A positive
// initial balance goes through OpenWallet, which also produces the OPENING
// transaction, ledger entry and events.
func NewWallet(id UUID, playerID string, currency Currency, now time.Time) (*Wallet, error) {
	zero, err := Zero(currency)
	if err != nil {
		return nil, err
	}
	return newWalletWithBalance(id, playerID, zero, now)
}

func newWalletWithBalance(id UUID, playerID string, balance Money, now time.Time) (*Wallet, error) {
	w := &Wallet{id: id, playerID: playerID, balance: balance, version: 1, createdAt: now.UTC(), updatedAt: now.UTC()}
	if err := w.validate(); err != nil {
		return nil, err
	}
	return w, nil
}

// RehydrateWallet rebuilds a wallet from stored state, enforcing invariants.
func RehydrateWallet(s WalletState) (*Wallet, error) {
	w := &Wallet{id: s.ID, playerID: s.PlayerID, balance: s.Balance, version: s.Version,
		createdAt: s.CreatedAt.UTC(), updatedAt: s.UpdatedAt.UTC()}
	if err := w.validate(); err != nil {
		return nil, err
	}
	return w, nil
}

func (w *Wallet) validate() error {
	if err := validateUUID("wallet id", w.id); err != nil {
		return err
	}
	if err := validateText("player id", w.playerID); err != nil {
		return err
	}
	if err := w.balance.check(); err != nil {
		return err
	}
	if w.balance.IsNegative() {
		return newInvalid(CodeInvariantViolation, "wallet balance cannot be negative")
	}
	if w.version < 1 {
		return newInvalid(CodeInvariantViolation, "wallet version must be at least 1")
	}
	if w.createdAt.IsZero() || w.updatedAt.IsZero() || w.updatedAt.Before(w.createdAt) {
		return newInvalid(CodeInvariantViolation, "wallet timestamps are missing or out of order")
	}
	return nil
}

func (w *Wallet) ID() UUID             { return w.id }
func (w *Wallet) PlayerID() string     { return w.playerID }
func (w *Wallet) Currency() Currency   { return w.balance.currency }
func (w *Wallet) Balance() Money       { return w.balance }
func (w *Wallet) Version() int64       { return w.version }
func (w *Wallet) CreatedAt() time.Time { return w.createdAt }
func (w *Wallet) UpdatedAt() time.Time { return w.updatedAt }

// State returns an immutable snapshot for persistence.
func (w *Wallet) State() WalletState {
	return WalletState{ID: w.id, PlayerID: w.playerID, Balance: w.balance, Version: w.version,
		CreatedAt: w.createdAt, UpdatedAt: w.updatedAt}
}

// Movement describes one applied balance change; it is the input of
// NewLedgerEntry and of the WalletBalanceChanged event.
type Movement struct {
	Direction Direction
	Amount    Money
	Before    Money
	After     Money
	// Version is the wallet version after the change.
	Version int64
}

// CanDebit reports whether the balance covers amount.
func (w *Wallet) CanDebit(amount Money) bool {
	if err := sameCurrency(w.balance, amount); err != nil || !amount.IsPositive() {
		return false
	}
	c, _ := w.balance.Cmp(amount)
	return c >= 0
}

func (w *Wallet) checkMovement(amount Money, now time.Time) error {
	if err := sameCurrency(w.balance, amount); err != nil {
		return err
	}
	if !amount.IsPositive() {
		return newInvalid(CodeInvalidAmount, "wallet movement requires an amount greater than zero")
	}
	if now.IsZero() {
		return newInvalid(CodeInvariantViolation, "movement time is required")
	}
	return nil
}

// Credit adds amount to the balance and bumps the version.
func (w *Wallet) Credit(amount Money, now time.Time) (Movement, error) {
	if err := w.checkMovement(amount, now); err != nil {
		return Movement{}, err
	}
	after, err := w.balance.Add(amount)
	if err != nil {
		return Movement{}, err
	}
	return w.apply(DirectionCredit, amount, after, now), nil
}

// Debit subtracts amount, failing with ErrInsufficientFunds when the balance
// would become negative. The wallet is left unchanged on error.
func (w *Wallet) Debit(amount Money, now time.Time) (Movement, error) {
	if err := w.checkMovement(amount, now); err != nil {
		return Movement{}, err
	}
	after, err := w.balance.Sub(amount)
	if err != nil {
		return Movement{}, err
	}
	if after.IsNegative() {
		return Movement{}, newInvalid(CodeInsufficientFunds, "balance %s cannot cover %s", w.balance, amount)
	}
	return w.apply(DirectionDebit, amount, after, now), nil
}

func (w *Wallet) apply(dir Direction, amount, after Money, now time.Time) Movement {
	mv := Movement{Direction: dir, Amount: amount, Before: w.balance, After: after, Version: w.version + 1}
	w.balance = after
	w.version++
	if t := now.UTC(); t.After(w.updatedAt) {
		w.updatedAt = t
	}
	return mv
}
