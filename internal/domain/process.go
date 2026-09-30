package domain

import "time"

// Outcome is the durable result class of Process.
type Outcome string

const (
	OutcomeProcessed Outcome = "PROCESSED"
	OutcomeRejected  Outcome = "REJECTED"
	// OutcomePending means the transaction is (still) PENDING_REFERENCE.
	OutcomePending Outcome = "PENDING_REFERENCE"
)

// ProcessInput is the state a use case loads, under the wallet lock, to apply
// one external operation. Wallet and Tx are updated in place only when Process
// returns a nil error.
type ProcessInput struct {
	Wallet *Wallet
	// Tx must be PENDING (first attempt) or PENDING_REFERENCE (retry).
	Tx *WagerTransaction
	// Reference is the lookup result for Tx's reference; nil when not found
	// or when Tx has no reference.
	Reference     *Reference
	Now           time.Time
	Retry         RetryPolicy // zero value means DefaultRetryPolicy
	CorrelationID string
	NewID         IDGenerator
}

// ProcessResult lists what the use case must persist in the same SQL commit:
// the updated wallet and transaction (always), the ledger entry when a balance
// moved and the events for the outbox, in emission order.
type ProcessResult struct {
	Outcome  Outcome
	Movement *Movement
	Ledger   *LedgerEntry
	Events   []Event
}

// Process applies the explicit rules of the five external kinds to one
// transaction and wallet. It is pure (no I/O) and atomic: on error nothing
// changes. Business rejections (insufficient funds, reference problems,
// mismatches) are not errors; they produce OutcomeRejected with the
// transaction REJECTED and a WagerTransactionRejected event.
//
//	BET       debit; INSUFFICIENT_FUNDS_BET when the balance is short.
//	WIN       credit; optional BET reference validated.
//	LOSS      no ledger, no balance change, wallet version untouched; the
//	          processed event is still emitted.
//	REFUND    credit of exactly the referenced BET amount.
//	ROLLBACK  reverses a BET (credit) or a WIN/REFUND (debit, with
//	          INSUFFICIENT_FUNDS_REVERSAL when the balance is short).
//
// A missing or unfinished reference keeps the transaction in PENDING_REFERENCE
// (event emitted once, on entry) until the retry policy is exhausted, which
// rejects with REFERENCE_NOT_FOUND or REFERENCE_UNRESOLVED.
func Process(in ProcessInput) (ProcessResult, error) {
	if in.Wallet == nil || in.Tx == nil {
		return ProcessResult{}, newInvalid(CodeInvalidOperation, "wallet and transaction are required")
	}
	if in.Tx.s.Origin != OriginExternal {
		return ProcessResult{}, newInvalid(CodeInvalidOperation, "only external transactions are processed")
	}
	if st := in.Tx.s.Status; st != StatusPending && st != StatusPendingReference {
		return ProcessResult{}, newInvalid(CodeInvalidTransition, "%s transaction is %s and cannot be processed", in.Tx.s.Kind, st)
	}
	if in.Now.IsZero() {
		return ProcessResult{}, newInvalid(CodeInvariantViolation, "processing time is required")
	}
	if in.Reference != nil && (!in.Tx.HasReference() || in.Reference.ExternalTransactionID != in.Tx.s.ReferenceExternalTransactionID) {
		return ProcessResult{}, newInvalid(CodeInvalidOperation, "reference snapshot does not belong to the transaction")
	}
	policy := in.Retry
	if policy == (RetryPolicy{}) {
		policy = DefaultRetryPolicy()
	}
	if err := policy.Validate(); err != nil {
		return ProcessResult{}, err
	}

	w, t := *in.Wallet, *in.Tx // work on copies; commit at the end
	now := in.Now.UTC()
	rules := kindRules[t.s.Kind]
	var res ProcessResult
	ctx := func() EventContext {
		return EventContext{EventID: in.NewID.next(), CorrelationID: in.CorrelationID,
			CausationID: t.s.ID.String(), OccurredAt: now}
	}
	commit := func(o Outcome) (ProcessResult, error) {
		res.Outcome = o
		*in.Wallet, *in.Tx = w, t
		return res, nil
	}
	rejectWith := func(code FailureCode) (ProcessResult, error) {
		observed := w.balance
		if code == FailureWalletMismatch || code == FailureCurrencyMismatch {
			observed = Money{} // the loaded wallet is not the operation's wallet/currency
		}
		if err := t.MarkRejected(code, observed, now); err != nil {
			return ProcessResult{}, err
		}
		ev, err := NewWagerTransactionRejected(ctx(), &t)
		if err != nil {
			return ProcessResult{}, err
		}
		res.Events = append(res.Events, ev)
		return commit(OutcomeRejected)
	}

	if t.s.WalletID != w.id || t.s.PlayerID != w.playerID {
		return rejectWith(FailureWalletMismatch)
	}
	if t.s.Money.currency != w.balance.currency {
		return rejectWith(FailureCurrencyMismatch)
	}

	dir := rules.Direction
	if t.HasReference() {
		d := EvaluateReference(&t, in.Reference)
		switch d.Action {
		case ReferenceReject:
			return rejectWith(d.Code)
		case ReferenceWait:
			if policy.Exhausted(t.s.AttemptCount+1, t.s.CreatedAt, now) {
				return rejectWith(d.ExpiryCode)
			}
			if t.s.Status == StatusPending {
				if err := t.MarkPendingReference(now, policy); err != nil {
					return ProcessResult{}, err
				}
				ev, err := NewWagerTransactionPendingReference(ctx(), &t)
				if err != nil {
					return ProcessResult{}, err
				}
				res.Events = append(res.Events, ev)
			} else if err := t.RecordReferenceAttempt(now, policy); err != nil {
				return ProcessResult{}, err
			}
			return commit(OutcomePending)
		default:
			dir = d.Direction
		}
	}

	if !rules.Ledger { // LOSS: processed without ledger or balance change
		if err := t.MarkProcessed(w.balance, now); err != nil {
			return ProcessResult{}, err
		}
		ev, err := NewWagerTransactionProcessed(ctx(), &t)
		if err != nil {
			return ProcessResult{}, err
		}
		res.Events = append(res.Events, ev)
		return commit(OutcomeProcessed)
	}

	var mv Movement
	var err error
	if dir == DirectionDebit {
		if !w.CanDebit(t.s.Money) {
			return rejectWith(rules.InsufficientFundsCode)
		}
		mv, err = w.Debit(t.s.Money, now)
	} else {
		mv, err = w.Credit(t.s.Money, now)
	}
	if err != nil {
		return ProcessResult{}, err
	}
	entry, err := NewLedgerEntry(in.NewID.next(), w.id, t.s.ID, mv, now)
	if err != nil {
		return ProcessResult{}, err
	}
	if err := t.MarkProcessed(w.balance, now); err != nil {
		return ProcessResult{}, err
	}
	processed, err := NewWagerTransactionProcessed(ctx(), &t)
	if err != nil {
		return ProcessResult{}, err
	}
	changed, err := NewWalletBalanceChanged(ctx(), w.id, t.s.ID, mv)
	if err != nil {
		return ProcessResult{}, err
	}
	res.Movement, res.Ledger, res.Events = &mv, entry, []Event{processed, changed}
	return commit(OutcomeProcessed)
}
