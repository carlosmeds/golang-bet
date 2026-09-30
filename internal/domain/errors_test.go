package domain

import (
	"errors"
	"fmt"
	"testing"
)

func errorsIs(err, target error) bool { return errors.Is(err, target) }

func TestErrorClassification(t *testing.T) {
	cause := errors.New("connection reset")
	tr := Transient(cause)
	pe := Permanent(cause)
	rj := Rejection(FailureAlreadyReversed, "already reversed")
	inv := newInvalid(CodeInvalidAmount, "bad")
	cf := &Error{Kind: ErrKindConflict, Code: CodeHashMismatch}

	if !IsTransient(tr) || IsPermanent(tr) || IsRejection(tr) {
		t.Error("transient")
	}
	if !IsPermanent(pe) || IsTransient(pe) {
		t.Error("permanent")
	}
	if !IsRejection(rj) || IsTransient(rj) || IsInvalid(rj) {
		t.Error("rejection")
	}
	if !IsInvalid(inv) || !IsConflict(cf) || IsConflict(inv) {
		t.Error("invalid/conflict")
	}
	if !errors.Is(tr, cause) || !errors.Is(pe, cause) {
		t.Error("cause not unwrapped")
	}
	wrapped := fmt.Errorf("persist: %w", tr)
	if !IsTransient(wrapped) {
		t.Error("classification lost through wrapping")
	}
	if Transient(nil) != nil || Permanent(nil) != nil {
		t.Error("nil must stay nil")
	}
	if c, ok := FailureCodeOf(rj); !ok || c != FailureAlreadyReversed {
		t.Errorf("FailureCodeOf(rejection) = %q %v", c, ok)
	}
	if c, ok := FailureCodeOf(pe); !ok || c != FailurePermanent {
		t.Errorf("FailureCodeOf(permanent) = %q %v", c, ok)
	}
	if _, ok := FailureCodeOf(tr); ok {
		t.Error("a transient error must not yield a failure code (REQ-022)")
	}
	if _, ok := FailureCodeOf(inv); ok {
		t.Error("an invalid-input error is not a business failure code")
	}
	if !errors.Is(inv, ErrInvalidAmount) || errors.Is(inv, ErrInvalidCurrency) {
		t.Error("errors.Is on sentinels")
	}
	// Same code, different kind, must not match.
	if errors.Is(&Error{Kind: ErrKindRejected, Code: CodeInvalidAmount}, ErrInvalidAmount) {
		t.Error("kind ignored by errors.Is")
	}
}

func TestFailureCodesAreStable(t *testing.T) {
	want := map[FailureCode]string{
		FailureInsufficientFundsBet: "INSUFFICIENT_FUNDS_BET", FailureInsufficientFundsReversal: "INSUFFICIENT_FUNDS_REVERSAL",
		FailureReferenceNotFound: "REFERENCE_NOT_FOUND", FailureReferenceUnresolved: "REFERENCE_UNRESOLVED",
		FailureReferenceFailed: "REFERENCE_FAILED", FailureReferenceMismatch: "REFERENCE_MISMATCH",
		FailureAlreadyReversed: "ALREADY_REVERSED", FailureWalletMismatch: "WALLET_MISMATCH",
		FailureCurrencyMismatch: "CURRENCY_MISMATCH", FailurePermanent: "PERMANENT_FAILURE",
	}
	for c, s := range want {
		if string(c) != s {
			t.Errorf("%s != %s", c, s)
		}
		if c != FailurePermanent && !c.IsRejection() {
			t.Errorf("%s should be a rejection code", c)
		}
	}
	if FailurePermanent.IsRejection() || FailureCode("NOPE").IsRejection() {
		t.Error("IsRejection")
	}
}

func TestUUID(t *testing.T) {
	u := NewUUID()
	if u.IsNil() || u[6]>>4 != 4 || u[8]>>6 != 2 {
		t.Errorf("not a v4 uuid: %s", u)
	}
	back, err := ParseUUID(u.String())
	if err != nil || back != u {
		t.Fatalf("round trip: %v", err)
	}
	up, err := ParseUUID("0F8FAD5B-D9CB-469F-A165-70867728950E")
	if err != nil || up.String() != "0f8fad5b-d9cb-469f-a165-70867728950e" {
		t.Errorf("normalization: %v %s", err, up)
	}
	for _, bad := range []string{"", "0f8fad5bd9cb469fa16570867728950e", "{0f8fad5b-d9cb-469f-a165-70867728950e}", "0f8fad5b-d9cb-469f-a165-70867728950g", "urn:uuid:0f8fad5b-d9cb-469f-a165-70867728950e"} {
		if _, err := ParseUUID(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	var viaText UUID
	if err := viaText.UnmarshalText([]byte(u.String())); err != nil || viaText != u {
		t.Error("UnmarshalText")
	}
	if a, b := NewUUID(), NewUUID(); a == b {
		t.Error("collision")
	}
}

func TestIDGeneratorDefaultsToRandom(t *testing.T) {
	var g IDGenerator
	if g.next().IsNil() {
		t.Error("nil generator produced nil uuid")
	}
}
