package domain

import (
	"errors"
	"fmt"
)

// ErrorKind classifies domain errors so callers can map them to a durable
// outcome without inspecting messages.
type ErrorKind string

const (
	// ErrKindInvalid marks input or invariant violations. The caller supplied
	// something the domain refuses to represent; nothing is persisted.
	ErrKindInvalid ErrorKind = "invalid"
	// ErrKindRejected marks a definitive business rejection. It carries a stable
	// FailureCode and is durably recorded (REQ-022, REQ-045).
	ErrKindRejected ErrorKind = "rejected"
	// ErrKindConflict marks a replay whose content differs from the stored one
	// (idempotency key or inbox message reuse).
	ErrKindConflict ErrorKind = "conflict"
	// ErrKindTransient marks an infrastructure error that may succeed on retry.
	// It never finalizes an operation.
	ErrKindTransient ErrorKind = "transient"
	// ErrKindPermanent marks an audited permanent infrastructure failure; the
	// operation ends in FAILED.
	ErrKindPermanent ErrorKind = "permanent"
)

// Error is the typed domain error. Two *Error values match under errors.Is
// when Kind and Code are equal, regardless of message or cause.
type Error struct {
	Kind    ErrorKind
	Code    string
	Message string
	Cause   error
}

func (e *Error) Error() string {
	msg := e.Code
	if e.Message != "" {
		msg += ": " + e.Message
	}
	if e.Cause != nil {
		msg += ": " + e.Cause.Error()
	}
	return msg
}

func (e *Error) Unwrap() error { return e.Cause }

// Is implements errors.Is matching on Kind and Code.
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	if !ok {
		return false
	}
	return e.Kind == t.Kind && e.Code == t.Code
}

// Codes for ErrKindInvalid, ErrKindConflict and ErrKindPermanent errors.
const (
	CodeInvalidAmount      = "INVALID_AMOUNT"
	CodeInvalidCurrency    = "INVALID_CURRENCY"
	CodeUninitializedMoney = "UNINITIALIZED_MONEY"
	CodeCurrencyMismatch   = "CURRENCY_MISMATCH"
	CodeAmountOverflow     = "AMOUNT_OVERFLOW"
	CodeInvalidIdentifier  = "INVALID_IDENTIFIER"
	CodeInvalidOperation   = "INVALID_OPERATION"
	CodeOpeningNotAllowed  = "OPENING_NOT_ALLOWED"
	CodeInvalidTransition  = "INVALID_TRANSITION"
	CodeInvariantViolation = "INVARIANT_VIOLATION"
	CodeInsufficientFunds  = "INSUFFICIENT_BALANCE"
	CodeInvalidEvent       = "INVALID_EVENT"
	CodeHashMismatch       = "HASH_MISMATCH"
	CodeInboxHashConflict  = "INBOX_HASH_CONFLICT"
	CodePermanentFailure   = "PERMANENT_FAILURE"
	CodeTransientFailure   = "TRANSIENT_FAILURE"
)

// Sentinels for errors.Is checks. Returned errors wrap these with context.
var (
	ErrInvalidAmount      = &Error{Kind: ErrKindInvalid, Code: CodeInvalidAmount}
	ErrInvalidCurrency    = &Error{Kind: ErrKindInvalid, Code: CodeInvalidCurrency}
	ErrUninitializedMoney = &Error{Kind: ErrKindInvalid, Code: CodeUninitializedMoney}
	ErrCurrencyMismatch   = &Error{Kind: ErrKindInvalid, Code: CodeCurrencyMismatch}
	ErrAmountOverflow     = &Error{Kind: ErrKindInvalid, Code: CodeAmountOverflow}
	ErrInvalidIdentifier  = &Error{Kind: ErrKindInvalid, Code: CodeInvalidIdentifier}
	ErrInvalidOperation   = &Error{Kind: ErrKindInvalid, Code: CodeInvalidOperation}
	ErrOpeningNotAllowed  = &Error{Kind: ErrKindInvalid, Code: CodeOpeningNotAllowed}
	ErrInvalidTransition  = &Error{Kind: ErrKindInvalid, Code: CodeInvalidTransition}
	ErrInvariantViolation = &Error{Kind: ErrKindInvalid, Code: CodeInvariantViolation}
	ErrInsufficientFunds  = &Error{Kind: ErrKindInvalid, Code: CodeInsufficientFunds}
	ErrInvalidEvent       = &Error{Kind: ErrKindInvalid, Code: CodeInvalidEvent}
	ErrHashMismatch       = &Error{Kind: ErrKindConflict, Code: CodeHashMismatch}
	ErrInboxHashConflict  = &Error{Kind: ErrKindConflict, Code: CodeInboxHashConflict}
)

func newInvalid(code, format string, args ...any) error {
	return &Error{Kind: ErrKindInvalid, Code: code, Message: fmt.Sprintf(format, args...)}
}

// FailureCode is a stable business rejection or permanent failure code
// persisted on a WagerTransaction and exposed to clients.
type FailureCode string

const (
	FailureInsufficientFundsBet      FailureCode = "INSUFFICIENT_FUNDS_BET"
	FailureInsufficientFundsReversal FailureCode = "INSUFFICIENT_FUNDS_REVERSAL"
	FailureReferenceNotFound         FailureCode = "REFERENCE_NOT_FOUND"
	FailureReferenceUnresolved       FailureCode = "REFERENCE_UNRESOLVED"
	FailureReferenceFailed           FailureCode = "REFERENCE_FAILED"
	FailureReferenceMismatch         FailureCode = "REFERENCE_MISMATCH"
	FailureAlreadyReversed           FailureCode = "ALREADY_REVERSED"
	FailureWalletMismatch            FailureCode = "WALLET_MISMATCH"
	FailureCurrencyMismatch          FailureCode = "CURRENCY_MISMATCH"
	// FailurePermanent is the code of a FAILED transaction (audited permanent
	// infrastructure failure).
	FailurePermanent FailureCode = "PERMANENT_FAILURE"
)

var rejectionCodes = map[FailureCode]bool{
	FailureInsufficientFundsBet:      true,
	FailureInsufficientFundsReversal: true,
	FailureReferenceNotFound:         true,
	FailureReferenceUnresolved:       true,
	FailureReferenceFailed:           true,
	FailureReferenceMismatch:         true,
	FailureAlreadyReversed:           true,
	FailureWalletMismatch:            true,
	FailureCurrencyMismatch:          true,
}

// IsRejection reports whether c is a business rejection code (status REJECTED).
func (c FailureCode) IsRejection() bool { return rejectionCodes[c] }

// String returns the stable wire value.
func (c FailureCode) String() string { return string(c) }

// Rejection builds a ErrKindRejected error carrying code.
func Rejection(code FailureCode, format string, args ...any) error {
	return &Error{Kind: ErrKindRejected, Code: string(code), Message: fmt.Sprintf(format, args...)}
}

// FailureCodeOf returns the failure code of a rejection or permanent failure
// error in err's chain.
func FailureCodeOf(err error) (FailureCode, bool) {
	var de *Error
	if errors.As(err, &de) && (de.Kind == ErrKindRejected || de.Kind == ErrKindPermanent) {
		return FailureCode(de.Code), true
	}
	return "", false
}

// Transient wraps an infrastructure error that may succeed on retry. It must
// not be converted into a business rejection.
func Transient(err error) error {
	if err == nil {
		return nil
	}
	return &Error{Kind: ErrKindTransient, Code: CodeTransientFailure, Cause: err}
}

// Permanent wraps an infrastructure error that will never succeed; the
// operation ends FAILED with FailurePermanent.
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return &Error{Kind: ErrKindPermanent, Code: string(FailurePermanent), Cause: err}
}

func kindOf(err error) (ErrorKind, bool) {
	var de *Error
	if errors.As(err, &de) {
		return de.Kind, true
	}
	return "", false
}

// IsTransient reports whether err is a retryable infrastructure error.
func IsTransient(err error) bool { k, ok := kindOf(err); return ok && k == ErrKindTransient }

// IsPermanent reports whether err is a permanent infrastructure failure.
func IsPermanent(err error) bool { k, ok := kindOf(err); return ok && k == ErrKindPermanent }

// IsRejection reports whether err is a definitive business rejection.
func IsRejection(err error) bool { k, ok := kindOf(err); return ok && k == ErrKindRejected }

// IsConflict reports whether err is an idempotency/inbox content conflict.
func IsConflict(err error) bool { k, ok := kindOf(err); return ok && k == ErrKindConflict }

// IsInvalid reports whether err is an input or invariant violation.
func IsInvalid(err error) bool { k, ok := kindOf(err); return ok && k == ErrKindInvalid }
