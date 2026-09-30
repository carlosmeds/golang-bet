package idempotency

import (
    "testing"
    "wagering/internal/contract"
)

func TestHashInvalidOp(t *testing.T) {
    op := contract.Operation{}
    _, err := Hash(op)
    if err == nil {
        t.Error("expected error for invalid op")
    }
}
