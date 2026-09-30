package domain

import "time"

// Origin tells where a transaction came from. OPENING is INTERNAL; the five
// provider kinds are EXTERNAL (REQ-024, REQ-025).
type Origin string

const (
	OriginInternal Origin = "INTERNAL"
	OriginExternal Origin = "EXTERNAL"
)

// WagerTransaction records one financial operation and its outcome
// (REQ-020). Status moves only along the REQ-021 state machine; terminal
// states are final. Fields are private; use the State snapshot to persist.
type WagerTransaction struct {
	s WagerTransactionState
}

// WagerTransactionState is the persisted shape used for rehydration and
// snapshots. Zero Money, zero times and empty strings mean "absent".
type WagerTransactionState struct {
	ID     UUID
	Origin Origin
	Kind   Kind
	Status Status

	// External identity (empty for OPENING).
	ProviderID                     string
	ExternalTransactionID          string
	IdempotencyKey                 string
	RequestHash                    string // lowercase hex SHA-256 of the canonical business payload
	RoundID                        string
	GameID                         string
	ReferenceExternalTransactionID string

	PlayerID string
	WalletID UUID
	Money    Money

	// Outcome.
	FailureCode   FailureCode
	ResultBalance Money // balance observed when processed (or when rejected, if known)

	// Pending-reference bookkeeping.
	AttemptCount  int
	NextAttemptAt time.Time

	CreatedAt   time.Time
	UpdatedAt   time.Time
	CompletedAt time.Time
}

// ExternalTransactionParams are the business fields of a provider request.
type ExternalTransactionParams struct {
	ID                             UUID
	ProviderID                     string
	ExternalTransactionID          string
	IdempotencyKey                 string
	RequestHash                    string
	Kind                           Kind
	PlayerID                       string
	WalletID                       UUID
	RoundID                        string
	GameID                         string
	Money                          Money
	ReferenceExternalTransactionID string
	Now                            time.Time
}

// NewExternalTransaction creates a PENDING transaction for one of the five
// external kinds, applying the per-kind amount and reference rules. OPENING is
// rejected with ErrOpeningNotAllowed.
func NewExternalTransaction(p ExternalTransactionParams) (*WagerTransaction, error) {
	if p.Kind == KindOpening {
		return nil, newInvalid(CodeOpeningNotAllowed, "OPENING is internal and cannot be created from external input")
	}
	now := p.Now.UTC()
	return buildTransaction(WagerTransactionState{
		ID: p.ID, Origin: OriginExternal, Kind: p.Kind, Status: StatusPending,
		ProviderID: p.ProviderID, ExternalTransactionID: p.ExternalTransactionID,
		IdempotencyKey: p.IdempotencyKey, RequestHash: p.RequestHash,
		RoundID: p.RoundID, GameID: p.GameID, ReferenceExternalTransactionID: p.ReferenceExternalTransactionID,
		PlayerID: p.PlayerID, WalletID: p.WalletID, Money: p.Money,
		CreatedAt: now, UpdatedAt: now,
	})
}

// NewOpeningTransaction creates the PROCESSED, internal OPENING transaction of
// a wallet opened with a positive balance. Its result balance is the credited
// amount. Provider identity, round, game, key, hash and reference are absent.
func NewOpeningTransaction(id, walletID UUID, playerID string, amount Money, now time.Time) (*WagerTransaction, error) {
	t := now.UTC()
	return buildTransaction(WagerTransactionState{
		ID: id, Origin: OriginInternal, Kind: KindOpening, Status: StatusProcessed,
		PlayerID: playerID, WalletID: walletID, Money: amount, ResultBalance: amount,
		CreatedAt: t, UpdatedAt: t, CompletedAt: t,
	})
}

// RehydrateWagerTransaction rebuilds a transaction from stored state,
// enforcing every invariant a constructor would.
func RehydrateWagerTransaction(s WagerTransactionState) (*WagerTransaction, error) {
	s.CreatedAt, s.UpdatedAt = s.CreatedAt.UTC(), s.UpdatedAt.UTC()
	if !s.NextAttemptAt.IsZero() {
		s.NextAttemptAt = s.NextAttemptAt.UTC()
	}
	if !s.CompletedAt.IsZero() {
		s.CompletedAt = s.CompletedAt.UTC()
	}
	return buildTransaction(s)
}

func buildTransaction(s WagerTransactionState) (*WagerTransaction, error) {
	if err := validateTransaction(&s); err != nil {
		return nil, err
	}
	return &WagerTransaction{s: s}, nil
}

func validateTransaction(s *WagerTransactionState) error {
	if err := validateUUID("transaction id", s.ID); err != nil {
		return err
	}
	rules, ok := RulesFor(s.Kind)
	if !ok {
		return newInvalid(CodeInvalidOperation, "unknown transaction kind %q", string(s.Kind))
	}
	if !s.Status.valid() {
		return newInvalid(CodeInvariantViolation, "unknown transaction status %q", string(s.Status))
	}
	wantOrigin := OriginInternal
	if rules.External {
		wantOrigin = OriginExternal
	}
	if s.Origin != wantOrigin {
		return newInvalid(CodeInvariantViolation, "%s transactions must have origin %s", s.Kind, wantOrigin)
	}
	if err := validateText("player id", s.PlayerID); err != nil {
		return err
	}
	if err := validateUUID("wallet id", s.WalletID); err != nil {
		return err
	}
	if err := rules.Amount.check(s.Kind, s.Money); err != nil {
		return err
	}

	if rules.External {
		for _, f := range []struct{ name, v string }{
			{"provider id", s.ProviderID}, {"external transaction id", s.ExternalTransactionID},
			{"idempotency key", s.IdempotencyKey}, {"round id", s.RoundID}, {"game id", s.GameID},
		} {
			if err := validateText(f.name, f.v); err != nil {
				return err
			}
		}
		if err := validateHash("request hash", s.RequestHash); err != nil {
			return err
		}
	} else if s.ProviderID != "" || s.ExternalTransactionID != "" || s.IdempotencyKey != "" ||
		s.RequestHash != "" || s.RoundID != "" || s.GameID != "" {
		return newInvalid(CodeInvariantViolation, "%s transactions carry no external identity", s.Kind)
	}

	switch rules.Reference {
	case ReferenceForbidden:
		if s.ReferenceExternalTransactionID != "" {
			return newInvalid(CodeInvalidOperation, "%s does not accept a reference", s.Kind)
		}
	case ReferenceRequired:
		if s.ReferenceExternalTransactionID == "" {
			return newInvalid(CodeInvalidOperation, "%s requires referenceExternalTransactionId", s.Kind)
		}
		fallthrough
	case ReferenceOptional:
		if s.ReferenceExternalTransactionID != "" {
			if err := validateText("reference external transaction id", s.ReferenceExternalTransactionID); err != nil {
				return err
			}
		}
	}

	return validateOutcome(s)
}

func validateOutcome(s *WagerTransactionState) error {
	bad := func(format string, args ...any) error { return newInvalid(CodeInvariantViolation, format, args...) }
	if s.CreatedAt.IsZero() || s.UpdatedAt.Before(s.CreatedAt) {
		return bad("transaction timestamps are missing or out of order")
	}
	if s.AttemptCount < 0 {
		return bad("attempt count cannot be negative")
	}
	if s.ResultBalance.IsInitialized() {
		if s.ResultBalance.currency != s.Money.currency {
			return bad("result balance currency differs from the amount currency")
		}
		if s.ResultBalance.IsNegative() {
			return bad("result balance cannot be negative")
		}
	}
	terminal := s.Status.IsTerminal()
	if terminal != !s.CompletedAt.IsZero() {
		return bad("completion time must be set exactly for terminal statuses")
	}
	if !s.CompletedAt.IsZero() && s.CompletedAt.Before(s.CreatedAt) {
		return bad("completion time precedes creation")
	}
	if terminal && !s.NextAttemptAt.IsZero() {
		return bad("terminal transactions have no next attempt")
	}

	switch s.Status {
	case StatusPending:
		if s.FailureCode != "" || s.ResultBalance.IsInitialized() || s.AttemptCount != 0 || !s.NextAttemptAt.IsZero() {
			return bad("PENDING carries no outcome or retry state")
		}
	case StatusPendingReference:
		if s.ReferenceExternalTransactionID == "" {
			return bad("PENDING_REFERENCE requires a reference")
		}
		if s.FailureCode != "" || s.ResultBalance.IsInitialized() {
			return bad("PENDING_REFERENCE carries no outcome")
		}
		if s.AttemptCount < 1 || s.NextAttemptAt.IsZero() {
			return bad("PENDING_REFERENCE requires an attempt count and next attempt time")
		}
	case StatusProcessed:
		if s.FailureCode != "" {
			return bad("PROCESSED carries no failure code")
		}
		if !s.ResultBalance.IsInitialized() {
			return bad("PROCESSED requires the observed result balance")
		}
	case StatusRejected:
		if !s.FailureCode.IsRejection() {
			return bad("REJECTED requires a business rejection code, got %q", string(s.FailureCode))
		}
	case StatusFailed:
		if s.FailureCode != FailurePermanent {
			return bad("FAILED requires failure code %s", FailurePermanent)
		}
		if s.ResultBalance.IsInitialized() {
			return bad("FAILED carries no result balance")
		}
	}
	if s.Kind == KindOpening && s.Status != StatusProcessed {
		return bad("OPENING is always PROCESSED")
	}
	return nil
}

// State returns a copy for persistence; mutating it does not affect t.
func (t *WagerTransaction) State() WagerTransactionState { return t.s }

func (t *WagerTransaction) ID() UUID                 { return t.s.ID }
func (t *WagerTransaction) Kind() Kind               { return t.s.Kind }
func (t *WagerTransaction) Status() Status           { return t.s.Status }
func (t *WagerTransaction) Origin() Origin           { return t.s.Origin }
func (t *WagerTransaction) WalletID() UUID           { return t.s.WalletID }
func (t *WagerTransaction) PlayerID() string         { return t.s.PlayerID }
func (t *WagerTransaction) Money() Money             { return t.s.Money }
func (t *WagerTransaction) ProviderID() string       { return t.s.ProviderID }
func (t *WagerTransaction) RoundID() string          { return t.s.RoundID }
func (t *WagerTransaction) GameID() string           { return t.s.GameID }
func (t *WagerTransaction) RequestHash() string      { return t.s.RequestHash }
func (t *WagerTransaction) IdempotencyKey() string   { return t.s.IdempotencyKey }
func (t *WagerTransaction) FailureCode() FailureCode { return t.s.FailureCode }
func (t *WagerTransaction) ResultBalance() Money     { return t.s.ResultBalance }
func (t *WagerTransaction) AttemptCount() int        { return t.s.AttemptCount }
func (t *WagerTransaction) NextAttemptAt() time.Time { return t.s.NextAttemptAt }
func (t *WagerTransaction) CreatedAt() time.Time     { return t.s.CreatedAt }
func (t *WagerTransaction) UpdatedAt() time.Time     { return t.s.UpdatedAt }
func (t *WagerTransaction) CompletedAt() time.Time   { return t.s.CompletedAt }

func (t *WagerTransaction) ExternalTransactionID() string { return t.s.ExternalTransactionID }

// ReferenceExternalTransactionID is empty when the operation has no reference.
func (t *WagerTransaction) ReferenceExternalTransactionID() string {
	return t.s.ReferenceExternalTransactionID
}

// HasReference reports whether the operation must resolve a reference.
func (t *WagerTransaction) HasReference() bool { return t.s.ReferenceExternalTransactionID != "" }

// MatchesHash reports whether hash equals the stored request hash. A false
// result on replay of the same key is an idempotency conflict
// (ErrHashMismatch).
func (t *WagerTransaction) MatchesHash(hash string) bool { return t.s.RequestHash == hash }

// CheckReplay returns ErrHashMismatch when hash differs from the stored one.
func (t *WagerTransaction) CheckReplay(hash string) error {
	if t.s.RequestHash != hash {
		return &Error{Kind: ErrKindConflict, Code: CodeHashMismatch, Message: "idempotency key reused with different content"}
	}
	return nil
}

// change validates and applies a transition atomically: t is untouched when
// the candidate state is invalid.
func (t *WagerTransaction) change(next Status, now time.Time, mutate func(*WagerTransactionState)) error {
	if !t.s.Status.CanTransitionTo(next) && !(t.s.Status == next && next == StatusPendingReference) {
		return newInvalid(CodeInvalidTransition, "%s cannot move from %s to %s", t.s.Kind, t.s.Status, next)
	}
	cand := t.s
	cand.Status = next
	if now.UTC().After(cand.UpdatedAt) {
		cand.UpdatedAt = now.UTC()
	}
	mutate(&cand)
	if err := validateTransaction(&cand); err != nil {
		return err
	}
	t.s = cand
	return nil
}

func (t *WagerTransaction) complete(s *WagerTransactionState, now time.Time) {
	s.CompletedAt = s.UpdatedAt
	if n := now.UTC(); n.After(s.CompletedAt) {
		s.CompletedAt = n
	}
	s.NextAttemptAt = time.Time{}
}

// MarkProcessed moves PENDING or PENDING_REFERENCE to PROCESSED, recording
// the wallet balance observed after the operation (replays return it).
func (t *WagerTransaction) MarkProcessed(resultBalance Money, now time.Time) error {
	return t.change(StatusProcessed, now, func(s *WagerTransactionState) {
		s.ResultBalance = resultBalance
		t.complete(s, now)
	})
}

// MarkRejected moves to REJECTED with a business rejection code. observed is
// the wallet balance at rejection time; pass the zero Money when unknown.
func (t *WagerTransaction) MarkRejected(code FailureCode, observed Money, now time.Time) error {
	return t.change(StatusRejected, now, func(s *WagerTransactionState) {
		s.FailureCode = code
		s.ResultBalance = observed
		t.complete(s, now)
	})
}

// MarkFailed moves to FAILED for an audited permanent infrastructure failure.
func (t *WagerTransaction) MarkFailed(now time.Time) error {
	return t.change(StatusFailed, now, func(s *WagerTransactionState) {
		s.FailureCode = FailurePermanent
		t.complete(s, now)
	})
}

// MarkPendingReference moves PENDING to PENDING_REFERENCE after the first
// unresolved lookup: attempt count 1 and the first backoff.
func (t *WagerTransaction) MarkPendingReference(now time.Time, policy RetryPolicy) error {
	if t.s.Status != StatusPending {
		return newInvalid(CodeInvalidTransition, "%s cannot move from %s to %s", t.s.Kind, t.s.Status, StatusPendingReference)
	}
	if err := policy.Validate(); err != nil {
		return err
	}
	return t.change(StatusPendingReference, now, func(s *WagerTransactionState) {
		s.AttemptCount = 1
		s.NextAttemptAt = now.UTC().Add(policy.Backoff(1))
	})
}

// RecordReferenceAttempt records one more unresolved lookup while staying in
// PENDING_REFERENCE and schedules the next one with exponential backoff.
func (t *WagerTransaction) RecordReferenceAttempt(now time.Time, policy RetryPolicy) error {
	if t.s.Status != StatusPendingReference {
		return newInvalid(CodeInvalidTransition, "reference attempts apply only to PENDING_REFERENCE, not %s", t.s.Status)
	}
	if err := policy.Validate(); err != nil {
		return err
	}
	return t.change(StatusPendingReference, now, func(s *WagerTransactionState) {
		s.AttemptCount++
		s.NextAttemptAt = now.UTC().Add(policy.Backoff(s.AttemptCount))
	})
}
