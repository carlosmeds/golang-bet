package domain

// ReversalState tells which successful reversals already exist for a
// referenced transaction (REQ-040, D07). The persistence layer derives it, under
// the wallet lock, from PROCESSED REFUND and ROLLBACK transactions that
// reference it.
type ReversalState struct {
	Refunded   bool
	RolledBack bool
}

// Reference is a snapshot of the transaction found by
// (providerId, referenceExternalTransactionId). A nil *Reference passed to
// EvaluateReference means the lookup found nothing.
type Reference struct {
	ExternalTransactionID string
	Kind                  Kind
	Status                Status
	ProviderID            string
	PlayerID              string
	WalletID              UUID
	RoundID               string
	Money                 Money
	Reversals             ReversalState
}

// ReferenceOf snapshots a stored transaction for reference evaluation.
func ReferenceOf(t *WagerTransaction, reversals ReversalState) Reference {
	s := t.s
	return Reference{
		ExternalTransactionID: s.ExternalTransactionID, Kind: s.Kind, Status: s.Status,
		ProviderID: s.ProviderID, PlayerID: s.PlayerID, WalletID: s.WalletID,
		RoundID: s.RoundID, Money: s.Money, Reversals: reversals,
	}
}

// ReferenceAction is the result class of a reference evaluation.
type ReferenceAction int

const (
	// ReferenceProceed: the reference is valid; apply the financial effect.
	ReferenceProceed ReferenceAction = iota + 1
	// ReferenceWait: the reference is missing or not yet final; keep the
	// transaction in PENDING_REFERENCE until it resolves or the retry policy
	// is exhausted, then reject with ExpiryCode.
	ReferenceWait
	// ReferenceReject: definitive business rejection with Code.
	ReferenceReject
)

// ReferenceDecision is the outcome of EvaluateReference.
type ReferenceDecision struct {
	Action ReferenceAction
	// Code is the rejection code when Action is ReferenceReject.
	Code FailureCode
	// ExpiryCode is the rejection code to use if waiting is exhausted:
	// REFERENCE_NOT_FOUND for a missing reference, REFERENCE_UNRESOLVED for
	// one that exists but is still PENDING/PENDING_REFERENCE.
	ExpiryCode FailureCode
	// Direction is the ledger direction of the effect when Action is
	// ReferenceProceed (REFUND credit; ROLLBACK credit of a BET, debit of a
	// WIN or REFUND; WIN credit).
	Direction Direction
}

func reject(code FailureCode) ReferenceDecision {
	return ReferenceDecision{Action: ReferenceReject, Code: code}
}

// EvaluateReference applies the reference rules for t (REQ-035, REQ-037,
// REQ-038, D07, D08, D09) and is pure. It returns Proceed immediately when t
// has no reference. Rules, in order, for a found reference:
//
//  1. identity: provider, player, round -> REFERENCE_MISMATCH; wallet ->
//     WALLET_MISMATCH; currency -> CURRENCY_MISMATCH. These are definitive even
//     while the reference is still pending.
//  2. kind: WIN and REFUND reference a BET; ROLLBACK references a BET, WIN or
//     REFUND; anything else -> REFERENCE_MISMATCH.
//  3. amount: REFUND and ROLLBACK must equal the referenced amount exactly ->
//     REFERENCE_MISMATCH.
//  4. status: PENDING/PENDING_REFERENCE -> wait; REJECTED/FAILED ->
//     REFERENCE_FAILED; only PROCESSED proceeds.
//  5. reversals: a BET accepts at most one successful REFUND or ROLLBACK in
//     total; a WIN or REFUND accepts at most one ROLLBACK -> ALREADY_REVERSED.
func EvaluateReference(t *WagerTransaction, ref *Reference) ReferenceDecision {
	if !t.HasReference() {
		return ReferenceDecision{Action: ReferenceProceed, Direction: kindRules[t.s.Kind].Direction}
	}
	if ref == nil {
		return ReferenceDecision{Action: ReferenceWait, ExpiryCode: FailureReferenceNotFound}
	}
	s := t.s
	switch {
	case ref.ProviderID != s.ProviderID, ref.PlayerID != s.PlayerID, ref.RoundID != s.RoundID:
		return reject(FailureReferenceMismatch)
	case ref.WalletID != s.WalletID:
		return reject(FailureWalletMismatch)
	case ref.Money.currency != s.Money.currency:
		return reject(FailureCurrencyMismatch)
	}

	dir := DirectionCredit
	switch s.Kind {
	case KindWin, KindRefund:
		if ref.Kind != KindBet {
			return reject(FailureReferenceMismatch)
		}
	case KindRollback:
		switch ref.Kind {
		case KindBet:
		case KindWin, KindRefund:
			dir = DirectionDebit
		default:
			return reject(FailureReferenceMismatch)
		}
	default:
		return reject(FailureReferenceMismatch)
	}
	if (s.Kind == KindRefund || s.Kind == KindRollback) && ref.Money != s.Money {
		return reject(FailureReferenceMismatch)
	}

	switch ref.Status {
	case StatusPending, StatusPendingReference:
		return ReferenceDecision{Action: ReferenceWait, ExpiryCode: FailureReferenceUnresolved}
	case StatusProcessed:
	default:
		return reject(FailureReferenceFailed)
	}

	if s.Kind == KindWin {
		return ReferenceDecision{Action: ReferenceProceed, Direction: dir}
	}
	switch ref.Kind {
	case KindBet:
		if ref.Reversals.Refunded || ref.Reversals.RolledBack {
			return reject(FailureAlreadyReversed)
		}
	default: // WIN or REFUND, reversed only by ROLLBACK
		if ref.Reversals.RolledBack {
			return reject(FailureAlreadyReversed)
		}
	}
	return ReferenceDecision{Action: ReferenceProceed, Direction: dir}
}
