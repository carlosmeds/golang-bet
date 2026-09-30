package domain

// Kind is the type of a wager transaction. OPENING is internal only; BET, WIN,
// LOSS, REFUND and ROLLBACK are the five external kinds (REQ-024).
type Kind string

const (
	KindOpening  Kind = "OPENING"
	KindBet      Kind = "BET"
	KindWin      Kind = "WIN"
	KindLoss     Kind = "LOSS"
	KindRefund   Kind = "REFUND"
	KindRollback Kind = "ROLLBACK"
)

// ExternalKinds lists the kinds accepted over HTTP/SQS.
var ExternalKinds = []Kind{KindBet, KindWin, KindLoss, KindRefund, KindRollback}

// ParseKind parses a wire value. It is case-sensitive and accepts every kind,
// including OPENING; use ParseExternalKind for transport input.
func ParseKind(s string) (Kind, error) {
	switch k := Kind(s); k {
	case KindOpening, KindBet, KindWin, KindLoss, KindRefund, KindRollback:
		return k, nil
	}
	return "", newInvalid(CodeInvalidOperation, "unknown transaction kind %q", s)
}

// ParseExternalKind parses transport input and rejects OPENING.
func ParseExternalKind(s string) (Kind, error) {
	k, err := ParseKind(s)
	if err != nil {
		return "", err
	}
	if k == KindOpening {
		return "", newInvalid(CodeOpeningNotAllowed, "OPENING is internal and cannot be submitted externally")
	}
	return k, nil
}

// IsExternal reports whether k may come from a provider.
func (k Kind) IsExternal() bool { return k != KindOpening && k.valid() }

func (k Kind) valid() bool {
	_, err := ParseKind(string(k))
	return err == nil
}

func (k Kind) String() string { return string(k) }

// Direction is the ledger direction of a balance movement.
type Direction string

const (
	DirectionCredit Direction = "CREDIT"
	DirectionDebit  Direction = "DEBIT"
)

func (d Direction) valid() bool { return d == DirectionCredit || d == DirectionDebit }

// Status is the lifecycle state of a WagerTransaction (REQ-021).
type Status string

const (
	StatusPending          Status = "PENDING"
	StatusPendingReference Status = "PENDING_REFERENCE"
	StatusProcessed        Status = "PROCESSED"
	StatusRejected         Status = "REJECTED"
	StatusFailed           Status = "FAILED"
)

func (s Status) valid() bool {
	switch s {
	case StatusPending, StatusPendingReference, StatusProcessed, StatusRejected, StatusFailed:
		return true
	}
	return false
}

// IsTerminal reports whether no further transition is allowed.
func (s Status) IsTerminal() bool {
	return s == StatusProcessed || s == StatusRejected || s == StatusFailed
}

var transitions = map[Status][]Status{
	StatusPending:          {StatusProcessed, StatusPendingReference, StatusRejected, StatusFailed},
	StatusPendingReference: {StatusProcessed, StatusRejected, StatusFailed},
}

// CanTransitionTo reports whether the state machine allows s -> next.
func (s Status) CanTransitionTo(next Status) bool {
	for _, n := range transitions[s] {
		if n == next {
			return true
		}
	}
	return false
}

// AmountRule constrains the amount of a transaction kind.
type AmountRule int

const (
	// AmountPositive requires amount > 0.
	AmountPositive AmountRule = iota + 1
	// AmountZero requires amount == 0.
	AmountZero
)

// ReferenceRule states whether referenceExternalTransactionId applies.
type ReferenceRule int

const (
	// ReferenceForbidden: the field must be absent.
	ReferenceForbidden ReferenceRule = iota + 1
	// ReferenceOptional: may be present; when present it must resolve.
	ReferenceOptional
	// ReferenceRequired: must be present and resolve to a processed transaction.
	ReferenceRequired
)

// Rules is the explicit per-kind rule set (REQ-034..REQ-038, REQ-025).
type Rules struct {
	External  bool
	Amount    AmountRule
	Reference ReferenceRule
	// Ledger reports whether a successful operation writes a ledger entry
	// and changes the wallet balance/version. LOSS does not (REQ-030).
	Ledger bool
	// Direction is the fixed ledger direction, or "" when it depends on the
	// referenced transaction (ROLLBACK) or no ledger entry exists (LOSS).
	Direction Direction
	// InsufficientFundsCode is the rejection code for a debit that the
	// balance cannot cover, or "" when the kind never debits.
	InsufficientFundsCode FailureCode
}

var kindRules = map[Kind]Rules{
	KindOpening:  {External: false, Amount: AmountPositive, Reference: ReferenceForbidden, Ledger: true, Direction: DirectionCredit},
	KindBet:      {External: true, Amount: AmountPositive, Reference: ReferenceForbidden, Ledger: true, Direction: DirectionDebit, InsufficientFundsCode: FailureInsufficientFundsBet},
	KindWin:      {External: true, Amount: AmountPositive, Reference: ReferenceOptional, Ledger: true, Direction: DirectionCredit},
	KindLoss:     {External: true, Amount: AmountZero, Reference: ReferenceForbidden, Ledger: false},
	KindRefund:   {External: true, Amount: AmountPositive, Reference: ReferenceRequired, Ledger: true, Direction: DirectionCredit},
	KindRollback: {External: true, Amount: AmountPositive, Reference: ReferenceRequired, Ledger: true, InsufficientFundsCode: FailureInsufficientFundsReversal},
}

// RulesFor returns the rule set of k; ok is false for unknown kinds.
func RulesFor(k Kind) (Rules, bool) {
	r, ok := kindRules[k]
	return r, ok
}

func (r AmountRule) check(k Kind, m Money) error {
	if err := m.check(); err != nil {
		return err
	}
	switch r {
	case AmountPositive:
		if !m.IsPositive() {
			return newInvalid(CodeInvalidAmount, "%s requires an amount greater than zero", k)
		}
	case AmountZero:
		if !m.IsZero() {
			return newInvalid(CodeInvalidAmount, "%s requires an amount of zero", k)
		}
	}
	return nil
}

// ValidateAmount applies the kind's amount rule (also rejects negatives and
// uninitialized values).
func ValidateAmount(k Kind, m Money) error {
	r, ok := kindRules[k]
	if !ok {
		return newInvalid(CodeInvalidOperation, "unknown transaction kind %q", string(k))
	}
	return r.Amount.check(k, m)
}
